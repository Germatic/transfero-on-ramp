// cmd/reconciler is a standalone process that polls onramp_orders in
// "awaiting_settlement" status and confirms on-chain USDT delivery via
// the Tronscan public API. When a matching confirmed TRC20 transfer is found,
// the order is transitioned to "delivered" and the tx hash is stored.
//
// Run as a separate PM2 process alongside the onramp service.
// Required env vars: same ONRAMP_DB_URL as the main service.
// Optional: TRONSCAN_API_KEY (increases rate limits).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"transfero-on-ramp/internal/db"
	"transfero-on-ramp/internal/store"
	"transfero-on-ramp/internal/tronscan"
)

const (
	// pollInterval is how often the reconciler checks for unconfirmed orders.
	pollInterval = 2 * time.Minute

	// minOrderAge is the minimum time an order must be in awaiting_settlement
	// before the reconciler starts checking Tronscan. Transfero typically
	// broadcasts within seconds but can take a few minutes.
	minOrderAge = 2 * time.Minute

	// giveUpAfter is how long we try before flagging an order for manual review.
	// Transfero's settlement SLA is same-day for D0; 48h is a safe outer bound.
	giveUpAfter = 48 * time.Hour

	// batchSize is the number of orders to check per poll cycle.
	batchSize = 50
)

func main() {
	_ = godotenv.Load()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	dbURL := os.Getenv("ONRAMP_DB_URL")
	if dbURL == "" {
		log.Error("ONRAMP_DB_URL is required")
		os.Exit(1)
	}

	pool, err := db.NewPool(context.Background(), dbURL)
	if err != nil {
		log.Error("connect DB", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Schema migration — adds tx_hash / delivered_at columns if not present.
	if err := db.EnsureSchema(context.Background(), pool); err != nil {
		log.Error("ensure schema", "err", err)
		os.Exit(1)
	}

	orderStore := store.NewOrderStore(pool)
	tron := tronscan.New(os.Getenv("TRONSCAN_API_KEY"))

	log.Info("settlement reconciler started", "pollInterval", pollInterval.String(), "minOrderAge", minOrderAge.String())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Run immediately on startup, then on each tick.
	runCycle(ctx, log, orderStore, tron)
	for {
		select {
		case <-ctx.Done():
			log.Info("reconciler shutting down")
			return
		case <-ticker.C:
			runCycle(ctx, log, orderStore, tron)
		}
	}
}

func runCycle(ctx context.Context, log *slog.Logger, orders *store.OrderStore, tron *tronscan.Client) {
	pending, err := orders.ListAwaitingSettlement(ctx, minOrderAge, batchSize)
	if err != nil {
		log.Warn("list awaiting_settlement failed", "err", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	log.Info("reconciling orders", "count", len(pending))

	for _, o := range pending {
		if ctx.Err() != nil {
			return
		}
		checkOrder(ctx, log, orders, tron, o)
		// Small pause between Tronscan calls to stay within rate limits.
		time.Sleep(500 * time.Millisecond)
	}
}

func checkOrder(ctx context.Context, log *slog.Logger, orders *store.OrderStore, tron *tronscan.Client, o store.Order) {
	log = log.With("order_id", o.ID, "address", o.DestinationAddress, "usdt", o.USDTAmount)

	// Flag orders that have been waiting too long.
	if time.Since(o.CreatedAt) > giveUpAfter {
		log.Error("order exceeded settlement timeout — manual review required",
			"age", time.Since(o.CreatedAt).String())
		// Transition to a distinct status so ops can identify it.
		_ = orders.UpdateStatus(ctx, o.ID, "settlement_timeout")
		return
	}

	tx, found, err := tron.FindInboundUSDT(ctx, o.DestinationAddress, o.USDTAmount, o.CreatedAt)
	if err != nil {
		log.Warn("tronscan lookup failed", "err", err)
		return
	}

	if !found {
		log.Info("USDT not yet confirmed on-chain", "age", time.Since(o.CreatedAt).Round(time.Second).String())
		return
	}

	if err := orders.MarkDelivered(ctx, o.ID, tx.TxHash); err != nil {
		log.Error("MarkDelivered failed", "tx_hash", tx.TxHash, "err", err)
		return
	}

	log.Info("order delivered on-chain",
		"tx_hash", tx.TxHash,
		"on_chain_amount", tx.Amount,
		"block_time", tx.BlockTime.Format(time.RFC3339),
	)
}
