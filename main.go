package main

import (
	_ "embed"
	"log"
	"net/http"
	"os"
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
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
