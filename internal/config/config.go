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
