// Package service implements the on-ramp business logic.
//
// The two main operations are:
//
//   CreateQuote  — fetches the live BRL/USDT price from Transfero, locks it by
//                  creating a Transfero session, and persists the quote.
//
//   ConfirmOrder — validates the quote is still open, debits BRL from dinacore,
//                  closes the Transfero session (confirming the trade), credits
//                  USDT to dinacore, and persists the order.
//
// Failure handling for ConfirmOrder:
//   If CloseSession fails after BRL is already debited, the service checks
//   GET /v1/closings for an entry matching the quoteId (oid). If found, the
//   trade happened and USDT is credited. If not found, BRL is refunded.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"time"

	"transfero-on-ramp/internal/dinacore"
	"transfero-on-ramp/internal/store"
	"transfero-on-ramp/internal/transfero"
)

// Sentinel errors that handlers inspect to return correct HTTP status codes.
var (
	ErrMarketClosed        = errors.New("outside market hours")
	ErrQuoteExpired        = store.ErrQuoteExpired
	ErrQuoteUsed           = store.ErrQuoteUsed
	ErrNotFound            = store.ErrNotFound
	ErrInsufficientBalance = errors.New("insufficient BRL balance")
)

// vendorRe matches any case-insensitive occurrence of the upstream vendor name.
var vendorRe = regexp.MustCompile(`(?i)transfero`)

// ProviderError is returned when the upstream provider returns a structured
// error. It carries the HTTP status and machine-readable code from the
// provider, with any vendor-identifying strings scrubbed out.
type ProviderError struct {
	Status int
	Code   string
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("provider error %d: %s", e.Status, e.Code)
}

// ─────────────────────────────────────────────────────────────────────────────
// Input / output types
// ─────────────────────────────────────────────────────────────────────────────

// QuoteRequest is the validated input for CreateQuote.
type QuoteRequest struct {
	AccountID  string  // resolved from Bearer token, not supplied by caller
	BRLAmount  float64 // e.g. 25000.00
	Settlement string  // D0 | D1 | D2 (default D0)
	Network    string  // mainnet | shasta (default mainnet)
}

// QuoteResponse is returned to the customer after a price is locked.
type QuoteResponse struct {
	QuoteID    string  `json:"quoteId"`
	USDTAmount float64 `json:"usdtAmount"`
	BRLAmount  float64 `json:"brlAmount"`
	Price      float64 `json:"price"`              // BRL per USDT (after fee markup)
	RawPrice   float64 `json:"rawPrice,omitempty"` // Transfero's original price (omitted when fee=0)
	FeePct     float64 `json:"feePct,omitempty"`   // markup applied, e.g. 0.002 = 0.2%
	Settlement string  `json:"settlement"`
	Network    string  `json:"network"`
	ExpiresAt  string  `json:"expiresAt"` // RFC3339
}

// OrderRequest is the validated input for ConfirmOrder.
type OrderRequest struct {
	AccountID          string  // resolved from Bearer token
	QuoteID            string
	DestinationAddress string  // TRC20 wallet where Transfero delivers USDT
	// RequestedBRL is the original merchant-facing BRL amount (before any spread).
	// When set (ExecuteSettlement path), Dinacore is debited for this amount so that
	// it stays in sync with Dinapay's merchants.balance. When zero (direct API call),
	// Dinacore is debited for quote.BRLAmount (Transfero's closing cost).
	RequestedBRL float64
}

// OrderResponse is returned after a trade is confirmed.
type OrderResponse struct {
	OrderID             string  `json:"orderId"`
	QuoteID             string  `json:"quoteId"`
	ClosingID           string  `json:"closingId"`
	OID                 string  `json:"oid"`
	USDTAmount          float64 `json:"usdtAmount"`
	BRLAmount           float64 `json:"brlAmount"`
	Price               float64 `json:"price"`              // BRL per USDT (after fee markup)
	RawPrice            float64 `json:"rawPrice,omitempty"` // Transfero's original price (omitted when fee=0)
	FeePct              float64 `json:"feePct,omitempty"`   // markup applied, e.g. 0.002 = 0.2%
	Settlement          string  `json:"settlement"`
	DestinationAddress  string  `json:"destinationAddress"`
	Network             string  `json:"network"`
	Status              string  `json:"status"`
	PixPaymentGroupID   string  `json:"pixPaymentGroupId,omitempty"`
	CreatedAt           string  `json:"createdAt"`
}

