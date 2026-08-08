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
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           newServer(cfg),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.SetOutput(os.Stdout)
	log.SetFlags(0)
	log.Printf("log viewer on %s base=%s logs=%s newapi=%s auth=%s",
		cfg.Addr, cfg.Base, cfg.LogDir, cfg.NewAPIURL, cfg.AuthMode)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
