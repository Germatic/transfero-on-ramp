package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/jackc/pgx/v5/pgxpool"

	"transfero-on-ramp/internal/config"
	"transfero-on-ramp/internal/db"
	"transfero-on-ramp/internal/dinacore"
	"transfero-on-ramp/internal/service"
	"transfero-on-ramp/internal/store"
	"transfero-on-ramp/internal/transfero"
	httpTransport "transfero-on-ramp/internal/transport/http"
)

func main() {
	// Load .env if present (ignored in production where env vars are set by the platform)
	_ = godotenv.Load()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg := config.Load()

	// ─── Own database (quotes + orders) ─────────────────────────────────────
	onrampPool, err := db.NewPool(context.Background(), cfg.DBURL)
	if err != nil {
		return fmt.Errorf("connect to onramp DB: %w", err)
	}
	defer onrampPool.Close()

	if err := db.EnsureSchema(context.Background(), onrampPool); err != nil {
		return fmt.Errorf("ensure schema: %w", err)
	}
	log.Info("onramp DB ready")

	// ─── Dinapay database (API key resolution) ───────────────────────────────
	var dinapayPool *pgxpool.Pool
	if cfg.DinapayDBURL != "" {
		p, err := db.NewPool(context.Background(), cfg.DinapayDBURL)
		if err != nil {
			log.Warn("dinapay DB unavailable; falling back to static keys", "err", err)
		} else {
			dinapayPool = p
			defer dinapayPool.Close()
			log.Info("dinapay DB ready")
		}
	} else {
		log.Warn("DINAPAY_DB_URL not set; API key resolution via static list only")
	}

	// ─── Transfero client ────────────────────────────────────────────────────
	tc := transfero.New(cfg.TransferoURL, cfg.TransferoAPIKey)

	// ─── DinaCore client ─────────────────────────────────────────────────────
	var dc *dinacore.Client
	if cfg.DinacoreURL != "" {
		dc = dinacore.New(cfg.DinacoreURL, cfg.DinacoreAPIKey)
		log.Info("dinacore client ready", "url", cfg.DinacoreURL)
	} else {
		log.Warn("DINACORE_URL not set; balance operations will be skipped")
	}

	// ─── Stores ──────────────────────────────────────────────────────────────
	quoteStore := store.NewQuoteStore(onrampPool)
	orderStore := store.NewOrderStore(onrampPool)

	// ─── Background: expire stale quotes ─────────────────────────────────────
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for range t.C {
			if err := quoteStore.ExpireStale(context.Background()); err != nil {
				log.Warn("expire stale quotes", "err", err)
			}
		}
	}()

	// ─── Service & router ─────────────────────────────────────────────────────
	svc := service.NewOnRampService(tc, dc, quoteStore, orderStore, log)
	router := httpTransport.NewRouter(svc, cfg.APIKeys, dinapayPool, log)

	// ─── HTTP server ──────────────────────────────────────────────────────────
	addr := ":" + cfg.Port
	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Graceful shutdown on SIGINT / SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		log.Info("shutting down", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
}