// OrderListResponse is the payload for the list endpoint.
type OrderListResponse struct {
	Data       []OrderResponse `json:"data"`
	Pagination PaginationMeta  `json:"pagination"`
}

// PaginationMeta matches Transfero's shape for consistency.
type PaginationMeta struct {
	Page       int `json:"page"`
	PageSize   int `json:"pageSize"`
	Total      int `json:"total"`
	TotalPages int `json:"totalPages"`
}

// ─────────────────────────────────────────────────────────────────────────────
// OnRampService
// ─────────────────────────────────────────────────────────────────────────────

// OTCDeskConfig holds the parameters for sending BRL to the Transfero OTC desk.
type OTCDeskConfig struct {
	BaasClient    *transfero.BaasClient
	AccountID     string // source account (collector, e.g. "2133")
	PIXKey        string // OTC desk PIX key
	TaxID         string // Transfero CNPJ (optional)
}

// OnRampService orchestrates quote creation and order confirmation.
type OnRampService struct {
	transfero     *transfero.Client
	dinacore      *dinacore.Client
	quoteStore    *store.QuoteStore
	orderStore    *store.OrderStore
	feeStore      *store.FeeStore
	settingsStore *store.SettingsStore
	otcDesk       *OTCDeskConfig // if set, BRL is sent via PIX after CloseSession
	log           *slog.Logger
}

// NewOnRampService creates an OnRampService with its required dependencies.
func NewOnRampService(
	tc *transfero.Client,
	dc *dinacore.Client,
	qs *store.QuoteStore,
	os *store.OrderStore,
	fs *store.FeeStore,
	ss *store.SettingsStore,
	log *slog.Logger,
) *OnRampService {
	return &OnRampService{
		transfero:     tc,
		dinacore:      dc,
		quoteStore:    qs,
		orderStore:    os,
		feeStore:      fs,
		settingsStore: ss,
		log:           log,
	}
}

// WithOTCDesk attaches the BaaS client and OTC desk config so that ConfirmOrder
// automatically sends the quoted BRL amount via PIX after booking the trade.
func (s *OnRampService) WithOTCDesk(cfg OTCDeskConfig) *OnRampService {
	s.otcDesk = &cfg
	return s
}

// ─────────────────────────────────────────────────────────────────────────────
// CreateQuote
// ─────────────────────────────────────────────────────────────────────────────

