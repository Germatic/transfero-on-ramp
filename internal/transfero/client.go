// Package transfero provides a client for the Transfero OTC API.
//
// Authentication flow:
//   POST /v1/auth/login  →  short-lived JWT
//
// The JWT is cached in memory and automatically refreshed when it is within
// 60 seconds of expiry, so callers never need to manage tokens manually.
//
// Trading flow:
//   GET  /v1/prices           → live price grid (BRL per USDT/USDC, D0/D1/D2)
//   POST /v1/sessions         → lock a price for ~7 seconds
//   POST /v1/sessions/:id/close → confirm the trade (idempotent via oid)
//   GET  /v1/closings         → confirmed trade history
package transfero

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Public types
// ─────────────────────────────────────────────────────────────────────────────

// PriceGrid is returned by GET /v1/prices.
type PriceGrid struct {
	Tier      string             `json:"tier"`
	Spot      float64            `json:"spot"`
	Timestamp string             `json:"timestamp"`
	Prices    map[string]Windows `json:"prices"` // key: "USDT" | "USDC"
}

// Windows holds D0/D1/D2 price entries for one currency.
type Windows struct {
	D0 PriceEntry `json:"D0"`
	D1 PriceEntry `json:"D1"`
	D2 PriceEntry `json:"D2"`
}

// PriceEntry is a single settlement price with its spread.
type PriceEntry struct {
	Price     float64 `json:"price"`      // BRL per USD unit
	SpreadPct float64 `json:"spread_pct"` // e.g. 1.02 means 1.02 % spread
}

// Session is returned by POST /v1/sessions.
type Session struct {
	SessionID  string  `json:"session_id"`
	Currency   string  `json:"currency"`
	Settlement string  `json:"settlement"`
	Amount     float64 `json:"amount"`    // USD amount
	Price      float64 `json:"price"`     // BRL per USD at lock time
	Spot       float64 `json:"spot"`
	SpreadPct  float64 `json:"spread_pct"`
	TotalBRL   float64 `json:"total_brl"` // = amount × price
	Tier       string  `json:"tier"`
	ClientName string  `json:"client_name"`
	Status     string  `json:"status"` // "open"
	CreatedAt  string  `json:"created_at"`
	ExpiresAt  string  `json:"expires_at"` // RFC3339, ~7s from creation
}

// Closing is returned by POST /v1/sessions/:id/close and GET /v1/closings.
type Closing struct {
	ClosingID  string  `json:"closing_id"`
	OID        string  `json:"oid"`
	SessionID  string  `json:"session_id"`
	Currency   string  `json:"currency"`
	Settlement string  `json:"settlement"`
	Side       string  `json:"side"`
	Amount     float64 `json:"amount"`
	Price      float64 `json:"price"`
	Spot       float64 `json:"spot"`
	SpreadPct  float64 `json:"spread_pct"`
	TotalBRL   float64 `json:"total_brl"`
	Tier       string  `json:"tier"`
	ClientName string  `json:"client_name"`
	ClosedAt   string  `json:"closed_at"`
	CreatedAt  string  `json:"created_at"`
}

// ClosingList is returned by GET /v1/closings.
type ClosingList struct {
	Data       []Closing  `json:"data"`
	Pagination Pagination `json:"pagination"`
}

// Pagination metadata from list endpoints.
type Pagination struct {
	Page       int `json:"page"`
	PageSize   int `json:"page_size"`
	Total      int `json:"total"`
	TotalPages int `json:"total_pages"`
}

