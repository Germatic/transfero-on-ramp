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
	"time"

	"transfero-on-ramp/internal/dinacore"
	"transfero-on-ramp/internal/store"
	"transfero-on-ramp/internal/transfero"
)

// Sentinel errors that handlers inspect to return correct HTTP status codes.
var (
	ErrMarketClosed  = errors.New("outside market hours")
	ErrQuoteExpired  = store.ErrQuoteExpired
	ErrQuoteUsed     = store.ErrQuoteUsed
	ErrNotFound      = store.ErrNotFound
	ErrInsufficientBalance = errors.New("insufficient BRL balance")
)

// ─────────────────────────────────────────────────────────────────────────────
// Input / output types
// ─────────────────────────────────────────────────────────────────────────────

// QuoteRequest is the validated input for CreateQuote.
type QuoteRequest struct {
	AccountID          string  // resolved from Bearer token, not supplied by caller
	BRLAmount          float64 // e.g. 25000.00
	DestinationAddress string  // Tron address for on-chain USDT delivery
	Settlement         string  // D0 | D1 | D2 (default D0)
	Network            string  // mainnet | shasta (default mainnet)
}

// QuoteResponse is returned to the customer after a price is locked.
type QuoteResponse struct {
	QuoteID            string  `json:"quoteId"`
	USDTAmount         float64 `json:"usdtAmount"`
	BRLAmount          float64 `json:"brlAmount"`
	Price              float64 `json:"price"`      // BRL per USDT
	Settlement         string  `json:"settlement"`
	DestinationAddress string  `json:"destinationAddress"`
	Network            string  `json:"network"`
	ExpiresAt          string  `json:"expiresAt"` // RFC3339
}

// OrderRequest is the validated input for ConfirmOrder.
type OrderRequest struct {
	AccountID string // resolved from Bearer token
	QuoteID   string
}

