// Package dinacore provides an HTTP client for the dinacore balance API.
//
// The on-ramp uses this to debit BRL and credit USDT on a customer's account
// after a Transfero trade is confirmed.
//
// Authentication: X-Api-Key header (same scheme as dinapay uses internally).
package dinacore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Types
// ─────────────────────────────────────────────────────────────────────────────

// BalanceResponse is returned by GET /api/balance/merchant/:id.
type BalanceResponse struct {
	MerchantID string  `json:"merchantId"`
	Currency   string  `json:"currency"`
	Balance    float64 `json:"balance"`
}

// APIError represents a dinacore error response.
type APIError struct {
	Status  int
	Message string `json:"error"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("dinacore %d: %s", e.Status, e.Message)
}

// ─────────────────────────────────────────────────────────────────────────────
// Client
// ─────────────────────────────────────────────────────────────────────────────

// Client is a thread-safe HTTP client for the dinacore balance API.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New creates a Client for the given dinacore base URL and API key.
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Balance operations
// ─────────────────────────────────────────────────────────────────────────────

// GetBalance returns the current balance for the given account and currency.
func (c *Client) GetBalance(ctx context.Context, accountID, currency string) (float64, error) {
	path := fmt.Sprintf("/api/balance/merchant/%s?currency=%s", accountID, currency)
	var resp BalanceResponse
	if err := c.do(ctx, "GET", path, nil, &resp); err != nil {
		return 0, fmt.Errorf("dinacore get balance (%s %s): %w", accountID, currency, err)
	}
	return resp.Balance, nil
}

// DebitBalance debits amount from the account's currency balance.
// refID is written to balance_ledger as the audit reference.
func (c *Client) DebitBalance(ctx context.Context, accountID, currency string, amount float64, refID string) error {
	body := map[string]any{
		"merchantId": accountID,
		"currency":   currency,
		"amount":     amount,
		"refId":      refID,
		"refType":    "onramp_debit",
	}
	if err := c.do(ctx, "POST", "/api/balance/debit", body, nil); err != nil {
		return fmt.Errorf("dinacore debit (%s %s %.6f ref=%s): %w", accountID, currency, amount, refID, err)
	}
	return nil
}

// CreditBalance credits amount to the account's currency balance.
// refID is written to balance_ledger as the audit reference.
func (c *Client) CreditBalance(ctx context.Context, accountID, currency string, amount float64, refID string) error {
	body := map[string]any{
		"merchantId": accountID,
		"currency":   currency,
		"amount":     amount,
		"refId":      refID,
		"refType":    "onramp_credit",
	}
	if err := c.do(ctx, "POST", "/api/balance/credit", body, nil); err != nil {
		return fmt.Errorf("dinacore credit (%s %s %.6f ref=%s): %w", accountID, currency, amount, refID, err)
	}
	return nil
}

// RefundBRL restores a BRL debit when a Transfero trade is confirmed to have
// not happened. Uses refType "onramp_refund" in the ledger for clarity.
func (c *Client) RefundBRL(ctx context.Context, accountID string, amount float64, originalRefID string) error {
	body := map[string]any{
		"merchantId": accountID,
		"currency":   "BRL",
		"amount":     amount,
		"refId":      originalRefID + "-refund",
		"refType":    "onramp_refund",
	}
	if err := c.do(ctx, "POST", "/api/balance/credit", body, nil); err != nil {
		return fmt.Errorf("dinacore refund BRL (%s %.6f): %w", accountID, amount, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal HTTP helper
// ─────────────────────────────────────────────────────────────────────────────

func (c *Client) do(ctx context.Context, method, path string, reqBody, out any) error {
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
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr APIError
		apiErr.Status = resp.StatusCode
		_ = json.Unmarshal(data, &apiErr)
		if apiErr.Message == "" {
			apiErr.Message = string(data)
		}
		return &apiErr
	}

	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
