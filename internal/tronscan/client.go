// Package tronscan provides a minimal client for the Tronscan public API,
// used to verify on-chain USDT (TRC20) delivery to a Tron address.
package tronscan

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	// BaseURL is the public Tronscan API endpoint.
	BaseURL = "https://apilist.tronscanapi.com"

	// USDTContract is the TRC20 contract address for USDT on Tron mainnet.
	USDTContract = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"

	// USDTDecimals is the number of decimals for USDT TRC20.
	USDTDecimals = 6
)

// Transfer represents a single TRC20 token transfer from Tronscan.
type Transfer struct {
	TxHash      string  // transaction hash
	From        string
	To          string
	Amount      float64 // amount in human-readable units (divided by 10^decimals)
	Confirmed   bool
	BlockTime   time.Time
}

// Client is a Tronscan API client.
type Client struct {
	baseURL string
	apiKey  string // optional; increases rate limits
	http    *http.Client
}

// New creates a Tronscan client.
// apiKey may be empty — the public API works without one at lower rate limits.
func New(apiKey string) *Client {
	return &Client{
		baseURL: BaseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// FindInboundUSDT searches for a confirmed inbound USDT transfer to toAddress
// with an amount within tolerance of expectedUSDT, created after notBefore.
// Returns the matching transfer and true if found.
func (c *Client) FindInboundUSDT(ctx context.Context, toAddress string, expectedUSDT float64, notBefore time.Time) (Transfer, bool, error) {
	params := url.Values{}
	params.Set("toAddress", toAddress)
	params.Set("contract_address", USDTContract)
	params.Set("start_timestamp", strconv.FormatInt(notBefore.UnixMilli(), 10))
	params.Set("confirm", "true")
	params.Set("limit", "20")
	params.Set("start", "0")

	endpoint := c.baseURL + "/api/token_trc20/transfers?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return Transfer{}, false, err
	}
	if c.apiKey != "" {
		req.Header.Set("TRON-PRO-API-KEY", c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Transfer{}, false, fmt.Errorf("tronscan request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return Transfer{}, false, fmt.Errorf("tronscan HTTP %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Data []struct {
			Hash            string `json:"hash"`
			From            string `json:"from"`
			To              string `json:"to"`
			Amount          string `json:"amount"`       // raw integer string
			Confirmed       int    `json:"confirmed"`    // 1 = confirmed
			BlockTimestamp  int64  `json:"block_timestamp"`
			ContractRet     string `json:"contract_ret"` // "SUCCESS"
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return Transfer{}, false, fmt.Errorf("tronscan decode: %w", err)
	}

	for _, tx := range result.Data {
		if tx.Confirmed != 1 || tx.ContractRet != "SUCCESS" {
			continue
		}
		rawAmt, err := strconv.ParseInt(tx.Amount, 10, 64)
		if err != nil {
			continue
		}
		amount := float64(rawAmt) / 1e6 // USDT has 6 decimals

		// Accept if within 0.01 USDT of expected (handles dust/rounding)
		diff := amount - expectedUSDT
		if diff < 0 {
			diff = -diff
		}
		if diff <= 0.01 {
			blockTime := time.UnixMilli(tx.BlockTimestamp).UTC()
			return Transfer{
				TxHash:    tx.Hash,
				From:      tx.From,
				To:        tx.To,
				Amount:    amount,
				Confirmed: true,
				BlockTime: blockTime,
			}, true, nil
		}
	}

	return Transfer{}, false, nil
}
