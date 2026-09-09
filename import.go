package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// import merges another pod's archive into this one.
//
// This is the one-shot counterpart to push mode (push.go). Turning on PUSH_URL
// makes a pod stop archiving locally from that moment on, but it says nothing
// about what the pod already wrote - and on this deployment that is 269MB of
// history, which would otherwise stay visible only through a viewer that no
// longer records anything.
//
//	logviewer -import -src /path/to/other-pods/archive [-day 20260901] [-pod oracle]
//
// It reads through the source's index, fetches each record - which reinlines the
// v2 blob pool, so what comes back is the original request bytes whatever format
// the source is in - and appends it here through AppendNew, the same path a push
// takes. Which means the same guarantee: it is idempotent on request id, so a run
// interrupted half way through 269MB is resumed by running it again, and a second
// full run reports every record as a duplicate and writes nothing.
//
// Records that cannot be read are skipped rather than aborting the day. A
// migration that stops on the first bad member would have to be restarted by
// hand for each one, and the alternative is not lossless anyway - an unreadable
// source record has nothing to import.
type importResult struct {
	stored    int
	duplicate int
	skipped   int
}

func importArchive(dstDir, srcDir, day, pod string) (importResult, error) {
	var res importResult
	if filepath.Clean(dstDir) == filepath.Clean(srcDir) {
		return res, fmt.Errorf("source and destination are the same directory")
	}
	src := newArchive(srcDir)
	dst := newArchive(dstDir)
	defer dst.Close()

	days := src.days()
	if day != "" {
		days = []string{day}
	}
	if len(days) == 0 {
		return res, fmt.Errorf("no archived days in %s", srcDir)
	}

	for _, d := range days {
		entries, err := src.index(d)
		if err != nil {
			return res, fmt.Errorf("read %s index: %w", d, err)
		}
		// Keep only the newest version of each id, exactly as the reader does:
		// the source archive is append-only too, and a corrected record there
		// must not be imported alongside the version it corrected.
		latest := make(map[string]int, len(entries))
		for i, e := range entries {
			latest[e.RID] = i
		}

		before := res
		for i, e := range entries {
			if latest[e.RID] != i {
				continue
			}
			rec, err := src.fetch(d, e)
			if err != nil {
				log.Printf("import %s: skip %s: %v", d, e.RID, err)
				res.skipped++
				continue
			}
			if pod != "" {
				rec.Pod = pod
			}
			ok, err := dst.AppendNew(rec)
			if err != nil {
				// Unlike an unreadable source record, a failed append is about
				// the destination - out of space, wrong owner - and every
				// record after it would fail the same way.
				return res, fmt.Errorf("append %s: %w", e.RID, err)
			}
			if ok {
				res.stored++
			} else {
				res.duplicate++
			}
		}
		// Per day, because this runs for tens of minutes over a real archive and
		// an operator watching it needs to see it moving.
		log.Printf("import %s: %d imported, %d already present, %d unreadable",
			d, res.stored-before.stored, res.duplicate-before.duplicate,
			res.skipped-before.skipped)
	}
	return res, nil
}

// runImport is the -import entry point; it returns true if it handled the
// invocation and the process should exit. See runReingest for why the flag set
// is only parsed when the flag is actually present.
func runImport(cfg Config) bool {
	asked := false
	for _, a := range os.Args[1:] {
		if a == "-import" || a == "--import" {
			asked = true
			break
		}
	}
	if !asked {
		return false
	}

	fs := flag.NewFlagSet("import", flag.ExitOnError)
	fs.Bool("import", false, "merge another pod's archive into ARCHIVE_DIR")
	src := fs.String("src", "", "the other pod's archive directory")
	day := fs.String("day", "", "day to import, yyyymmdd; empty means every day in -src")
	pod := fs.String("pod", "", "label imported records with this pod name; empty leaves them as they are")
	if err := fs.Parse(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
	if *src == "" {
		log.Fatal("-import requires -src")
	}
	res, err := importArchive(cfg.ArchiveDir, *src, *day, *pod)
	if err != nil {
		log.Fatalf("import: %v (%d imported, %d already present, %d unreadable before the failure; re-running resumes)",
			err, res.stored, res.duplicate, res.skipped)
	}
	log.Printf("import: %d records from %s into %s (%d already present, %d unreadable)",
		res.stored, *src, cfg.ArchiveDir, res.duplicate, res.skipped)
	return true
}
