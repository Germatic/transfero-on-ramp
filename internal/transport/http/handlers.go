package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"transfero-on-ramp/internal/auth"
	"transfero-on-ramp/internal/service"
	"transfero-on-ramp/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeProviderErr writes a structured provider error response with only
// "status" and "code" fields. Returns true so callers can return early.
func writeProviderErr(w http.ResponseWriter, err error) bool {
	var pe *service.ProviderError
	if !errors.As(err, &pe) {
		return false
	}
	writeJSON(w, pe.Status, map[string]any{
		"status": pe.Status,
		"code":   pe.Code,
	})
	return true
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// ─────────────────────────────────────────────────────────────────────────────
// Health
// ─────────────────────────────────────────────────────────────────────────────

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ─────────────────────────────────────────────────────────────────────────────
// Quote handlers
// ─────────────────────────────────────────────────────────────────────────────

// POST /v1/quotes
// Body: { "brlAmount": 25000.00, "settlement": "D0", "network": "mainnet" }
// Note: destinationAddress is provided at settlement/confirm time, not here.
func handleCreateQuote(svc *service.OnRampService, log *slog.Logger) http.HandlerFunc {
	type request struct {
		BRLAmount  float64 `json:"brlAmount"`
		Settlement string  `json:"settlement"`
		Network    string  `json:"network"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		account, ok := auth.FromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		var req request
		if err := decode(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.BRLAmount <= 0 {
			writeError(w, http.StatusBadRequest, "brlAmount must be positive")
			return
		}

		resp, err := svc.CreateQuote(r.Context(), service.QuoteRequest{
			AccountID:  account.ID,
			BRLAmount:  req.BRLAmount,
			Settlement: req.Settlement,
			Network:    req.Network,
		})
		if err != nil {
			log.Warn("create quote failed", "err", err)
			if writeProviderErr(w, err) {
				return
			}
			code, msg := mapServiceErr(err)
			writeError(w, code, msg)
			return
		}

		writeJSON(w, http.StatusCreated, resp)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Order handlers
// ─────────────────────────────────────────────────────────────────────────────

// POST /v1/orders
// Body: { "quoteId": "...", "destinationAddress": "T..." }
func handleConfirmOrder(svc *service.OnRampService, log *slog.Logger) http.HandlerFunc {
	type request struct {
		QuoteID            string `json:"quoteId"`
		DestinationAddress string `json:"destinationAddress"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		account, ok := auth.FromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		var req request
		if err := decode(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.QuoteID == "" {
			writeError(w, http.StatusBadRequest, "quoteId is required")
			return
		}
		if req.DestinationAddress == "" {
			writeError(w, http.StatusBadRequest, "destinationAddress is required")
			return
		}

		resp, err := svc.ConfirmOrder(r.Context(), service.OrderRequest{
			AccountID:          account.ID,
			QuoteID:            req.QuoteID,
			DestinationAddress: req.DestinationAddress,
		})
		if err != nil {
			log.Warn("confirm order failed", "quoteId", req.QuoteID, "err", err)
			if writeProviderErr(w, err) {
				return
			}
			code, msg := mapServiceErr(err)
			writeError(w, code, msg)
			return
		}

		writeJSON(w, http.StatusCreated, resp)
	}
}

// GET /v1/orders/{id}
func handleGetOrder(svc *service.OnRampService, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "missing order id")
			return
		}

		resp, err := svc.GetOrder(r.Context(), id)
		if err != nil {
			code, msg := mapServiceErr(err)
			log.Warn("get order failed", "id", id, "err", err)
			writeError(w, code, msg)
			return
		}

		writeJSON(w, http.StatusOK, resp)
	}
}

// GET /v1/orders?page=1&pageSize=50
func handleListOrders(svc *service.OnRampService, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page := parseIntParam(r, "page", 1)
		pageSize := parseIntParam(r, "pageSize", 50)

		resp, err := svc.ListOrders(r.Context(), page, pageSize)
		if err != nil {
			log.Warn("list orders failed", "err", err)
			writeError(w, http.StatusInternalServerError, "list orders failed")
			return
		}

		writeJSON(w, http.StatusOK, resp)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Rates handler
// ─────────────────────────────────────────────────────────────────────────────

// GET /v1/rates?settlement=D0
// Returns an indicative BRL→USDT price. No quote is locked.
func handleGetRates(svc *service.OnRampService, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settlement := r.URL.Query().Get("settlement")
		if settlement == "" {
			settlement = "D0"
		}

		account, _ := auth.FromContext(r.Context())
		resp, err := svc.GetIndicativeRates(r.Context(), account.ID, settlement)
		if err != nil {
			log.Warn("get rates failed", "err", err)
			if writeProviderErr(w, err) {
				return
			}
			code, msg := mapServiceErr(err)
			writeError(w, code, msg)
			return
		}

		writeJSON(w, http.StatusOK, resp)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal execute handler (called by dinapay OnRampExecutor)
// ─────────────────────────────────────────────────────────────────────────────

// POST /v1/internal/execute
// Atomically executes a BRL→USDT settlement: quote + confirm in one call.
// Only reachable with the ONRAMP_INTERNAL_KEY registered in ONRAMP_API_KEYS.
func handleInternalExecute(svc *service.OnRampService, log *slog.Logger) http.HandlerFunc {
	type request struct {
		PayoutID   string  `json:"payoutId"`
		AccountID  string  `json:"accountId"`
		BRLAmount  float64 `json:"brlAmount"`
		Address    string  `json:"destinationAddress"`
		Network    string  `json:"network"`
		Settlement string  `json:"settlement"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := decode(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.BRLAmount <= 0 {
			writeError(w, http.StatusBadRequest, "brlAmount must be positive")
			return
		}
		if req.Address == "" {
			writeError(w, http.StatusBadRequest, "destinationAddress is required")
			return
		}
		if req.AccountID == "" {
			writeError(w, http.StatusBadRequest, "accountId is required")
			return
		}

		resp, err := svc.ExecuteSettlement(r.Context(), service.ExecuteRequest{
			PayoutID:   req.PayoutID,
			AccountID:  req.AccountID,
			BRLAmount:  req.BRLAmount,
			Address:    req.Address,
			Network:    req.Network,
			Settlement: req.Settlement,
		})
		if err != nil {
			log.Warn("internal execute failed", "payoutId", req.PayoutID, "err", err)
			if writeProviderErr(w, err) {
				return
			}
			code, msg := mapServiceErr(err)
			writeError(w, code, msg)
			return
		}

		writeJSON(w, http.StatusCreated, resp)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Error mapping
// ─────────────────────────────────────────────────────────────────────────────

func mapServiceErr(err error) (int, string) {
	switch {
	case errors.Is(err, service.ErrQuoteExpired):
		return http.StatusGone, "quote expired"
	case errors.Is(err, service.ErrQuoteUsed):
		return http.StatusConflict, "quote already used"
	case errors.Is(err, store.ErrNotFound), errors.Is(err, service.ErrNotFound):
		return http.StatusNotFound, "not found"
	case errors.Is(err, service.ErrInsufficientBalance):
		return http.StatusPaymentRequired, "insufficient BRL balance"
	default:
		return http.StatusInternalServerError, "internal error"
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Utilities
// ─────────────────────────────────────────────────────────────────────────────

func parseIntParam(r *http.Request, name string, def int) int {
	if s := r.URL.Query().Get(name); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			return v
		}
	}
	return def
}
