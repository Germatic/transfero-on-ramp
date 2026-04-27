package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoAccountSettings is returned when no settings row exists for an account.
var ErrNoAccountSettings = errors.New("no onramp settings configured for account")

// AccountSettings holds per-account on-ramp configuration.
type AccountSettings struct {
	AccountID     string
	SpotMarkupPct float64 // e.g. 0.36 = 0.36 %
	Description   string
}

// SettingsStore handles persistence for onramp_account_settings.
type SettingsStore struct {
	pool *pgxpool.Pool
}

// NewSettingsStore creates a SettingsStore backed by the given connection pool.
func NewSettingsStore(pool *pgxpool.Pool) *SettingsStore {
	return &SettingsStore{pool: pool}
}

// GetSettings returns the settings for the given account.
// Returns ErrNoAccountSettings when no row exists.
func (s *SettingsStore) GetSettings(ctx context.Context, accountID string) (AccountSettings, error) {
	const sql = `
		SELECT account_id, spot_markup_pct, COALESCE(description, '')
		FROM onramp_account_settings
		WHERE account_id = $1`

	var as AccountSettings
	err := s.pool.QueryRow(ctx, sql, accountID).Scan(&as.AccountID, &as.SpotMarkupPct, &as.Description)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AccountSettings{}, ErrNoAccountSettings
		}
		return AccountSettings{}, err
	}
	return as, nil
}

// SetSettings upserts the settings for an account.
func (s *SettingsStore) SetSettings(ctx context.Context, as AccountSettings) error {
	const sql = `
		INSERT INTO onramp_account_settings (account_id, spot_markup_pct, description, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (account_id) DO UPDATE
		  SET spot_markup_pct = EXCLUDED.spot_markup_pct,
		      description     = EXCLUDED.description,
		      updated_at      = now()`
	_, err := s.pool.Exec(ctx, sql, as.AccountID, as.SpotMarkupPct, as.Description)
	return err
}
