// Package auth resolves Dinaria API keys to accounts using the dinapay
// api_keys table. This allows the on-ramp to share the same key scheme as
// dinapay without duplicating key management.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Account holds the resolved account for an authenticated request.
// MerchantID is set only when the API key is merchant-scoped.
type Account struct {
	ID         string // account slug, e.g. "bpn"
	Name       string
	MerchantID string // empty for account-level keys
}

type contextKey string

const accountKey contextKey = "account"

// WithAccount stores the resolved account in the context.
func WithAccount(ctx context.Context, a Account) context.Context {
	return context.WithValue(ctx, accountKey, a)
}

// FromContext retrieves the resolved account from the request context.
// Returns (zero, false) if no account has been stored.
func FromContext(ctx context.Context) (Account, bool) {
	a, ok := ctx.Value(accountKey).(Account)
	return a, ok
}

// hashKey returns the SHA-256 hex of a raw API key (matching dinapay's scheme).
func hashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ResolveAPIKey looks up a raw Bearer token in the dinapay api_keys table and
// returns the associated account. Returns (zero, false) when not found, revoked,
// or when db is nil (graceful degradation to static keys).
func ResolveAPIKey(ctx context.Context, db *pgxpool.Pool, raw string) (Account, bool) {
	if raw == "" || db == nil {
		return Account{}, false
	}
	hash := hashKey(strings.TrimSpace(raw))

	var a Account
	var merchantID *string
	err := db.QueryRow(ctx, `
		SELECT a.id, a.name, k.merchant_id
		FROM api_keys k
		JOIN accounts a ON a.id = k.account_id
		WHERE k.key_hash = $1
		  AND k.revoked_at IS NULL
		  AND a.status = 'active'
	`, hash).Scan(&a.ID, &a.Name, &merchantID)
	if err != nil {
		return Account{}, false
	}
	if merchantID != nil {
		a.MerchantID = *merchantID
	}
	return a, true
}
