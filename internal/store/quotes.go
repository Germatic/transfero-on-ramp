package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// ErrQuoteExpired is returned when a quote's session window has passed.
var ErrQuoteExpired = errors.New("quote expired")

// ErrQuoteUsed is returned when a quote has already been confirmed.
var ErrQuoteUsed = errors.New("quote already used")

// Quote mirrors a row in onramp_quotes.
type Quote struct {
	ID                  string
	AccountID           string
	TransferoSessionID  string
	BRLAmount           float64
	USDTAmount          float64
	Price               float64
	Settlement          string
	DestinationAddress  string
	Network             string
	Status              string // open | used | expired
	ExpiresAt           time.Time
	CreatedAt           time.Time
}

// QuoteStore handles persistence for locked Transfero quote sessions.
type QuoteStore struct {
	pool *pgxpool.Pool
}

// NewQuoteStore creates a QuoteStore backed by the given connection pool.
func NewQuoteStore(pool *pgxpool.Pool) *QuoteStore {
	return &QuoteStore{pool: pool}
}

// Insert persists a new quote and returns the generated UUID.
func (s *QuoteStore) Insert(ctx context.Context, q Quote) (string, error) {
	const sql = `
		INSERT INTO onramp_quotes
			(account_id, transfero_session_id, brl_amount, usdt_amount, price,
			 settlement, destination_address, network, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id`

	var id string
	err := s.pool.QueryRow(ctx, sql,
		q.AccountID,
		q.TransferoSessionID,
		q.BRLAmount,
		q.USDTAmount,
		q.Price,
		q.Settlement,
		q.DestinationAddress,
		q.Network,
		q.ExpiresAt,
	).Scan(&id)
	return id, err
}

// Get retrieves a quote by ID. Returns ErrNotFound if absent.
func (s *QuoteStore) Get(ctx context.Context, id string) (Quote, error) {
	const sql = `
		SELECT id, account_id, transfero_session_id, brl_amount, usdt_amount, price,
		       settlement, destination_address, network, status, expires_at, created_at
		FROM onramp_quotes WHERE id = $1`

	var q Quote
	err := s.pool.QueryRow(ctx, sql, id).Scan(
		&q.ID,
		&q.AccountID,
		&q.TransferoSessionID,
		&q.BRLAmount,
		&q.USDTAmount,
		&q.Price,
		&q.Settlement,
		&q.DestinationAddress,
		&q.Network,
		&q.Status,
		&q.ExpiresAt,
		&q.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Quote{}, ErrNotFound
	}
	return q, err
}

// MarkUsed atomically transitions a quote from "open" to "used".
// Returns ErrNotFound, ErrQuoteExpired, or ErrQuoteUsed on failure.
func (s *QuoteStore) MarkUsed(ctx context.Context, id string) error {
	// First load to give a clear error for expired/used states
	q, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if q.Status == "used" {
		return ErrQuoteUsed
	}
	if q.Status == "expired" || time.Now().After(q.ExpiresAt) {
		return ErrQuoteExpired
	}

	const sql = `UPDATE onramp_quotes SET status = 'used' WHERE id = $1 AND status = 'open'`
	tag, err := s.pool.Exec(ctx, sql, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Race: another request beat us to it
		return ErrQuoteUsed
	}
	return nil
}

// ExpireStale marks all open quotes whose expiry has passed as "expired".
// Intended to be called periodically by a background goroutine.
func (s *QuoteStore) ExpireStale(ctx context.Context) error {
	const sql = `UPDATE onramp_quotes SET status = 'expired' WHERE status = 'open' AND expires_at < now()`
	_, err := s.pool.Exec(ctx, sql)
	return err
}
