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

	// NewAPIToken is an admin access token used only to resolve channel ids to
	// channel names for the list view. New API does not log the name, and the
	// upstream address cannot stand in for it when many channels share a host.
	// Optional: without it the list shows the address.
	NewAPIToken string

	// Spool retention. LogDir is a transient buffer (expected to be tmpfs);
	// once a call is folded into the archive its raw lines have no further use.
	SpoolKeep     time.Duration
	SpoolMaxBytes int64
	IngestEvery   time.Duration

	// SearchDays bounds how far back a full-text search inflates bodies.
	// Listing and index-only filters are not affected.
	SearchDays int

	// Multi-pod aggregation. One New API cluster can run as several pods behind
	// one database; each pod writes its own DEBUG log, so each viewer folds its
	// own share of the traffic and every list, total and chart is a fraction of
	// the answer. Push mode makes one pod the only writer - see push.go for the
	// sending half and receive.go for the receiving one.
	//
	// Both halves are off unless configured, and a viewer with neither set
	// behaves exactly as a single-machine install.
	//
	// PushURL turns this viewer into a SENDER: folded records go over HTTP
	// instead of into the local archive. PodName labels them.
	PushURL string
	PodName string

	// PushToken is the shared secret. On a sender it is presented with every
	// push; on a receiver it is what enables the endpoint at all - unset, the
	// receiver refuses every push rather than accepting unauthenticated writes
	// into the permanent store.
	PushToken string

	// PushPods optionally restricts which sender pod names a receiver accepts.
	// Empty accepts any named pod.
	PushPods []string

	PushTimeout time.Duration
	// PushMaxBytes bounds one pushed batch, both the compressed body and (times
	// pushInflateRatio) what it inflates to.
	PushMaxBytes int64
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

// envList reads a comma-separated variable, dropping blanks. Returns nil for an
// unset variable, which every caller reads as "no restriction".
func envList(k string) []string {
	var out []string
	for _, p := range strings.Split(os.Getenv(k), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
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
		NewAPIToken:  env("NEWAPI_TOKEN", ""),
		// One hour of spool covers any single agent session, so a restart can
		// only lose calls that were still in flight.
		SpoolKeep:     time.Duration(envFloat("SPOOL_KEEP_MIN", 60) * float64(time.Minute)),
		SpoolMaxBytes: envInt64("SPOOL_MAX_MB", 256) * 1024 * 1024,
		IngestEvery:   time.Duration(envFloat("INGEST_EVERY_SEC", 2) * float64(time.Second)),
		SearchDays:    int(envInt64("SEARCH_DAYS", 7)),

		PushURL:   pushEndpoint(env("PUSH_URL", "")),
		PodName:   podName(env("POD_NAME", "")),
		PushToken: env("PUSH_TOKEN", ""),
		PushPods:  envList("PUSH_PODS"),
		// Generous: a batch is up to 8MB of bodies over what is a WAN link in
		// the deployment this exists for, and a timeout is a failed push, which
		// stops the sender consuming its spool until it is retried.
		PushTimeout:  time.Duration(envFloat("PUSH_TIMEOUT_SEC", 60) * float64(time.Second)),
		PushMaxBytes: envInt64("PUSH_MAX_MB", 64) * 1024 * 1024,
	}
}

// podName labels which pod a record came from. It is provenance only - nothing
// keys off it - but a push carries it and the receiver requires it, so fall
// back to the hostname rather than pushing anonymously: in the deployment this
// exists for the hostname IS the pod name.
func podName(configured string) string {
	if configured != "" {
		return configured
	}
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}
