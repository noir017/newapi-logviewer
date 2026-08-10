package main

import (
	"os"
	"path/filepath"
	"testing"
)

// reindex replaces the index by rename, so the result carries the temp file's
// permissions rather than the original's. In the deployment that matters,
// reindex runs as root against an archive the server writes as nobody - and a
// root-owned .idx breaks only writes, so reads keep working and the archive
// looks healthy while silently recording nothing.
func TestReindexPreservesFileMode(t *testing.T) {
	dir := t.TempDir()
	arc := newArchive(dir)
	st := 200
	if err := arc.Append(&Record{
		RequestID: "20260810mode", TS: "2026/08/10 10:00:00", Status: &st,
		Model: "m", Request: Raw(`{"model":"m"}`),
	}); err != nil {
		t.Fatal(err)
	}
	arc.Close()

	ip := filepath.Join(dir, "arc-20260810.idx")
	if err := os.Chmod(ip, 0o640); err != nil {
		t.Skipf("chmod unsupported here: %v", err)
	}
	before, err := os.Stat(ip)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode().Perm() != 0o640 {
		t.Skipf("filesystem does not honour the mode (got %v)", before.Mode().Perm())
	}

	if _, err := reindexDay(dir, "20260810"); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	after, err := os.Stat(ip)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("mode changed by reindex: %v -> %v",
			before.Mode().Perm(), after.Mode().Perm())
	}
}
