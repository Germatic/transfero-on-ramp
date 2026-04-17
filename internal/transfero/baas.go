// Package transfero — BaaS OAuth2 client.
//
// BaasClient authenticates via Azure AD client_credentials and wraps the
// Transfero BaaS API endpoint that submits PIX payment groups.
// It is separate from Client (OTC JWT-based auth) so both can coexist.
package transfero

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// BaasClient authenticates via Azure AD client_credentials and exposes a
// minimal subset of the Transfero BaaS API used by the on-ramp service.
type BaasClient struct {
	baseURL      string
	tokenURL     string
	clientID     string
	clientSecret string
	scope        string
	http         *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

// NewBaasClient creates a BaasClient ready to call the Transfero BaaS API.
func NewBaasClient(baseURL, tokenURL, clientID, clientSecret, scope string) *BaasClient {
	return &BaasClient{
		baseURL:      strings.TrimRight(baseURL, "/"),
		tokenURL:     tokenURL,
		clientID:     clientID,
		clientSecret: clientSecret,
		scope:        scope,
		http:         &http.Client{Timeout: 20 * time.Second},
	}
}

// ─── Auth ─────────────────────────────────────────────────────────────────────

func (c *BaasClient) getAccessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.accessToken != "" && time.Now().Add(60*time.Second).Before(c.expiresAt) {
		return c.accessToken, nil
	}

	data := url.Values{}
	data.Set("grant_type", "client_credentials")
	data.Set("client_id", c.clientID)
	data.Set("client_secret", c.clientSecret)
	data.Set("scope", c.scope)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("transfero baas token: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("transfero baas token %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}

	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"` // seconds
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return "", fmt.Errorf("transfero baas token decode: %w", err)
	}

	c.accessToken = tok.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return c.accessToken, nil
}

func (c *BaasClient) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	token, err := c.getAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return c.http.Do(req)
}

// ─── PIX payment group ─────────────────────────────────────────────────────

// PIXPayee describes a single PIX recipient in a payment group.
type PIXPayee struct {
	Amount       float64 `json:"amount"`
	Currency     string  `json:"currency"`
	Name         string  `json:"name"`
	TaxIDCountry string  `json:"taxIdCountry"` // ISO 3166-1 alpha-3, e.g. "BRA"
	TaxID        string  `json:"taxId"`
	PixKey       string  `json:"pixKey"`
	Description  string  `json:"description,omitempty"`
}

// PaymentGroupResult is the subset of the Transfero payment-group response we care about.
type PaymentGroupResult struct {
	PaymentGroupID string `json:"paymentGroupId"`
}

// SendPIX submits a single PIX payout from fromAccountID to pixKey for the given amount.
// Returns the Transfero paymentGroupId so it can be stored for audit/reconciliation.
func (c *BaasClient) SendPIX(
	ctx context.Context,
	fromAccountID string,
	payee PIXPayee,
) (string, error) {
	items := []PIXPayee{payee}
	payload, err := json.Marshal(items)
	if err != nil {
		return "", err
	}

	path := fmt.Sprintf("/api/v2.0/accounts/%s/paymentgroup", fromAccountID)
	resp, err := c.do(ctx, http.MethodPost, path, strings.NewReader(string(payload)), "application/json")
	if err != nil {
		return "", fmt.Errorf("transfero baas send pix: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("transfero baas send pix %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}

	var result PaymentGroupResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("transfero baas send pix decode: %w", err)
	}
	return result.PaymentGroupID, nil
}
