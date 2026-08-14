package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
)

// reindex rebuilds arc-DAY.idx from the records already inside
// arc-DAY.jsonl.gz.
//
// The index is a derived file: every field in it also exists in the record it
// points at. So when a column is added to the list view - the channel, here -
// history can be backfilled without the raw logs, which by then have usually
// been rotated away. Nothing is re-parsed and nothing is re-downloaded.
//
// Only the .idx is rewritten, via a temporary file and a rename, so a failure
// part-way leaves the previous index in place and every record as readable as
// before. The compressed data file is never opened for writing.
//
//	logviewer -reindex [-day 20260809]
//
// Run this with the server stopped. A running ingester holds the .idx open in
// append mode, and a rename out from under it would silently send its appends
// to the replaced inode.
func reindexDay(dir, day string) (int, error) {
	a := newArchive(dir)
	entries, err := a.index(day)
	if err != nil {
		return 0, fmt.Errorf("read index: %w", err)
	}

	_, ip := a.paths(day)
	// The replacement must end up with the original's owner and mode. rename
	// gives the temp file's, and a root-run reindex against a nobody-owned
	// archive would leave the running ingester unable to append - a failure
	// that reads right through, because only writes need the permission.
	orig, err := os.Stat(ip)
	if err != nil {
		return 0, err
	}
	tmp := ip + ".rebuild"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp) // no-op once the rename below has succeeded

	n := 0
	for _, e := range entries {
		rec, err := a.fetch(day, e)
		if err != nil {
			f.Close()
			return n, fmt.Errorf("fetch %s: %w", e.RID, err)
		}
		// Re-derive the outcome before projecting. Records archived before the
		// field existed have Outcome empty but still carry the billing payload
		// it comes from, so this backfills history from the archive alone - the
		// raw logs it was parsed from are long gone.
		rec.refreshOutcome()
		// The offsets are the one thing that cannot be recomputed from the
		// record, so they are carried across verbatim.
		line, err := json.Marshal(makeIdxEntry(rec, e.Off, e.Len))
		if err != nil {
			f.Close()
			return n, err
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			f.Close()
			return n, err
		}
		n++
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return n, err
	}
	if err := f.Close(); err != nil {
		return n, err
	}
	if err := os.Chmod(tmp, orig.Mode().Perm()); err != nil {
		return n, err
	}
	if err := preserveOwner(tmp, orig); err != nil {
		return n, err
	}
	return n, os.Rename(tmp, ip)
}

// runReindex is the -reindex entry point; it returns true if it handled the
// invocation and the process should exit. See runReingest for why the flag set
// is only parsed when the flag is actually present.
func runReindex(cfg Config) bool {
	asked := false
	for _, a := range os.Args[1:] {
		if a == "-reindex" || a == "--reindex" {
			asked = true
			break
		}
	}
	if !asked {
		return false
	}

	fs := flag.NewFlagSet("reindex", flag.ExitOnError)
	fs.Bool("reindex", false, "rebuild arc-*.idx from the archived records")
	day := fs.String("day", "", "day to rebuild, yyyymmdd; empty means every day")
	if err := fs.Parse(os.Args[1:]); err != nil {
		log.Fatal(err)
	}

	arc := newArchive(cfg.ArchiveDir)
	days := arc.days()
	if *day != "" {
		days = []string{*day}
	}
	total := 0
	for _, d := range days {
		n, err := reindexDay(cfg.ArchiveDir, d)
		if err != nil {
			log.Fatalf("reindex %s: %v (%d entries rewritten before the failure; the old index is intact)", d, err, n)
		}
		log.Printf("reindex %s: %d entries", d, n)
		total += n
	}
	log.Printf("reindex: %d entries across %d days in %s", total, len(days), cfg.ArchiveDir)
	return true
}
