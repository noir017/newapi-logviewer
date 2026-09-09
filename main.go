package main

import (
	_ "embed"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
	// Timestamps in the log are wall-clock local time, and the UI's time filter
	// sends epoch seconds - so the process must be able to resolve $TZ. A
	// scratch image has no /usr/share/zoneinfo, so carry the database in the
	// binary (~450KB) rather than depending on the base image.
	_ "time/tzdata"
)

// The UI is a single file baked into the binary: the container ships one
// static binary and nothing else, so there is no static-file mount to keep
// in sync and nothing to serve off disk.
//
//go:embed ui.html
var indexHTML []byte

// The stats view's charts. Vendored rather than fetched: the container is a
// scratch image on an air-gapped LAN, so a CDN <script> would simply fail to
// load. Kept a separate asset (not inlined into ui.html) so it carries an
// immutable cache header and does not bloat every page load of the list view.
// Not under vendor/, which `go mod vendor` owns and would wipe.
//
//go:embed assets/chart.umd.min.js
var chartJS []byte

func main() {
	cfg := loadConfig()
	log.SetOutput(os.Stdout)
	log.SetFlags(0)

	// Maintenance mode: apply a parser fix to already-archived records and
	// exit, without starting a server.
	if runReingest(cfg) {
		return
	}
	// Maintenance mode: rebuild the derived index after a new list column.
	if runReindex(cfg) {
		return
	}
	// Maintenance mode: recompute fields a parser bug damaged, in place. Unlike
	// the two above this needs neither the raw logs nor a scratch archive - the
	// underlying data survived and only the arithmetic over it was wrong.
	if runRepair(cfg) {
		return
	}
	// Maintenance mode: merge another pod's archive into this one, for the
	// history that predates push mode.
	if runImport(cfg) {
		return
	}

	handler := newServer(cfg)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// The ingester is the only thing that reads New API's raw log. It folds
	// finished calls into the archive and lets the spool be discarded, which is
	// what keeps the streaming chunks - 76% of log volume - off disk entirely.
	go handler.ing.Run(cfg.IngestEvery)

	log.Printf("log viewer on %s base=%s spool=%s archive=%s auth=%s",
		cfg.Addr, cfg.Base, cfg.LogDir, cfg.ArchiveDir, cfg.AuthMode)
	// Which half of push mode this process is, said once at startup. Both are
	// silent when they are working, and "the other pod's calls are missing" is
	// otherwise impossible to diagnose from the log.
	if cfg.PushURL != "" {
		log.Printf("push mode: records are pushed to %s as pod %q, not archived locally",
			cfg.PushURL, cfg.PodName)
		if cfg.PushToken == "" {
			log.Printf("warning: PUSH_URL is set but PUSH_TOKEN is empty; the receiver will reject every push")
		} else if strings.HasPrefix(cfg.PushURL, "http://") {
			// The token is a bearer credential in a header. Worth one line at
			// startup: the deployment this exists for reaches the other pod
			// over the public internet, where http:// hands the token to
			// anything on the path.
			log.Printf("warning: PUSH_URL is http://, so PUSH_TOKEN and every prompt in the batch cross the network in the clear")
		}
	}
	if cfg.PushToken != "" {
		log.Printf("push receiver at %s%s, accepting pods %v (empty means any)",
			cfg.Base, pushPath, cfg.PushPods)
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
