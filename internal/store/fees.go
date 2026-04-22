package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OnrampFee represents a fee row for a given account + currency pair.
type OnrampFee struct {
	AccountID    string
	FromCurrency string
	ToCurrency   string
	FeePct       float64 // e.g. 0.002 = 0.2%
}

// FeeStore handles persistence for onramp_fees.
type FeeStore struct {
	pool *pgxpool.Pool
}

// NewFeeStore creates a FeeStore backed by the given connection pool.
func NewFeeStore(pool *pgxpool.Pool) *FeeStore {
	return &FeeStore{pool: pool}
}

// GetFee returns the currently active fee percentage for a given
// (account, fromCurrency, toCurrency) triple.
// Returns 0 (passthrough) when no row exists.
func (s *FeeStore) GetFee(ctx context.Context, accountID, fromCurrency, toCurrency string) (float64, error) {
	const sql = `
		SELECT fee_pct
		FROM onramp_fees
		WHERE account_id    = $1
		  AND from_currency = $2
		  AND to_currency   = $3
		ORDER BY effective_from DESC
		LIMIT 1`

	var feePct float64
	err := s.pool.QueryRow(ctx, sql, accountID, fromCurrency, toCurrency).Scan(&feePct)
	if err != nil {
		// No row = 0% fee (passthrough); any other error is real.
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return feePct, nil
}

// SetFee inserts a new fee row, making it the active schedule.
// Old rows are preserved for audit history.
func (s *FeeStore) SetFee(ctx context.Context, f OnrampFee) error {
	const sql = `
		INSERT INTO onramp_fees (account_id, from_currency, to_currency, fee_pct)
		VALUES ($1, $2, $3, $4)`
	_, err := s.pool.Exec(ctx, sql, f.AccountID, f.FromCurrency, f.ToCurrency, f.FeePct)
	return err
}

// ListFees returns all current active fee rows (latest effective_from per pair).
func (s *FeeStore) ListFees(ctx context.Context, accountID string) ([]OnrampFee, error) {
	const sql = `
		SELECT DISTINCT ON (account_id, from_currency, to_currency)
		  account_id, from_currency, to_currency, fee_pct
		FROM onramp_fees
		WHERE account_id = $1
		ORDER BY account_id, from_currency, to_currency, effective_from DESC`

	rows, err := s.pool.Query(ctx, sql, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []OnrampFee
	for rows.Next() {
		var f OnrampFee
		if err := rows.Scan(&f.AccountID, &f.FromCurrency, &f.ToCurrency, &f.FeePct); err != nil {
			return nil, err
		}
		result = append(result, f)
	}
	return result, rows.Err()
}
