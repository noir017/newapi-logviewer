package main

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ingester consumes New API's raw *.log spool and moves finished calls into the
// archive.
//
// The spool is expected to live on tmpfs. New API writes every streaming chunk
// there - 76% of its output by volume - and that traffic never needs to reach a
// disk: once a call is complete its chunks are folded into one record and only
// the record is persisted. The spool then only has to be large enough to hold
// calls that are still in flight.
//
// Completion is decided by the GIN access-log line. New API emits it once the
// response is fully written, and it is genuinely last: across 3,645 real
// records, zero had any further line after their GIN line. A call without one
// is still running (or was interrupted), so it stays in the spool and is
// retried on the next pass.
type ingester struct {
	dir      string // spool directory (tmpfs)
	arc      *archive
	keep     time.Duration // how long a fully-consumed spool file may linger
	maxBytes int64         // spool high-water mark; 0 disables
	// stallAfter bounds how long a call may sit without new lines before it is
	// archived as incomplete. Must exceed the longest plausible gap between
	// streaming chunks, or a slow model gets filed as truncated.
	stallAfter time.Duration

	mu       sync.Mutex
	offsets  map[string]int64    // spool file -> bytes consumed
	pending  map[string]*Record  // in-flight calls, keyed by request id
	archived map[string]struct{} // ids already written, so a retry cannot double-write
	stop     chan struct{}
}

func newIngester(spoolDir string, arc *archive, keep time.Duration, maxBytes int64) *ingester {
	return &ingester{
		dir: spoolDir, arc: arc, keep: keep, maxBytes: maxBytes,
		stallAfter: 10 * time.Minute,
		offsets:    map[string]int64{},
		pending:    map[string]*Record{},
		archived:   map[string]struct{}{},
		stop:       make(chan struct{}),
	}
}

// Run polls the spool until Stop. Polling rather than inotify: the spool is a
// handful of files on tmpfs, a stat every second costs nothing, and it keeps
// the binary dependency-free and identical across platforms.
func (i *ingester) Run(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	i.once()
	for {
		select {
		case <-i.stop:
			return
		case <-t.C:
			i.once()
		}
	}
}

func (i *ingester) Stop() { close(i.stop) }

// once reads whatever is new in the spool and archives every call that finished.
func (i *ingester) once() {
	i.mu.Lock()
	defer i.mu.Unlock()

	files, _ := filepath.Glob(filepath.Join(i.dir, "*.log"))
	sort.Slice(files, func(a, b int) bool { return mtime(files[a]).Before(mtime(files[b])) })

	live := map[string]bool{}
	for _, f := range files {
		live[f] = true
		st, err := os.Stat(f)
		if err != nil {
			continue
		}
		size := st.Size()
		from := i.offsets[f]
		switch {
		case from > size:
			from = 0 // truncated in place
		case from == size:
			continue
		}
		// No LIMIT_MB tail-seek here, unlike the old reader: skipping into the
		// middle of a spool file would permanently lose the calls that were
		// skipped, and the archive is meant to be complete.
		parseRange(f, from, false, i.pending)
		i.offsets[f] = size
	}
	for f := range i.offsets {
		if !live[f] {
			delete(i.offsets, f)
		}
	}

	i.flushFinished()
	i.prune(files)
}

// flushFinished archives every pending call that has its GIN line, and forgets
// it. Callers hold i.mu.
func (i *ingester) flushFinished() {
	now := time.Now()
	for rid, rec := range i.pending {
		done := rec.Status != nil && rec.TS != ""
		if !done {
			// A call with no GIN line is normally still streaming. But some
			// never get one: the client disconnects, the gateway restarts
			// mid-response, or the call straddles a log rotation and its tail
			// lands in a file this pass has not reached. Without a deadline
			// those records would sit in memory forever and never be archived
			// - a slow leak that also silently loses the call.
			//
			// So after stallAfter with no new lines, archive what we have and
			// mark it incomplete rather than dropping or keeping it.
			if rec.lastSeen.IsZero() || now.Sub(rec.lastSeen) < i.stallAfter {
				continue
			}
			rec.Stalled = true
		}
		if !worthArchiving(rec) {
			delete(i.pending, rid)
			continue
		}
		if _, already := i.archived[rid]; already {
			delete(i.pending, rid)
			continue
		}
		if err := i.arc.Append(rec); err != nil {
			// Leave it pending: a failed append (disk full, permissions) must
			// not silently drop the call. It retries next pass.
			log.Printf("archive append %s: %v", rid, err)
			continue
		}
		i.archived[rid] = struct{}{}
		delete(i.pending, rid)
	}
	// Bound the dedup set. Ids are only revisited within one spool file's
	// lifetime, so anything older than the retention window cannot recur.
	if len(i.archived) > 50000 {
		i.archived = map[string]struct{}{}
	}
}

// worthArchiving keeps dashboard polling out of the permanent record.
//
// New API assigns a request id to every HTTP request, including the console's
// own /api/* status polls - on an idle gateway those are the overwhelming
// majority of ids. They have no request body and no billing entry, so they are
// distinguishable, and the previous design filtered them at display time. Here
// the filter has to happen before the write: the archive is permanent, and a
// year of health checks is a year of noise nobody can remove.
//
// Errors are kept regardless of shape, since a call that failed before its body
// was logged is exactly what someone would come looking for.
func worthArchiving(r *Record) bool {
	if len(r.Errors) > 0 {
		return true
	}
	if !r.Request.empty() || !r.Billing.empty() {
		return true
	}
	if r.Status != nil && *r.Status >= 400 {
		return true
	}
	return false
}

// prune deletes spool files that are fully consumed, by age and then by size.
//
// Deleting is safe here in a way it would not be for the log directory itself:
// everything in the spool has already been folded into the archive, which is
// permanent. A file is only removed once its bytes have been read, and the
// file New API is currently writing is never touched.
//
// The size sweep is the one that actually protects the gateway: the spool is a
// fixed-size tmpfs, and a full tmpfs makes New API's log writes fail. Age alone
// cannot bound it - a burst of traffic can fill the spool well inside the
// retention window - so once the high-water mark is crossed, consumed files are
// dropped oldest-first regardless of age.
func (i *ingester) prune(files []string) {
	newest := ""
	if len(files) > 0 {
		newest = files[len(files)-1] // mtime order; New API is still writing this one
	}
	type finfo struct {
		path string
		size int64
		mod  time.Time
		done bool
	}
	var infos []finfo
	var total int64
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			continue
		}
		total += st.Size()
		infos = append(infos, finfo{f, st.Size(), st.ModTime(), i.offsets[f] >= st.Size()})
	}

	drop := func(fi finfo) bool {
		if fi.path == newest || !fi.done {
			return false
		}
		if err := os.Remove(fi.path); err != nil {
			return false
		}
		delete(i.offsets, fi.path)
		total -= fi.size
		return true
	}

	if i.keep > 0 {
		cutoff := time.Now().Add(-i.keep)
		for _, fi := range infos {
			if fi.mod.Before(cutoff) {
				drop(fi)
			}
		}
	}
	if i.maxBytes > 0 && total > i.maxBytes {
		sort.Slice(infos, func(a, b int) bool { return infos[a].mod.Before(infos[b].mod) })
		for _, fi := range infos {
			if total <= i.maxBytes {
				break
			}
			drop(fi)
		}
	}
}

// pendingCount reports in-flight calls, for the health endpoint.
func (i *ingester) pendingCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.pending)
}

func mtime(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}
