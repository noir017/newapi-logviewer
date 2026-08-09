package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	LogDir       string
	ArchiveDir   string
	NewAPIURL    string
	Addr         string
	Base         string
	AuthMode     string
	RequireAdmin bool
	AuthTTL      time.Duration

	// Spool retention. LogDir is a transient buffer (expected to be tmpfs);
	// once a call is folded into the archive its raw lines have no further use.
	SpoolKeep     time.Duration
	SpoolMaxBytes int64
	IngestEvery   time.Duration

	// SearchDays bounds how far back a full-text search inflates bodies.
	// Listing and index-only filters are not affected.
	SearchDays int
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
		ArchiveDir:   env("ARCHIVE_DIR", "/archive"),
		NewAPIURL:    env("NEWAPI_URL", "http://new-api:3000"),
		Addr:         ":" + env("PORT", "7070"),
		Base:         base,
		AuthMode:     strings.ToLower(env("AUTH_MODE", "none")),
		RequireAdmin: envBool("REQUIRE_ADMIN", true),
		AuthTTL:      time.Duration(envFloat("AUTH_TTL", 120) * float64(time.Second)),
		// One hour of spool covers any single agent session, so a restart can
		// only lose calls that were still in flight.
		SpoolKeep:     time.Duration(envFloat("SPOOL_KEEP_MIN", 60) * float64(time.Minute)),
		SpoolMaxBytes: envInt64("SPOOL_MAX_MB", 256) * 1024 * 1024,
		IngestEvery:   time.Duration(envFloat("INGEST_EVERY_SEC", 2) * float64(time.Second)),
		SearchDays:    int(envInt64("SEARCH_DAYS", 7)),
	}
}
