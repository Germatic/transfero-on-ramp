package http

import (
	"log/slog"
	"net/http"
	"strings"

	"transfero-on-ramp/internal/auth"
	"transfero-on-ramp/internal/service"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewRouter builds and returns the application's HTTP mux.
//
// The dinapayDB pool is used to resolve incoming Bearer tokens to accounts.
// It may be nil, in which case the middleware falls back to the static key list.
func NewRouter(
	svc *service.OnRampService,
	staticKeys []string,
	dinapayDB *pgxpool.Pool,
	log *slog.Logger,
) http.Handler {
	mux := http.NewServeMux()

	authMW := withAPIKey(staticKeys, dinapayDB, log)

	// Public
	mux.HandleFunc("GET /health", handleHealth)

	// Protected
	mux.Handle("POST /v1/quotes", authMW(handleCreateQuote(svc, log)))
	mux.Handle("POST /v1/orders", authMW(handleConfirmOrder(svc, log)))
	mux.Handle("GET /v1/orders", authMW(handleListOrders(svc, log)))
	mux.Handle("GET /v1/orders/{id}", authMW(handleGetOrder(svc, log)))

	return mux
}

// ─────────────────────────────────────────────────────────────────────────────
// Auth middleware
// ─────────────────────────────────────────────────────────────────────────────

// withAPIKey returns a middleware that resolves the Bearer token.
//
// Resolution order:
//  1. Look up the token hash in the dinapay api_keys table (requires dinapayDB).
//  2. Fall back to a static in-memory key set (ONRAMP_API_KEYS env var).
//
// On success the resolved Account is stored in the request context so handlers
// can retrieve it with auth.FromContext.
func withAPIKey(staticKeys []string, dinapayDB *pgxpool.Pool, log *slog.Logger) func(http.Handler) http.Handler {
	staticSet := make(map[string]struct{}, len(staticKeys))
	for _, k := range staticKeys {
		staticSet[k] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := extractBearerToken(r)
			if raw == "" {
				writeError(w, http.StatusUnauthorized, "missing Bearer token")
				return
			}

			// Try dinapay DB resolution first
			if dinapayDB != nil {
				if account, ok := auth.ResolveAPIKey(r.Context(), dinapayDB, raw); ok {
					ctx := auth.WithAccount(r.Context(), account)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}

			// Fall back to static keys — treated as an "anonymous" account
			if _, ok := staticSet[raw]; ok {
				ctx := auth.WithAccount(r.Context(), auth.Account{ID: "static", Name: "static"})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			log.Warn("unauthorized request", "remote", r.RemoteAddr, "path", r.URL.Path)
			writeError(w, http.StatusUnauthorized, "invalid API key")
		})
	}
}

// extractBearerToken extracts the raw token from the Authorization header.
// Accepts both "Bearer <token>" and a bare token.
func extractBearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	if rest, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(h)
}

