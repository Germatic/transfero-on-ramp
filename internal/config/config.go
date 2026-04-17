package config

import (
	"os"
	"strings"
)

type Config struct {
	Port            string
	DBURL           string   // own postgres (onramp_quotes, onramp_orders)
	DinapayDBURL    string   // dinapay postgres — used only to resolve API keys
	DinacoreURL     string
	DinacoreAPIKey  string
	TransferoURL    string
	TransferoAPIKey string
	APIKeys         []string // legacy static keys (fallback when dinapay DB unavailable)

	// Transfero BaaS — used to send PIX to the OTC desk after a trade is booked
	BaasURL          string // https://api-baasic.transfero.com
	BaasTokenURL     string // Azure AD token endpoint
	BaasClientID     string
	BaasClientSecret string
	BaasScope        string
	BaasAccountID    string // source account (2133 — the collector)

	// OTC desk PIX key to send BRL to after CloseSession
	OTCPixKey string // e.g. 6899becc-e5f6-4b80-902f-3b3dee23e468
	OTCTaxID  string // Transfero CNPJ (optional — leave empty to omit)
}

func Load() Config {
	return Config{
		Port:            getEnv("ONRAMP_PORT", "8094"),
		DBURL:           getEnv("ONRAMP_DB_URL", "postgres://localhost/onramp?sslmode=disable"),
		DinapayDBURL:    getEnv("DINAPAY_DB_URL", ""),
		DinacoreURL:     getEnv("DINACORE_URL", "http://localhost:8093"),
		DinacoreAPIKey:  getEnv("DINACORE_API_KEY", ""),
		TransferoURL:    getEnv("TRANSFERO_API_URL", "https://staging.otc.transfero.com"),
		TransferoAPIKey: getEnv("TRANSFERO_API_KEY", ""),
		APIKeys:         splitKeys(getEnv("ONRAMP_API_KEYS", "")),

		BaasURL:          getEnv("TRANSFERO_BAAS_URL", "https://api-baasic.transfero.com"),
		BaasTokenURL:     getEnv("TRANSFERO_BAAS_TOKEN_URL", ""),
		BaasClientID:     getEnv("TRANSFERO_BAAS_CLIENT_ID", ""),
		BaasClientSecret: getEnv("TRANSFERO_BAAS_CLIENT_SECRET", ""),
		BaasScope:        getEnv("TRANSFERO_BAAS_SCOPE", ""),
		BaasAccountID:    getEnv("TRANSFERO_BAAS_ACCOUNT_ID", "2133"),

		OTCPixKey: getEnv("TRANSFERO_OTC_PIX_KEY", "6899becc-e5f6-4b80-902f-3b3dee23e468"),
		OTCTaxID:  getEnv("TRANSFERO_OTC_TAX_ID", ""),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitKeys(s string) []string {
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
