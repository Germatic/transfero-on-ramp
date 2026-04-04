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
// Body: { "brlAmount": 25000.00, "destinationAddress": "T...", "settlement": "D0", "network": "mainnet" }
func handleCreateQuote(svc *service.OnRampService, log *slog.Logger) http.HandlerFunc {
	type request struct {
		BRLAmount          float64 `json:"brlAmount"`
		DestinationAddress string  `json:"destinationAddress"`
		Settlement         string  `json:"settlement"`
		Network            string  `json:"network"`
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
		if req.DestinationAddress == "" {
			writeError(w, http.StatusBadRequest, "destinationAddress is required")
			return
		}

		resp, err := svc.CreateQuote(r.Context(), service.QuoteRequest{
			AccountID:          account.ID,
			BRLAmount:          req.BRLAmount,
			DestinationAddress: req.DestinationAddress,
			Settlement:         req.Settlement,
			Network:            req.Network,
		})
		if err != nil {
			code, msg := mapServiceErr(err)
			log.Warn("create quote failed", "err", err)
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
// Body: { "quoteId": "..." }
func handleConfirmOrder(svc *service.OnRampService, log *slog.Logger) http.HandlerFunc {
	type request struct {
		QuoteID string `json:"quoteId"`
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

		resp, err := svc.ConfirmOrder(r.Context(), service.OrderRequest{
			AccountID: account.ID,
			QuoteID:   req.QuoteID,
		})
		if err != nil {
			code, msg := mapServiceErr(err)
			log.Warn("confirm order failed", "quoteId", req.QuoteID, "err", err)
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
// Error mapping
// ─────────────────────────────────────────────────────────────────────────────

func mapServiceErr(err error) (int, string) {
	switch {
	case errors.Is(err, service.ErrMarketClosed):
		return http.StatusServiceUnavailable, "market closed"
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