// OrderResponse is returned after a trade is confirmed.
type OrderResponse struct {
	OrderID            string  `json:"orderId"`
	QuoteID            string  `json:"quoteId"`
	ClosingID          string  `json:"closingId"`
	OID                string  `json:"oid"`
	USDTAmount         float64 `json:"usdtAmount"`
	BRLAmount          float64 `json:"brlAmount"`
	Price              float64 `json:"price"`
	Settlement         string  `json:"settlement"`
	DestinationAddress string  `json:"destinationAddress"`
	Network            string  `json:"network"`
	Status             string  `json:"status"`
	CreatedAt          string  `json:"createdAt"`
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

// OnRampService orchestrates quote creation and order confirmation.
type OnRampService struct {
	transfero  *transfero.Client
	dinacore   *dinacore.Client
	quoteStore *store.QuoteStore
	orderStore *store.OrderStore
	log        *slog.Logger
}

// NewOnRampService creates an OnRampService with its required dependencies.
func NewOnRampService(
	tc *transfero.Client,
	dc *dinacore.Client,
	qs *store.QuoteStore,
	os *store.OrderStore,
	log *slog.Logger,
) *OnRampService {
	return &OnRampService{
		transfero:  tc,
		dinacore:   dc,
		quoteStore: qs,
		orderStore: os,
		log:        log,
	}
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

	// BRL ÷ price = USDT, rounded to 6 decimal places
	usdtAmount := math.Round((req.BRLAmount/entry.Price)*1_000_000) / 1_000_000

	// Check that the account has enough BRL balance before locking the price
	if s.dinacore != nil && req.AccountID != "" {
		balance, err := s.dinacore.GetBalance(ctx, req.AccountID, "BRL")
		if err != nil {
			s.log.Warn("dinacore balance check failed (proceeding)", "account", req.AccountID, "err", err)
		} else if balance < req.BRLAmount {
			return QuoteResponse{}, ErrInsufficientBalance
		}
	}

	// Lock the price by creating a Transfero session
	sess, err := s.transfero.CreateSession(ctx, usdtAmount, req.Settlement, req.DestinationAddress)
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
		Price:              sess.Price,
		Settlement:         sess.Settlement,
		DestinationAddress: req.DestinationAddress,
		Network:            req.Network,
		ExpiresAt:          expiresAt,
	})
	if err != nil {
		return QuoteResponse{}, fmt.Errorf("persist quote: %w", err)
	}

	return QuoteResponse{
		QuoteID:            quoteID,
		USDTAmount:         sess.Amount,
		BRLAmount:          sess.TotalBRL,
		Price:              sess.Price,
		Settlement:         sess.Settlement,
		DestinationAddress: req.DestinationAddress,
		Network:            req.Network,
		ExpiresAt:          expiresAt.Format(time.RFC3339),
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// ConfirmOrder
// ─────────────────────────────────────────────────────────────────────────────

// ConfirmOrder executes the full confirmation sequence:
//
//  1. Validate the quote is open and not expired.
//  2. Debit BRL from dinacore (reserves the funds).
//  3. Close the Transfero session (books the trade).
//     On failure: verify whether the trade actually occurred (GET /v1/closings).
//     If not: refund BRL.  If yes: continue to step 4.
//  4. Credit USDT to dinacore (best-effort; logged on failure).
//  5. Persist the order record.
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

	// 2. Debit BRL from dinacore
	if s.dinacore != nil && req.AccountID != "" {
		if err := s.dinacore.DebitBalance(ctx, req.AccountID, "BRL", quote.BRLAmount, req.QuoteID); err != nil {
			return OrderResponse{}, fmt.Errorf("debit BRL: %w", err)
		}
	}

	// 3. Close the Transfero session — books the trade
	//    quoteId is the oid, making this call idempotent.
	closing, closeErr := s.transfero.CloseSession(ctx, quote.TransferoSessionID, req.QuoteID)
	if closeErr != nil {
		s.log.Warn("transfero close session failed; verifying trade state",
			"quoteId", req.QuoteID, "sessionId", quote.TransferoSessionID, "err", closeErr)

		// Verify whether the trade actually happened on Transfero's side
		confirmed, verifyErr := s.findClosingByOID(ctx, req.QuoteID)
		if verifyErr != nil || confirmed == nil {
			// Trade did NOT happen — refund BRL
			s.log.Info("trade not found on Transfero; refunding BRL", "quoteId", req.QuoteID)
			if s.dinacore != nil && req.AccountID != "" {
				if rfErr := s.dinacore.RefundBRL(ctx, req.AccountID, quote.BRLAmount, req.QuoteID); rfErr != nil {
					s.log.Error("BRL refund failed — MANUAL INTERVENTION REQUIRED",
						"quoteId", req.QuoteID, "account", req.AccountID,
						"brlAmount", quote.BRLAmount, "err", rfErr)
				}
			}
			return OrderResponse{}, fmt.Errorf("close session: %w", closeErr)
		}
		// Trade DID happen but close response was lost — continue with the verified closing
		s.log.Info("trade confirmed via closings history", "quoteId", req.QuoteID, "closingId", confirmed.ClosingID)
		closing = confirmed
	}

	// 4. Credit USDT to dinacore (best-effort: the trade is done, USDT is owed)
	if s.dinacore != nil && req.AccountID != "" {
		if err := s.dinacore.CreditBalance(ctx, req.AccountID, "USDT", closing.Amount, closing.ClosingID); err != nil {
			// Log but do not fail — the Transfero trade is confirmed and USDT is in flight
			s.log.Error("dinacore USDT credit failed — MANUAL INTERVENTION REQUIRED",
				"quoteId", req.QuoteID, "closingId", closing.ClosingID,
				"account", req.AccountID, "usdtAmount", closing.Amount, "err", err)
		}
	}

	// Mark the quote as consumed (best-effort)
	_ = s.quoteStore.MarkUsed(ctx, req.QuoteID)

	// 5. Persist the confirmed order
	orderID, err := s.orderStore.Insert(ctx, store.Order{
		AccountID:          req.AccountID,
		QuoteID:            req.QuoteID,
		TransferoClosingID: closing.ClosingID,
		OID:                req.QuoteID,
		BRLAmount:          closing.TotalBRL,
		USDTAmount:         closing.Amount,
		Price:              closing.Price,
		Settlement:         closing.Settlement,
		DestinationAddress: quote.DestinationAddress,
		Network:            quote.Network,
	})
	if err != nil {
		return OrderResponse{}, fmt.Errorf("persist order: %w", err)
	}

	return OrderResponse{
		OrderID:            orderID,
		QuoteID:            req.QuoteID,
		ClosingID:          closing.ClosingID,
		OID:                req.QuoteID,
		USDTAmount:         closing.Amount,
		BRLAmount:          closing.TotalBRL,
		Price:              closing.Price,
		Settlement:         closing.Settlement,
		DestinationAddress: quote.DestinationAddress,
		Network:            quote.Network,
		Status:             "confirmed",
		CreatedAt:          closing.CreatedAt,
	}, nil
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
		AccountID:          req.AccountID,
		BRLAmount:          req.BRLAmount,
		DestinationAddress: req.Address,
		Settlement:         req.Settlement,
		Network:            req.Network,
	})
	if err != nil {
		return ExecuteResponse{}, fmt.Errorf("create quote: %w", err)
	}

	order, err := s.ConfirmOrder(ctx, OrderRequest{
		AccountID: req.AccountID,
		QuoteID:   quote.QuoteID,
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
	Settlement   string  `json:"settlement"`
	IndicativeAt string  `json:"indicativeAt"`
}

// GetIndicativeRates returns the current indicative BRL→USDT price without locking a session.
func (s *OnRampService) GetIndicativeRates(ctx context.Context, settlement string) (RatesResponse, error) {
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

	return RatesResponse{
		FromCurrency: "BRL",
		ToCurrency:   "USDT",
		Price:        entry.Price,
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
	return OrderResponse{
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
		CreatedAt:          o.CreatedAt.Format(time.RFC3339),
	}
}

func (s *OnRampService) wrapTransferoErr(err error, op string) error {
	var apiErr *transfero.APIError
	if errors.As(err, &apiErr) && (apiErr.Status == 403) {
		return ErrMarketClosed
	}
	return fmt.Errorf("%s: %w", op, err)
}
