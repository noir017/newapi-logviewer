package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	LogDir       string
	NewAPIURL    string
	Addr         string
	Base         string
	AuthMode     string
	RequireAdmin bool
	CacheTTL     time.Duration
	AuthTTL      time.Duration
	LimitBytes   int64
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	v := strings.ToLower(os.Getenv(k))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func envFloat(k string, def float64) float64 {
	if f, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return f
	}
	return def
}

func envInt64(k string, def int64) int64 {
	if n, err := strconv.ParseInt(os.Getenv(k), 10, 64); err == nil {
		return n
	}
	return def
}

func loadConfig() Config {
	base := env("BASE_PATH", "/logviewer")
	base = "/" + strings.Trim(base, "/")
	if base == "/" {
		base = ""
	}
	return Config{
		LogDir:       env("LOG_DIR", "/logs"),
		NewAPIURL:    env("NEWAPI_URL", "http://new-api:3000"),
		Addr:         ":" + env("PORT", "7070"),
		Base:         base,
		AuthMode:     strings.ToLower(env("AUTH_MODE", "none")),
		RequireAdmin: envBool("REQUIRE_ADMIN", true),
		CacheTTL:     time.Duration(envFloat("CACHE_TTL", 3) * float64(time.Second)),
		AuthTTL:      time.Duration(envFloat("AUTH_TTL", 120) * float64(time.Second)),
		// tail bound per file; MB in, bytes out
		LimitBytes: envInt64("LIMIT_MB", 40) * 1024 * 1024,
	}
}