// APIError represents a Transfero RFC-7807 error response.
type APIError struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
	Title  string `json:"title"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("transfero %d %s: %s", e.Status, e.Code, e.Detail)
}

// ─────────────────────────────────────────────────────────────────────────────
// Client
// ─────────────────────────────────────────────────────────────────────────────

// Client is a thread-safe Transfero OTC API client with built-in JWT caching.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// New creates a Client for the given base URL and API key.
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Auth
// ─────────────────────────────────────────────────────────────────────────────

// bearerToken returns a valid JWT, refreshing it if necessary.
// It is safe to call concurrently; at most one refresh happens at a time.
func (c *Client) bearerToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Refresh when the token is absent or will expire within 60 seconds
	if c.token == "" || time.Until(c.expiresAt) < 60*time.Second {
		if err := c.login(ctx); err != nil {
			return "", err
		}
	}
	return c.token, nil
}

// login calls POST /v1/auth/login and stores the token.
// Must be called with c.mu held.
func (c *Client) login(ctx context.Context) error {
	body := map[string]string{"api_key": c.apiKey}

	var resp struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"` // RFC3339
	}
	if err := c.do(ctx, "POST", "/v1/auth/login", nil, body, &resp); err != nil {
		return fmt.Errorf("transfero login: %w", err)
	}

	exp, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	if err != nil {
		// Fall back to 30 minutes if the format is unexpected
		exp = time.Now().Add(30 * time.Minute)
	}
	c.token = resp.Token
	c.expiresAt = exp
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Trading endpoints
// ─────────────────────────────────────────────────────────────────────────────

// GetPrices returns the live price grid for all currencies and settlement windows.
func (c *Client) GetPrices(ctx context.Context) (*PriceGrid, error) {
	tok, err := c.bearerToken(ctx)
	if err != nil {
		return nil, err
	}
	var grid PriceGrid
	if err := c.do(ctx, "GET", "/v1/prices", &tok, nil, &grid); err != nil {
		return nil, fmt.Errorf("transfero get prices: %w", err)
	}
	return &grid, nil
}

// CreateSession locks a price quote for the given USDT amount and settlement window.
// The session expires after the server-configured TTL (~7 seconds by default).
// The destination wallet is provided at close time, not here.
func (c *Client) CreateSession(ctx context.Context, usdtAmount float64, settlement string) (*Session, error) {
	tok, err := c.bearerToken(ctx)
	if err != nil {
		return nil, err
	}

	body := map[string]any{
		"currency":   "USDT",
		"settlement": settlement,
		"amount":     usdtAmount,
	}

	var sess Session
	if err := c.do(ctx, "POST", "/v1/sessions", &tok, body, &sess); err != nil {
		return nil, fmt.Errorf("transfero create session: %w", err)
	}
	return &sess, nil
}

// CloseSession confirms the trade locked by the session.
// oid is the idempotency key — reusing the same oid returns the existing closing.
// wallet is the TRC20 address where Transfero will deliver USDT after settlement.
func (c *Client) CloseSession(ctx context.Context, sessionID, oid, wallet string) (*Closing, error) {
	tok, err := c.bearerToken(ctx)
	if err != nil {
		return nil, err
	}

	body := map[string]string{"oid": oid, "wallet": wallet}
	var closing Closing
	path := "/v1/sessions/" + sessionID + "/close"
	if err := c.do(ctx, "POST", path, &tok, body, &closing); err != nil {
		return nil, fmt.Errorf("transfero close session %s: %w", sessionID, err)
	}
	return &closing, nil
}

// GetClosings returns the paginated trade history.
func (c *Client) GetClosings(ctx context.Context, page, pageSize int) (*ClosingList, error) {
	tok, err := c.bearerToken(ctx)
	if err != nil {
		return nil, err
	}

	path := fmt.Sprintf("/v1/closings?page=%d&page_size=%d", page, pageSize)
	var list ClosingList
	if err := c.do(ctx, "GET", path, &tok, nil, &list); err != nil {
		return nil, fmt.Errorf("transfero get closings: %w", err)
	}
	return &list, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal HTTP helper
// ─────────────────────────────────────────────────────────────────────────────

// do executes an HTTP request and decodes the JSON response into out.
// If bearer is non-nil it is added as an Authorization header.
// If reqBody is non-nil it is JSON-encoded as the request body.
// On non-2xx responses, an *APIError is returned.
func (c *Client) do(ctx context.Context, method, path string, bearer *string, reqBody, out any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		buf, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != nil {
		req.Header.Set("Authorization", "Bearer "+*bearer)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr APIError
		apiErr.Status = resp.StatusCode
		// Best-effort parse of the RFC-7807 error body
		_ = json.Unmarshal(data, &apiErr)
		if apiErr.Code == "" {
			apiErr.Code = http.StatusText(resp.StatusCode)
		}
		if apiErr.Detail == "" {
			apiErr.Detail = string(data)
		}
		return &apiErr
	}

	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