// CreateQuote fetches the live price from Transfero, calculates the USDT amount
// the customer receives for their BRL, locks the price by creating a Transfero
// session, and persists the quote record.
//
// The quote is valid for the session TTL configured on Transfero (~7 seconds).
// The customer must call ConfirmOrder before ExpiresAt.
func (s *OnRampService) CreateQuote(ctx context.Context, req QuoteRequest) (QuoteResponse, error) {
	if req.Settlement == "" {
		req.Settlement = "D0"
	}
	if req.Network == "" {
		req.Network = "mainnet"
	}

	// Fetch the live price grid
	grid, err := s.transfero.GetPrices(ctx)
	if err != nil {
		return QuoteResponse{}, s.wrapTransferoErr(err, "get prices")
	}

	usdtPrices, ok := grid.Prices["USDT"]
	if !ok {
		return QuoteResponse{}, fmt.Errorf("USDT prices not available from Transfero")
	}

	var entry transfero.PriceEntry
	switch req.Settlement {
	case "D0":
		entry = usdtPrices.D0
	case "D1":
		entry = usdtPrices.D1
	case "D2":
		entry = usdtPrices.D2
	default:
		return QuoteResponse{}, fmt.Errorf("invalid settlement %q: must be D0, D1 or D2", req.Settlement)
	}
	if entry.Price <= 0 {
		return QuoteResponse{}, fmt.Errorf("invalid price from Transfero: %v", entry.Price)
	}

	// Look up the per-account fee markup for BRL → USDT (0 when no row exists).
	feePct := 0.0
	if s.feeStore != nil && req.AccountID != "" {
		if f, err := s.feeStore.GetFee(ctx, req.AccountID, "BRL", "USDT"); err != nil {
			s.log.Warn("fee lookup failed (proceeding with 0%)", "account", req.AccountID, "err", err)
		} else {
			feePct = f
		}
	}

	// Apply the markup: adjusted_price = raw_price * (1 + fee_pct).
	// The user receives fewer USDT for the same BRL; the spread is our revenue.
	rawPrice := entry.Price
	adjustedPrice := rawPrice * (1 + feePct)

	// BRL ÷ adjusted_price = USDT, rounded to 6 decimal places
	usdtAmount := math.Round((req.BRLAmount/adjustedPrice)*1_000_000) / 1_000_000

	// Check that the account has enough BRL balance before locking the price
	if s.dinacore != nil && req.AccountID != "" {
		balance, err := s.dinacore.GetBalance(ctx, req.AccountID, "BRL")
		if err != nil {
			s.log.Warn("dinacore balance check failed (proceeding)", "account", req.AccountID, "err", err)
		} else if balance < req.BRLAmount {
			return QuoteResponse{}, ErrInsufficientBalance
		}
	}

	// Lock the price by creating a Transfero session (wallet provided at close time)
	sess, err := s.transfero.CreateSession(ctx, usdtAmount, req.Settlement)
	if err != nil {
		return QuoteResponse{}, s.wrapTransferoErr(err, "create session")
	}

	expiresAt, err := time.Parse(time.RFC3339, sess.ExpiresAt)
	if err != nil {
		expiresAt = time.Now().Add(10 * time.Second)
	}

	quoteID, err := s.quoteStore.Insert(ctx, store.Quote{
		AccountID:          req.AccountID,
		TransferoSessionID: sess.SessionID,
		BRLAmount:          sess.TotalBRL,
		USDTAmount:         sess.Amount,
		Price:              adjustedPrice,
		RawPrice:           rawPrice,
		FeePct:             feePct,
		Settlement:         sess.Settlement,
		Network:            req.Network,
		ExpiresAt:          expiresAt,
	})
	if err != nil {
		return QuoteResponse{}, fmt.Errorf("persist quote: %w", err)
	}

	resp := QuoteResponse{
		QuoteID:    quoteID,
		USDTAmount: sess.Amount,
		BRLAmount:  sess.TotalBRL,
		Price:      adjustedPrice,
		Settlement: sess.Settlement,
		Network:    req.Network,
		ExpiresAt:  expiresAt.Format(time.RFC3339),
	}
	if feePct > 0 {
		resp.RawPrice = rawPrice
		resp.FeePct = feePct
	}
	return resp, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// ConfirmOrder
// ─────────────────────────────────────────────────────────────────────────────

// ConfirmOrder executes the full confirmation sequence:
//
//  1. Validate the quote is open and not expired.
//  2. Debit BRL from dinacore (reserves the funds).
//  3. Close the Transfero session (books the trade at the locked price).
//     On failure: verify whether the trade actually occurred (GET /v1/closings).
//     If not: refund BRL.  If yes: continue to step 4.
//  4. Send BRL via PIX to the Transfero OTC desk PIX key (funds the trade).
//     On failure: persist order with status "payment_failed" and return error.
//  5. Credit USDT to dinacore (best-effort; logged on failure).
//  6. Persist the order record with status "awaiting_settlement".
func (s *OnRampService) ConfirmOrder(ctx context.Context, req OrderRequest) (OrderResponse, error) {
	// 1. Load and validate the quote
	quote, err := s.quoteStore.Get(ctx, req.QuoteID)
	if err != nil {
		return OrderResponse{}, err
	}
	if quote.Status == "used" {
		return OrderResponse{}, ErrQuoteUsed
	}
	if quote.Status == "expired" || time.Now().After(quote.ExpiresAt) {
		return OrderResponse{}, ErrQuoteExpired
	}

	// 2. Debit BRL from dinacore.
	// Use RequestedBRL (the merchant-facing amount) when available so that Dinacore
	// stays in sync with Dinapay's merchants.balance. For direct API calls that do
	// not supply RequestedBRL, fall back to quote.BRLAmount (Transfero's exact cost).
	dinaDebitAmt := quote.BRLAmount
	if req.RequestedBRL > 0 {
		dinaDebitAmt = req.RequestedBRL
	}
	if s.dinacore != nil && req.AccountID != "" {
		if err := s.dinacore.DebitBalance(ctx, req.AccountID, "BRL", dinaDebitAmt, req.QuoteID); err != nil {
			return OrderResponse{}, fmt.Errorf("debit BRL: %w", err)
		}
	}

	// 3. Close the Transfero session — books the trade at the locked price.
	//    wallet is sent here so Transfero knows where to deliver USDT after settlement.
	//    quoteId is the oid, making this call idempotent.
	closing, closeErr := s.transfero.CloseSession(ctx, quote.TransferoSessionID, req.QuoteID, req.DestinationAddress)
	if closeErr != nil {
		s.log.Warn("transfero close session failed; verifying trade state",
			"quoteId", req.QuoteID, "sessionId", quote.TransferoSessionID, "err", closeErr)

		// Verify whether the trade actually happened on Transfero's side
		confirmed, verifyErr := s.findClosingByOID(ctx, req.QuoteID)
		if verifyErr != nil || confirmed == nil {
			// Trade did NOT happen — refund the same amount that was debited.
			s.log.Info("trade not found on Transfero; refunding BRL", "quoteId", req.QuoteID)
			if s.dinacore != nil && req.AccountID != "" {
				if rfErr := s.dinacore.RefundBRL(ctx, req.AccountID, dinaDebitAmt, req.QuoteID); rfErr != nil {
					s.log.Error("BRL refund failed — MANUAL INTERVENTION REQUIRED",
						"quoteId", req.QuoteID, "account", req.AccountID,
						"brlAmount", dinaDebitAmt, "err", rfErr)
				}
			}
			return OrderResponse{}, fmt.Errorf("close session: %w", closeErr)
		}
		// Trade DID happen but close response was lost — continue with the verified closing
		s.log.Info("trade confirmed via closings history", "quoteId", req.QuoteID, "closingId", confirmed.ClosingID)
		closing = confirmed
	}

	// 4. Send BRL via PIX to the Transfero OTC desk — funds the booked trade.
	var pixPaymentGroupID string
	orderStatus := "awaiting_settlement"
	if s.otcDesk != nil && s.otcDesk.BaasClient != nil {
		payee := transfero.PIXPayee{
			Amount:       closing.TotalBRL,
			Currency:     "BRL",
			Name:         "Transfero OTC Desk",
			TaxIDCountry: "BRA",
			TaxID:        s.otcDesk.TaxID,
			PixKey:       s.otcDesk.PIXKey,
			Description:  "OTC settlement " + closing.ClosingID,
		}
		pgID, pixErr := s.otcDesk.BaasClient.SendPIX(ctx, s.otcDesk.AccountID, payee)
		if pixErr != nil {
			s.log.Error("PIX send to OTC desk failed — trade is booked but funds not sent; MANUAL INTERVENTION REQUIRED",
				"quoteId", req.QuoteID, "closingId", closing.ClosingID,
				"brlAmount", closing.TotalBRL, "otcPixKey", s.otcDesk.PIXKey, "err", pixErr)
			// Persist so operators can see it and retry
			_ = s.quoteStore.MarkUsed(ctx, req.QuoteID)
			orderID, _ := s.orderStore.Insert(ctx, store.Order{
				AccountID:          req.AccountID,
				QuoteID:            req.QuoteID,
				TransferoClosingID: closing.ClosingID,
				OID:                req.QuoteID,
				BRLAmount:          closing.TotalBRL,
				USDTAmount:         closing.Amount,
				Price:              closing.Price,
				Settlement:         closing.Settlement,
				DestinationAddress: req.DestinationAddress,
				Network:            quote.Network,
				Status:             "payment_failed",
			})
			_ = orderID
			return OrderResponse{}, fmt.Errorf("send BRL to OTC desk: %w", pixErr)
		}
		pixPaymentGroupID = pgID
		s.log.Info("BRL sent to Transfero OTC desk",
			"quoteId", req.QuoteID, "closingId", closing.ClosingID,
			"brlAmount", closing.TotalBRL, "paymentGroupId", pgID)
	} else {
		s.log.Warn("OTC desk not configured — skipping PIX send; trade is booked only",
			"quoteId", req.QuoteID, "closingId", closing.ClosingID)
		orderStatus = "confirmed"
	}

	// 5. Credit USDT to dinacore — only for in-swaps where USDT stays in Dinaria
	//    custody (req.DestinationAddress == ""). For payout-swaps the destination
	//    address is passed to Transfero at CloseSession and USDT is delivered
	//    directly to the customer's external wallet; crediting Dinacore here would
	//    create a phantom balance that does not reflect any real custody holding.
	//
	//    When the in-swap flow is implemented (customer holds a USDT balance inside
	//    Dinaria rather than receiving it on-chain immediately), DestinationAddress
	//    will be empty and this block will run correctly.
	if s.dinacore != nil && req.AccountID != "" && req.DestinationAddress == "" {
		if err := s.dinacore.CreditBalance(ctx, req.AccountID, "USDT", closing.Amount, closing.ClosingID); err != nil {
			s.log.Error("dinacore USDT credit failed — MANUAL INTERVENTION REQUIRED",
				"quoteId", req.QuoteID, "closingId", closing.ClosingID,
				"account", req.AccountID, "usdtAmount", closing.Amount, "err", err)
		}
	}

	// Mark the quote as consumed (best-effort)
	_ = s.quoteStore.MarkUsed(ctx, req.QuoteID)

	// 6. Persist the confirmed order (carry fee audit fields from the quote)
	orderID, err := s.orderStore.Insert(ctx, store.Order{
		AccountID:          req.AccountID,
		QuoteID:            req.QuoteID,
		TransferoClosingID: closing.ClosingID,
		OID:                req.QuoteID,
		BRLAmount:          closing.TotalBRL,
		USDTAmount:         closing.Amount,
		Price:              quote.Price,
		RawPrice:           quote.RawPrice,
		FeePct:             quote.FeePct,
		Settlement:         closing.Settlement,
		DestinationAddress: req.DestinationAddress,
		Network:            quote.Network,
		Status:             orderStatus,
		PixPaymentGroupID:  pixPaymentGroupID,
	})
	if err != nil {
		return OrderResponse{}, fmt.Errorf("persist order: %w", err)
	}

	resp := OrderResponse{
		OrderID:            orderID,
		QuoteID:            req.QuoteID,
		ClosingID:          closing.ClosingID,
		OID:                req.QuoteID,
		USDTAmount:         closing.Amount,
		BRLAmount:          closing.TotalBRL,
		Price:              quote.Price,
		Settlement:         closing.Settlement,
		DestinationAddress: quote.DestinationAddress,
		Network:            quote.Network,
		Status:             orderStatus,
		PixPaymentGroupID:  pixPaymentGroupID,
		CreatedAt:          closing.CreatedAt,
	}
	if quote.FeePct > 0 {
		resp.RawPrice = quote.RawPrice
		resp.FeePct = quote.FeePct
	}
	return resp, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal execution (called by dinapay OnRampExecutor)
// ─────────────────────────────────────────────────────────────────────────────

// ExecuteRequest is the input for ExecuteSettlement.
type ExecuteRequest struct {
	PayoutID   string  // dinapay payout ID (for logging / idempotency tracking)
	AccountID  string
	BRLAmount  float64
	Address    string
	Network    string
	Settlement string
}

// ExecuteResponse is the output for ExecuteSettlement.
type ExecuteResponse struct {
	SettlementID       string  `json:"settlementId"`
	ClosingID          string  `json:"closingId"`
	USDTAmount         float64 `json:"usdtAmount"`
	BRLAmount          float64 `json:"brlAmount"`
	ExchangeRate       float64 `json:"exchangeRate"`
	DestinationAddress string  `json:"destinationAddress"`
	Network            string  `json:"network"`
}

// ExecuteSettlement atomically runs CreateQuote + ConfirmOrder.
// Called by dinapay's OnRampExecutor from the inline execution path.
// The payoutId is stored in logs for traceability.
func (s *OnRampService) ExecuteSettlement(ctx context.Context, req ExecuteRequest) (ExecuteResponse, error) {
	if req.Settlement == "" {
		req.Settlement = "D0"
	}
	if req.Network == "" {
		req.Network = "mainnet"
	}

	s.log.Info("internal execute settlement",
		"payoutId", req.PayoutID,
		"accountId", req.AccountID,
		"brlAmount", req.BRLAmount,
	)

	quote, err := s.CreateQuote(ctx, QuoteRequest{
		AccountID:  req.AccountID,
		BRLAmount:  req.BRLAmount,
		Settlement: req.Settlement,
		Network:    req.Network,
	})
	if err != nil {
		return ExecuteResponse{}, fmt.Errorf("create quote: %w", err)
	}

	order, err := s.ConfirmOrder(ctx, OrderRequest{
		AccountID:          req.AccountID,
		QuoteID:            quote.QuoteID,
		DestinationAddress: req.Address,
		RequestedBRL:       req.BRLAmount, // full merchant amount; keeps Dinacore in sync with Dinapay
	})
	if err != nil {
		return ExecuteResponse{}, fmt.Errorf("confirm order: %w", err)
	}

	return ExecuteResponse{
		SettlementID:       order.OrderID,
		ClosingID:          order.ClosingID,
		USDTAmount:         order.USDTAmount,
		BRLAmount:          order.BRLAmount,
		ExchangeRate:       order.Price,
		DestinationAddress: order.DestinationAddress,
		Network:            order.Network,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Indicative rates
// ─────────────────────────────────────────────────────────────────────────────

// RatesResponse is the response for GET /v1/rates.
type RatesResponse struct {
	FromCurrency string  `json:"fromCurrency"`
	ToCurrency   string  `json:"toCurrency"`
	Price        float64 `json:"price"`
	Spot         float64 `json:"spot"`
	Settlement   string  `json:"settlement"`
	IndicativeAt string  `json:"indicativeAt"`
}

// GetIndicativeRates returns the current indicative BRL→USDT price without locking a session.
// If the account has a max_d0_premium_pct configured and the D0 price exceeds
// spot × (1 + maxPremium/100), a MARKET_CONDITION ProviderError is returned.
func (s *OnRampService) GetIndicativeRates(ctx context.Context, accountID, settlement string) (RatesResponse, error) {
	if settlement == "" {
		settlement = "D0"
	}

	grid, err := s.transfero.GetPrices(ctx)
	if err != nil {
		return RatesResponse{}, s.wrapTransferoErr(err, "get prices")
	}

	usdtPrices, ok := grid.Prices["USDT"]
	if !ok {
		return RatesResponse{}, fmt.Errorf("USDT prices not available")
	}

	var entry transfero.PriceEntry
	switch settlement {
	case "D0":
		entry = usdtPrices.D0
	case "D1":
		entry = usdtPrices.D1
	case "D2":
		entry = usdtPrices.D2
	default:
		return RatesResponse{}, fmt.Errorf("invalid settlement %q", settlement)
	}

	// Guard: reject if D0 premium over spot exceeds the account's configured threshold.
	if s.settingsStore != nil && accountID != "" {
		maxPremium, err := s.settingsStore.GetMaxD0PremiumPct(ctx, accountID)
		if err != nil {
			s.log.Warn("failed to load max_d0_premium_pct; skipping guard", "account", accountID, "err", err)
		} else if maxPremium != nil && grid.Spot > 0 {
			ceiling := grid.Spot * (1 + *maxPremium/100)
			if entry.Price > ceiling {
				s.log.Info("MARKET_CONDITION: D0 premium exceeds threshold",
					"account", accountID,
					"spot", grid.Spot,
					"d0", entry.Price,
					"max_premium_pct", *maxPremium,
					"ceiling", ceiling,
				)
				return RatesResponse{}, &ProviderError{Status: 422, Code: "MARKET_CONDITION"}
			}
		}
	}

	return RatesResponse{
		FromCurrency: "BRL",
		ToCurrency:   "USDT",
		Price:        entry.Price,
		Spot:         grid.Spot,
		Settlement:   settlement,
		IndicativeAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// findClosingByOID searches recent Transfero closings for one matching our oid.
// Used as a verification step when CloseSession returns an error.
func (s *OnRampService) findClosingByOID(ctx context.Context, oid string) (*transfero.Closing, error) {
	// Search the first few pages (the matching closing should be very recent)
	for page := 1; page <= 3; page++ {
		list, err := s.transfero.GetClosings(ctx, page, 50)
		if err != nil {
			return nil, err
		}
		for i := range list.Data {
			if list.Data[i].OID == oid {
				return &list.Data[i], nil
			}
		}
		if page >= list.Pagination.TotalPages {
			break
		}
	}
	return nil, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Read operations
// ─────────────────────────────────────────────────────────────────────────────

// GetOrder retrieves a single order by ID.
func (s *OnRampService) GetOrder(ctx context.Context, id string) (OrderResponse, error) {
	o, err := s.orderStore.Get(ctx, id)
	if err != nil {
		return OrderResponse{}, err
	}
	return orderToResponse(o), nil
}

// ListOrders returns a paginated list of orders, newest first.
func (s *OnRampService) ListOrders(ctx context.Context, page, pageSize int) (OrderListResponse, error) {
	if pageSize < 1 {
		pageSize = 50
	}
	orders, total, err := s.orderStore.List(ctx, page, pageSize)
	if err != nil {
		return OrderListResponse{}, err
	}
	totalPages := (total + pageSize - 1) / pageSize
	data := make([]OrderResponse, 0, len(orders))
	for _, o := range orders {
		data = append(data, orderToResponse(o))
	}
	return OrderListResponse{
		Data: data,
		Pagination: PaginationMeta{
			Page:       page,
			PageSize:   pageSize,
			Total:      total,
			TotalPages: totalPages,
		},
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func orderToResponse(o store.Order) OrderResponse {
	resp := OrderResponse{
		OrderID:            o.ID,
		QuoteID:            o.QuoteID,
		ClosingID:          o.TransferoClosingID,
		OID:                o.OID,
		USDTAmount:         o.USDTAmount,
		BRLAmount:          o.BRLAmount,
		Price:              o.Price,
		Settlement:         o.Settlement,
		DestinationAddress: o.DestinationAddress,
		Network:            o.Network,
		Status:             o.Status,
		PixPaymentGroupID:  o.PixPaymentGroupID,
		CreatedAt:          o.CreatedAt.Format(time.RFC3339),
	}
	if o.FeePct > 0 {
		resp.RawPrice = o.RawPrice
		resp.FeePct = o.FeePct
	}
	return resp
}

func (s *OnRampService) wrapTransferoErr(err error, op string) error {
	var apiErr *transfero.APIError
	if errors.As(err, &apiErr) {
		code := strings.TrimSpace(vendorRe.ReplaceAllString(apiErr.Code, ""))
		if code == "" {
			code = strings.TrimSpace(vendorRe.ReplaceAllString(apiErr.Title, ""))
		}
		return &ProviderError{Status: apiErr.Status, Code: code}
	}
	return fmt.Errorf("%s: %w", op, err)
}
