package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Order mirrors a row in onramp_orders.
type Order struct {
	ID                 string
	AccountID          string
	QuoteID            string
	TransferoClosingID string
	OID                string // idempotency key
	BRLAmount          float64
	USDTAmount         float64
	Price              float64
	Settlement         string
	DestinationAddress string
	Network            string
	Status             string // confirmed | delivering | delivered | failed
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// OrderStore handles persistence for confirmed on-ramp orders.
type OrderStore struct {
	pool *pgxpool.Pool
}

// NewOrderStore creates an OrderStore backed by the given connection pool.
func NewOrderStore(pool *pgxpool.Pool) *OrderStore {
	return &OrderStore{pool: pool}
}

// Insert persists a new confirmed order and returns the generated UUID.
func (s *OrderStore) Insert(ctx context.Context, o Order) (string, error) {
	const sql = `
		INSERT INTO onramp_orders
			(account_id, quote_id, transfero_closing_id, oid,
			 brl_amount, usdt_amount, price, settlement,
			 destination_address, network)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id`

	var id string
	err := s.pool.QueryRow(ctx, sql,
		o.AccountID,
		o.QuoteID,
		o.TransferoClosingID,
		o.OID,
		o.BRLAmount,
		o.USDTAmount,
		o.Price,
		o.Settlement,
		o.DestinationAddress,
		o.Network,
	).Scan(&id)
	return id, err
}

// Get retrieves an order by ID. Returns ErrNotFound if absent.
func (s *OrderStore) Get(ctx context.Context, id string) (Order, error) {
	const sql = `
		SELECT id, account_id, quote_id, transfero_closing_id, oid,
		       brl_amount, usdt_amount, price, settlement,
		       destination_address, network, status, created_at, updated_at
		FROM onramp_orders WHERE id = $1`

	var o Order
	err := s.pool.QueryRow(ctx, sql, id).Scan(
		&o.ID,
		&o.AccountID,
		&o.QuoteID,
		&o.TransferoClosingID,
		&o.OID,
		&o.BRLAmount,
		&o.USDTAmount,
		&o.Price,
		&o.Settlement,
		&o.DestinationAddress,
		&o.Network,
		&o.Status,
		&o.CreatedAt,
		&o.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	return o, err
}

// UpdateStatus sets the order status and bumps updated_at.
func (s *OrderStore) UpdateStatus(ctx context.Context, id, status string) error {
	const sql = `UPDATE onramp_orders SET status = $2, updated_at = now() WHERE id = $1`
	_, err := s.pool.Exec(ctx, sql, id, status)
	return err
}

// List returns a page of orders, newest first.
func (s *OrderStore) List(ctx context.Context, page, pageSize int) ([]Order, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 200 {
		pageSize = 50
	}
	offset := (page - 1) * pageSize

	// Count total for pagination metadata
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM onramp_orders`).Scan(&total); err != nil {
		return nil, 0, err
	}

	const sql = `
		SELECT id, account_id, quote_id, transfero_closing_id, oid,
		       brl_amount, usdt_amount, price, settlement,
		       destination_address, network, status, created_at, updated_at
		FROM onramp_orders
		ORDER BY created_at DESC
		LIMIT $1 OFFSET $2`

	rows, err := s.pool.Query(ctx, sql, pageSize, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var orders []Order
	for rows.Next() {
		var o Order
		if err := rows.Scan(
			&o.ID,
			&o.AccountID,
			&o.QuoteID,
			&o.TransferoClosingID,
			&o.OID,
			&o.BRLAmount,
			&o.USDTAmount,
			&o.Price,
			&o.Settlement,
			&o.DestinationAddress,
			&o.Network,
			&o.Status,
			&o.CreatedAt,
			&o.UpdatedAt,
		); err != nil {
			return nil, 0, err
		}
		orders = append(orders, o)
	}
	return orders, total, rows.Err()
}
