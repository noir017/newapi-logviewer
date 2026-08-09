package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

// reingest applies a parser fix to history.
//
// Folding is lossy by design, so when the parser learns a shape it previously
// could not read (a provider's native streaming format, say), records already
// archived stay wrong. Re-running the raw log through the current parser into a
// scratch archive and appending the results here fixes them.
//
// The live archive is only ever appended to. Existing members and their offsets
// are untouched, so a failure part-way leaves every previously-readable record
// exactly as readable as before; the reader takes the last version of an id.
//
//	logviewer -reingest -src /scratch/archive -day 20260809 [-only rid,rid]
//
// -src is an archive produced by pointing a viewer at the raw logs. ARCHIVE_DIR
// is the destination, so this runs with the same configuration as the server.
func reingest(dstDir, srcDir, day, only string) (int, error) {
	if dstDir == srcDir {
		return 0, fmt.Errorf("source and destination are the same directory")
	}
	src := newArchive(srcDir)
	entries, err := src.index(day)
	if err != nil {
		return 0, fmt.Errorf("read source index: %w", err)
	}

	want := map[string]bool{}
	for _, r := range strings.Split(only, ",") {
		if r = strings.TrimSpace(r); r != "" {
			want[r] = true
		}
	}

	// Keep only the newest version of each id in the source, so re-running this
	// does not stack duplicates in the destination.
	latest := map[string]int{}
	for i, e := range entries {
		latest[e.RID] = i
	}

	dst := newArchive(dstDir)
	defer dst.Close()
	n := 0
	for i, e := range entries {
		if latest[e.RID] != i {
			continue
		}
		if len(want) > 0 && !want[e.RID] {
			continue
		}
		rec, err := src.fetch(day, e)
		if err != nil {
			return n, fmt.Errorf("fetch %s: %w", e.RID, err)
		}
		if err := dst.Append(rec); err != nil {
			return n, fmt.Errorf("append %s: %w", e.RID, err)
		}
		n++
	}
	return n, nil
}

// runReingest is the -reingest entry point; it returns true if it handled the
// invocation and the process should exit.
//
// The flag set is only parsed when -reingest is actually present. This binary
// is normally started with new-api's own flags (--log-dir and friends), and a
// stray flag.Parse would abort the server on an unknown one.
func runReingest(cfg Config) bool {
	asked := false
	for _, a := range os.Args[1:] {
		if a == "-reingest" || a == "--reingest" {
			asked = true
			break
		}
	}
	if !asked {
		return false
	}

	fs := flag.NewFlagSet("reingest", flag.ExitOnError)
	fs.Bool("reingest", false, "append records from -src into ARCHIVE_DIR")
	src := fs.String("src", "", "scratch archive directory to copy from")
	day := fs.String("day", "", "day to copy, yyyymmdd")
	only := fs.String("only", "", "comma-separated request ids; empty means every record that day")
	if err := fs.Parse(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
	if *src == "" || *day == "" {
		log.Fatal("-reingest requires -src and -day")
	}
	n, err := reingest(cfg.ArchiveDir, *src, *day, *only)
	if err != nil {
		log.Fatalf("reingest: %v (%d records appended before the failure)", err, n)
	}
	log.Printf("reingest: appended %d records from %s into %s", n, *src, cfg.ArchiveDir)
	return true
}
