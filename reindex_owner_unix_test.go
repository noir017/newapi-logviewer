//go:build unix

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The regression this pins cost two hours of silently unarchived calls: a
// reindex run as root left the .idx owned by root, and the server - running as
// nobody - failed every append with EACCES while every read kept working.
func TestReindexPreservesOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to change ownership")
	}
	dir := t.TempDir()
	arc := newArchive(dir)
	st := 200
	if err := arc.Append(&Record{
		RequestID: "20260810own", TS: "2026/08/10 10:00:00", Status: &st,
		Model: "m", Request: Raw(`{"model":"m"}`),
	}); err != nil {
		t.Fatal(err)
	}
	arc.Close()

	ip := filepath.Join(dir, "arc-20260810.idx")
	const nobody = 65534
	if err := os.Chown(ip, nobody, nobody); err != nil {
		t.Fatalf("chown fixture: %v", err)
	}

	// reindex runs as root here, exactly as it does in the one-off container.
	if _, err := reindexDay(dir, "20260810"); err != nil {
		t.Fatalf("reindex: %v", err)
	}

	fi, err := os.Stat(ip)
	if err != nil {
		t.Fatal(err)
	}
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no stat_t on this platform")
	}
	if int(sys.Uid) != nobody || int(sys.Gid) != nobody {
		t.Errorf("owner changed by reindex: %d:%d, want %d:%d",
			sys.Uid, sys.Gid, nobody, nobody)
	}
}
