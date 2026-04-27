package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SettingsStore handles persistence for onramp_account_settings.
type SettingsStore struct {
	pool *pgxpool.Pool
}

// NewSettingsStore creates a SettingsStore backed by the given connection pool.
func NewSettingsStore(pool *pgxpool.Pool) *SettingsStore {
	return &SettingsStore{pool: pool}
}

// GetMaxD0PremiumPct returns the maximum allowed D0-over-spot premium for the
// given account, expressed as a percentage (e.g. 0.036 = 0.036%).
// Returns nil when no row exists (meaning no guard is applied).
func (s *SettingsStore) GetMaxD0PremiumPct(ctx context.Context, accountID string) (*float64, error) {
	const sql = `
		SELECT max_d0_premium_pct
		FROM onramp_account_settings
		WHERE account_id = $1`

	var pct *float64
	err := s.pool.QueryRow(ctx, sql, accountID).Scan(&pct)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return pct, nil
}

// SetMaxD0PremiumPct upserts the max_d0_premium_pct for an account.
// Pass nil to disable the guard.
func (s *SettingsStore) SetMaxD0PremiumPct(ctx context.Context, accountID string, pct *float64) error {
	const sql = `
		INSERT INTO onramp_account_settings (account_id, max_d0_premium_pct, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (account_id) DO UPDATE
		  SET max_d0_premium_pct = EXCLUDED.max_d0_premium_pct,
		      updated_at         = now()`
	_, err := s.pool.Exec(ctx, sql, accountID, pct)
	return err
}
