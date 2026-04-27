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
	ID                  string
	AccountID           string
	QuoteID             string
	TransferoClosingID  string
	OID                 string  // idempotency key
	BRLAmount           float64
	USDTAmount          float64
	Price               float64 // adjusted price (= RawPrice * (1 + FeePct))
	RawPrice            float64 // Transfero's original price before markup
	FeePct              float64 // markup applied, e.g. 0.002 = 0.2%
	Settlement          string
	DestinationAddress  string
	Network             string
	Status              string     // awaiting_settlement | confirmed | delivering | delivered | failed | payment_failed
	PixPaymentGroupID   string     // Transfero paymentGroupId for the BRL PIX sent to OTC desk
	PayoutID            string     // dinapay payout row ID — used by reconciler to mark payout completed
	TxHash              string     // on-chain Tron tx hash, set by settlement reconciler
	DeliveredAt         *time.Time // timestamp of on-chain delivery confirmation
	CreatedAt           time.Time
	UpdatedAt           time.Time
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
			 brl_amount, usdt_amount, price, raw_price, fee_pct,
			 settlement, destination_address, network, status, pix_payment_group_id, payout_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING id`

	status := o.Status
	if status == "" {
		status = "awaiting_settlement"
	}
	var pixGroupID *string
	if o.PixPaymentGroupID != "" {
		pixGroupID = &o.PixPaymentGroupID
	}
	var payoutID *string
	if o.PayoutID != "" {
		payoutID = &o.PayoutID
	}
	rawPrice := o.RawPrice
	if rawPrice == 0 {
		rawPrice = o.Price
	}

	var id string
	err := s.pool.QueryRow(ctx, sql,
		o.AccountID,
		o.QuoteID,
		o.TransferoClosingID,
		o.OID,
		o.BRLAmount,
		o.USDTAmount,
		o.Price,
		rawPrice,
		o.FeePct,
		o.Settlement,
		o.DestinationAddress,
		o.Network,
		status,
		pixGroupID,
		payoutID,
	).Scan(&id)
	return id, err
}

// Get retrieves an order by ID. Returns ErrNotFound if absent.
func (s *OrderStore) Get(ctx context.Context, id string) (Order, error) {
	const sql = `
		SELECT id, account_id, quote_id, transfero_closing_id, oid,
		       brl_amount, usdt_amount, price, raw_price, fee_pct,
		       settlement, destination_address, network, status, pix_payment_group_id,
		       created_at, updated_at
		FROM onramp_orders WHERE id = $1`

	var o Order
	var pixGroupID *string
	var rawPrice *float64
	err := s.pool.QueryRow(ctx, sql, id).Scan(
		&o.ID,
		&o.AccountID,
		&o.QuoteID,
		&o.TransferoClosingID,
		&o.OID,
		&o.BRLAmount,
		&o.USDTAmount,
		&o.Price,
		&rawPrice,
		&o.FeePct,
		&o.Settlement,
		&o.DestinationAddress,
		&o.Network,
		&o.Status,
		&pixGroupID,
		&o.CreatedAt,
		&o.UpdatedAt,
	)
	if pixGroupID != nil {
		o.PixPaymentGroupID = *pixGroupID
	}
	if rawPrice != nil {
		o.RawPrice = *rawPrice
	}
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

// MarkDelivered transitions an order to "delivered" and records the on-chain tx hash.
func (s *OrderStore) MarkDelivered(ctx context.Context, id, txHash string) error {
	const sql = `
		UPDATE onramp_orders
		SET status       = 'delivered',
		    tx_hash      = $2,
		    delivered_at = now(),
		    updated_at   = now()
		WHERE id = $1 AND status = 'awaiting_settlement'`
	_, err := s.pool.Exec(ctx, sql, id, txHash)
	return err
}

// ListAwaitingSettlement returns orders stuck in awaiting_settlement that are
// older than minAge and have a destination_address set (on-chain delivery).
func (s *OrderStore) ListAwaitingSettlement(ctx context.Context, minAge time.Duration, limit int) ([]Order, error) {
	const sql = `
		SELECT id, account_id, usdt_amount, destination_address, network, created_at,
		       COALESCE(payout_id, '')
		FROM onramp_orders
		WHERE status = 'awaiting_settlement'
		  AND destination_address <> ''
		  AND created_at < now() - $1::interval
		ORDER BY created_at ASC
		LIMIT $2`

	rows, err := s.pool.Query(ctx, sql, minAge.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var orders []Order
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.AccountID, &o.USDTAmount, &o.DestinationAddress, &o.Network, &o.CreatedAt, &o.PayoutID); err != nil {
			return nil, err
		}
		orders = append(orders, o)
	}
	return orders, rows.Err()
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
		       brl_amount, usdt_amount, price, raw_price, fee_pct,
		       settlement, destination_address, network, status, pix_payment_group_id,
		       created_at, updated_at
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
		var pixGroupID *string
		var rawPrice *float64
		if err := rows.Scan(
			&o.ID,
			&o.AccountID,
			&o.QuoteID,
			&o.TransferoClosingID,
			&o.OID,
			&o.BRLAmount,
			&o.USDTAmount,
			&o.Price,
			&rawPrice,
			&o.FeePct,
			&o.Settlement,
			&o.DestinationAddress,
			&o.Network,
			&o.Status,
			&pixGroupID,
			&o.CreatedAt,
			&o.UpdatedAt,
		); err != nil {
			return nil, 0, err
		}
		if pixGroupID != nil {
			o.PixPaymentGroupID = *pixGroupID
		}
		if rawPrice != nil {
			o.RawPrice = *rawPrice
		}
		orders = append(orders, o)
	}
	return orders, total, rows.Err()
}
