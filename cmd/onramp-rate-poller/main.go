// onramp-rate-poller opens transient Transfero discovery sessions on a fixed
// interval and logs indicative BRL→USDT prices per onramp_account_settings row.
//
// It does NOT lock quotes, persist onramp_quotes rows, or debit balances —
// same engine path as GET /v1/rates / GetIndicativeRates.
//
// Run as a separate PM2 process alongside onramp. Reuses ~/dinapay/onramp.env:
//
//   ONRAMP_DB_URL, TRANSFERO_API_URL, TRANSFERO_API_KEY (required)
//   ONRAMP_RATE_POLL_INTERVAL   — default 5m  (e.g. 10m, 300s)
//   ONRAMP_RATE_POLL_AMOUNT_BRL — default 100 (discovery session size)
//   ONRAMP_RATE_POLL_SETTLEMENT — default D0
//   ONRAMP_RATE_POLL_ACCOUNT    — single account (default bpn)
//   ONRAMP_RATE_POLL_ACCOUNTS   — optional comma list; overrides ACCOUNT
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"transfero-on-ramp/internal/config"
	"transfero-on-ramp/internal/db"
	"transfero-on-ramp/internal/service"
	"transfero-on-ramp/internal/store"
	"transfero-on-ramp/internal/transfero"
)

const (
	defaultPollInterval = 5 * time.Minute
	defaultPollAccount  = "bpn"
)

var brtLoc *time.Location

func init() {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		loc = time.FixedZone("BRT", -3*3600)
	}
	brtLoc = loc
}

func brtNow() string {
	return time.Now().In(brtLoc).Format("2006-01-02 15:04:05 BRT")
}

func newPollerLogger() *slog.Logger {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// Drop default UTC timestamp — each line carries brtTime instead.
			if a.Key == slog.TimeKey && len(groups) == 0 {
				return slog.Attr{}
			}
			return a
		},
	})
	return slog.New(handler)
}

func main() {
	_ = godotenv.Load()

	log := newPollerLogger()
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg := config.Load()
	interval := envDuration("ONRAMP_RATE_POLL_INTERVAL", defaultPollInterval)
	amountBRL := envFloat("ONRAMP_RATE_POLL_AMOUNT_BRL", service.IndicativeRatesAmount)
	settlement := strings.ToUpper(strings.TrimSpace(os.Getenv("ONRAMP_RATE_POLL_SETTLEMENT")))
	if settlement == "" {
		settlement = "D0"
	}
	accounts := resolveAccountList()

	if cfg.DBURL == "" {
		return errString("ONRAMP_DB_URL is required")
	}
	if cfg.TransferoAPIKey == "" {
		return errString("TRANSFERO_API_KEY is required")
	}

	ctx := context.Background()
	pool, err := db.NewPool(ctx, cfg.DBURL)
	if err != nil {
		return errWrap("connect onramp DB", err)
	}
	defer pool.Close()

	if err := db.EnsureSchema(ctx, pool); err != nil {
		return errWrap("ensure schema", err)
	}

	tc := transfero.New(cfg.TransferoURL, cfg.TransferoAPIKey)
	settingsStore := store.NewSettingsStore(pool)
	// Discard service logs — poller emits one line per account only.
	svcLog := slog.New(slog.NewJSONHandler(io.Discard, nil))
	svc := service.NewOnRampService(tc, nil, nil, nil, nil, settingsStore, svcLog)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// First tick immediately so deploy smoke is instant.
	pollOnce(ctx, log, svc, accounts, amountBRL, settlement)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			pollOnce(ctx, log, svc, accounts, amountBRL, settlement)
		}
	}
}

func pollOnce(ctx context.Context, log *slog.Logger, svc *service.OnRampService, accounts []string, amountBRL float64, settlement string) {
	for _, accountID := range accounts {
		rates, err := svc.GetIndicativeRates(ctx, accountID, settlement, amountBRL)
		if err != nil {
			continue
		}
		log.Info("onramp.rate_poll",
			"brtTime", brtNow(),
			"account", accountID,
			"customerPrice", round4(rates.Price),
		)
	}
}

func round4(v float64) float64 {
	return math.Round(v*10000) / 10000
}

func resolveAccountList() []string {
	if multi := splitCSV(os.Getenv("ONRAMP_RATE_POLL_ACCOUNTS")); len(multi) > 0 {
		return multi
	}
	if single := strings.TrimSpace(os.Getenv("ONRAMP_RATE_POLL_ACCOUNT")); single != "" {
		return []string{single}
	}
	return []string{defaultPollAccount}
}

func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	if d < 30*time.Second {
		return 30 * time.Second
	}
	return d
}

func envFloat(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	var f float64
	if _, err := fmt.Sscanf(v, "%f", &f); err != nil || f <= 0 {
		return def
	}
	return f
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

type stringErr string

func (e stringErr) Error() string { return string(e) }

func errString(msg string) error { return stringErr(msg) }

func errWrap(msg string, err error) error {
	return stringErr(msg + ": " + err.Error())
}
