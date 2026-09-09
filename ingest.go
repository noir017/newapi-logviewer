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
// Completion is decided by the GIN access-log line where there is one, and by
// the billing line otherwise. New API emits the GIN line once the response is
// fully written, and it is genuinely last: across 3,645 real records, zero had
// any further line after their GIN line.
//
// But it does not always reach the log FILE. gin's access logger writes to
// gin.DefaultWriter, a different sink from the file New API opens for its own
// logger, and on this deployment the file contains 347,863 DEBUG lines and zero
// GIN lines while `docker logs` shows them on stdout throughout. So the billing
// line - which is written after delivery and always lands in the file - is
// accepted as a completion marker too, after a short quiet period.
//
// A call with neither is still running (or was interrupted), so it stays in the
// spool and is retried on the next pass.
type ingester struct {
	dir      string // spool directory (tmpfs)
	arc      *archive
	keep     time.Duration // how long a fully-consumed spool file may linger
	maxBytes int64         // spool high-water mark; 0 disables
	// stallAfter bounds how long a call may sit without new lines before it is
	// archived as incomplete. Must exceed the longest plausible gap between
	// streaming chunks, or a slow model gets filed as truncated.
	stallAfter time.Duration
	// settleAfter is how long a billed call must be quiet before it is archived
	// without a GIN line. Short: it only has to outlast the few trailing chunks
	// that can follow the billing line, not a whole inter-chunk gap.
	settleAfter time.Duration

	mu       sync.Mutex
	offsets  map[string]int64    // spool file -> bytes consumed
	pending  map[string]*Record  // in-flight calls, keyed by request id
	archived map[string]struct{} // ids already written, so a retry cannot double-write
	stop     chan struct{}

	// push, when set, replaces the local archive as the destination: folded
	// records go to another pod's viewer over HTTP and are only forgotten once
	// it has acknowledged them. Nil is the single-machine case and the one this
	// file was written for - every branch on it below leaves that path exactly
	// as it was. See push.go.
	push *pusher

	// Write-side health, for /healthz. Both outages this code has had were
	// invisible from outside: the container was healthy, pages were fast, and
	// pending was 0, while the archive recorded nothing for hours. `pending`
	// cannot detect it - a stalled call is archived-or-dropped by the deadline
	// in flushFinished either way, so the counter reads clean whether writes
	// are landing or failing. What has to be reported is whether an append has
	// SUCCEEDED recently, and whether one is currently failing.
	lastAppend  time.Time // last successful Append
	lastErr     string    // last append error, empty once one succeeds
	appendFails int       // consecutive failures
	appends     int64     // successful appends since start
	truncErr    string    // last live-spool truncate error, empty once one succeeds
}

func newIngester(spoolDir string, arc *archive, keep time.Duration, maxBytes int64) *ingester {
	return &ingester{
		dir: spoolDir, arc: arc, keep: keep, maxBytes: maxBytes,
		stallAfter:  10 * time.Minute,
		settleAfter: 15 * time.Second,
		offsets:     map[string]int64{},
		pending:     map[string]*Record{},
		archived:    map[string]struct{}{},
		stop:        make(chan struct{}),
	}
}

// withPusher sends folded records to another pod instead of archiving them
// locally. A nil pusher (PUSH_URL unset) leaves the ingester in its
// single-machine configuration.
func (i *ingester) withPusher(p *pusher) *ingester {
	i.push = p
	return i
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

	// Push mode with the receiver down: stop consuming the spool until it comes
	// back, and spend this pass retrying the backlog instead.
	//
	// The spool is the only buffer. A record read out of it lives in memory
	// until the receiver acknowledges it, and the read offsets live in memory
	// too - so a restart mid-outage re-reads the spool from the start and
	// rebuilds every unacknowledged record. Reading further would trade log
	// files that can still be re-read for records that a restart would lose,
	// and pruning during an outage would delete the very bytes the recovery
	// depends on. The cost is spool space, which /healthz reports and
	// SPOOL_MAX_MB bounds.
	if i.push != nil && i.push.failing() {
		i.flushFinished()
		return
	}

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
//
// In push mode the destination is another pod rather than the local archive, and
// the finished calls are shipped as one batch after the walk instead of one at a
// time inside it: a WAN round trip per record would not keep up with a burst,
// and a record is only forgotten once the receiver has acknowledged it either
// way.
func (i *ingester) flushFinished() {
	now := time.Now()
	var batch []*Record
	var batchBytes int64
	for rid, rec := range i.pending {
		done := rec.Status != nil && rec.TS != ""
		if !done && rec.billingSeen && !rec.lastSeen.IsZero() &&
			now.Sub(rec.lastSeen) >= i.settleAfter {
			// No GIN line, but billing arrived - the response was delivered and
			// charged. gin's access logger writes to a different sink than the
			// file New API logs to, so on this deployment the GIN line never
			// reaches the spool at all and every call would otherwise wait out
			// stallAfter and be filed as Stalled.
			//
			// settleAfter, not immediately: billing is emitted around the end of
			// the response, and a few trailing chunks can still follow it in the
			// file. Archiving the moment billing lands truncated real content.
			// A short quiet period costs nothing - the record is only written
			// once, and it is written correct.
			done = true
		}
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
		// Stalled is decided here, after the last finalize, so the outcome has
		// to be re-derived before the record is frozen into the archive.
		rec.refreshOutcome()
		if !worthArchiving(rec) {
			delete(i.pending, rid)
			continue
		}
		if _, already := i.archived[rid]; already {
			delete(i.pending, rid)
			continue
		}
		if i.push != nil {
			// Collect; the batch goes out below. Whatever does not fit stays
			// pending and leaves on the next pass, which is also what keeps one
			// request bounded when a burst finishes at once.
			if len(batch) >= pushMaxRecords || batchBytes >= pushMaxBytes {
				continue
			}
			batch = append(batch, rec)
			batchBytes += recordSize(rec)
			continue
		}
		if err := i.arc.Append(rec); err != nil {
			// Leave it pending: a failed append (disk full, permissions) must
			// not silently drop the call. It retries next pass.
			log.Printf("archive append %s: %v", rid, err)
			i.appendFails++
			i.lastErr = err.Error()
			continue
		}
		i.appends++
		i.lastAppend = now
		i.appendFails, i.lastErr = 0, ""
		i.archived[rid] = struct{}{}
		delete(i.pending, rid)
	}
	if len(batch) > 0 {
		i.flushPush(batch, now)
	}
	// Bound the dedup set. Ids are only revisited within one spool file's
	// lifetime, so anything older than the retention window cannot recur.
	if len(i.archived) > 50000 {
		i.archived = map[string]struct{}{}
	}
}

// flushPush ships one batch and consumes the records only on an ACK.
//
// A failure leaves every record of the batch pending, which is what makes the
// spool the buffer: nothing is deleted, nothing is truncated, and the next pass
// retries the same records rather than reading more. The receiver deduplicates
// on request id, so a batch that landed and lost its ACK is re-sent safely.
//
// The write-side health counters are shared with the local append path: in push
// mode the push IS the write, and /healthz has to go red for the same reason -
// records are being folded and not stored anywhere.
func (i *ingester) flushPush(batch []*Record, now time.Time) {
	if !i.push.ready(now) {
		return // inside the backoff window; nothing is consumed
	}
	if i.push.pod != "" {
		for _, rec := range batch {
			rec.Pod = i.push.pod
		}
	}
	if err := i.push.send(batch); err != nil {
		i.push.failed(now, err)
		log.Printf("push %d records to %s: %v (retry in %s; the spool is held until it succeeds)",
			len(batch), i.push.url, err, time.Until(i.push.nextTry).Round(time.Second))
		i.appendFails++
		i.lastErr = err.Error()
		return
	}
	i.push.succeeded(len(batch))
	for _, rec := range batch {
		i.archived[rec.RequestID] = struct{}{}
		delete(i.pending, rec.RequestID)
	}
	i.appends += int64(len(batch))
	i.lastAppend = now
	i.appendFails, i.lastErr = 0, ""
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
// permanent. A file is only removed once its bytes have been read.
//
// The size sweep is the one that actually protects the gateway: the spool is a
// fixed-size tmpfs, and a full tmpfs makes New API's log writes fail. Age alone
// cannot bound it - a burst of traffic can fill the spool well inside the
// retention window - so once the high-water mark is crossed, consumed files are
// dropped oldest-first regardless of age.
//
// The file New API is still writing is never deleted, but it can be truncated -
// see truncateLive, and the incident note there. Skipping it entirely is what
// let the spool reach 100% of its tmpfs and take the gateway's logging down.
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
		// Still over after dropping every older file. With New API's actual
		// rotation behaviour - one log file per process start, never rotated -
		// that is the normal case and not an edge one: there ARE no older
		// files, so every sweep above was a no-op and the single live file grew
		// to fill the tmpfs unopposed.
		if total > i.maxBytes && newest != "" {
			if freed := i.truncateLive(newest); freed > 0 {
				total -= freed
			}
		}
	}
}

// truncateLive reclaims a fully-consumed spool file that New API still holds
// open, by truncating it rather than unlinking it.
//
// Unlinking would be worse than doing nothing: New API keeps writing to the
// open descriptor, so the inode's blocks are never freed while the directory
// entry is gone - the space stays consumed and the log becomes unreadable. Only
// truncation returns the pages while leaving the descriptor valid. This is the
// copytruncate pattern, and it is safe here precisely because the bytes have
// already been folded into the archive.
//
// Returns the number of bytes freed.
//
// This exists because of a real outage: a 256MB tmpfs at 100%, a single
// never-rotated log file that prune refused to touch, and ~22 hours during
// which New API's log writes failed while the container stayed healthy and the
// viewer served pages normally.
func (i *ingester) truncateLive(path string) int64 {
	// Re-stat immediately before truncating and require the file to be fully
	// consumed as of this instant. Anything New API appends between this stat
	// and the truncate is lost; that window is microseconds against a poll
	// interval of seconds, and the alternative is the gateway losing all
	// logging once the tmpfs fills.
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	size := st.Size()
	if size == 0 || i.offsets[path] < size {
		return 0 // unread bytes: dropping them would lose calls
	}
	if err := os.Truncate(path, 0); err != nil {
		// Truncating is a permission on the FILE, unlike unlinking a rotated one,
		// which is a permission on the directory the viewer owns. New API creates
		// its log 0644 as root, so this fails unless something hands the file
		// over - the combined image's entrypoint runs a watcher that chowns spool
		// files to the viewer's uid for exactly this reason.
		//
		// The chmod retry below only helps when the viewer already owns the file
		// (chmod requires ownership, not directory ownership - verified as uid
		// 65534 against a root-owned file: EPERM). It is kept for the case where
		// the owner is right but the mode is not, and it is cheap.
		if cherr := os.Chmod(path, 0o666); cherr == nil {
			err = os.Truncate(path, 0)
		}
		if err != nil {
			// Worth surfacing on /healthz rather than only in the log: this is
			// the one reclaim path that protects the gateway, and when it fails
			// the spool fills to 100% of its tmpfs and New API's writes start
			// failing. It failed silently in production for exactly this reason,
			// and the spool-over-max signal alone did not say why.
			i.truncErr = err.Error()
			log.Printf("spool truncate %s: %v", path, err)
			return 0
		}
	}
	i.truncErr = ""

	// Resume from wherever the file now ends, which is NOT always zero. If New
	// API opened the log with O_APPEND its next write lands at 0 and the stat
	// below reports 0. Without O_APPEND the descriptor keeps its old offset and
	// the next write recreates a sparse file that still reports the old size -
	// the pages are freed either way, but reading from 0 would then scan
	// hundreds of megabytes of holes on every pass.
	if st2, err := os.Stat(path); err == nil {
		i.offsets[path] = st2.Size()
	} else {
		i.offsets[path] = 0
	}
	log.Printf("spool: truncated live file %s, freed %d bytes (spool was over SPOOL_MAX_MB)", path, size)
	return size
}

// pendingCount reports in-flight calls, for the health endpoint.
func (i *ingester) pendingCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.pending)
}

// health reports whether ingest is actually working, not merely running.
//
// The distinction matters because reads need no write permission: a viewer that
// cannot write its archive still serves fast, correct pages and a green
// pending-only health check while recording nothing. That happened twice here -
// once from a root-run -reindex leaving the .idx owned by root, once from the
// spool filling and New API's writes failing.
//
// Unhealthy means one of:
//   - an append is currently failing (permissions, disk full, bad ownership)
//   - the spool is over its high-water mark, which means bytes are arriving
//     faster than they are being folded, or not being folded at all
//   - calls are waiting to be archived and none has been written for a while
//
// Staleness alone is deliberately NOT a failure: an idle gateway legitimately
// archives nothing for hours, and a health check that goes red overnight is one
// that gets ignored by morning. It only counts as a failure when there is work
// waiting - pending > 0 with no successful append - which is the shape both real
// incidents had.
func (i *ingester) health() map[string]any {
	i.mu.Lock()
	defer i.mu.Unlock()

	var spool int64
	files, _ := filepath.Glob(filepath.Join(i.dir, "*.log"))
	for _, f := range files {
		if st, err := os.Stat(f); err == nil {
			spool += st.Size()
		}
	}

	h := map[string]any{
		"pending":      len(i.pending),
		"appends":      i.appends,
		"spool_bytes":  spool,
		"spool_files":  len(files),
		"archive_ok":   true,
		"append_fails": i.appendFails,
	}
	if !i.lastAppend.IsZero() {
		h["last_append"] = i.lastAppend.Format(time.RFC3339)
		h["last_append_age_sec"] = int(time.Since(i.lastAppend).Seconds())
	}
	if i.lastErr != "" {
		h["last_error"] = i.lastErr
	}
	// In push mode the destination is another pod, so the numbers an operator
	// needs are about the link, not the disk: how many records have been
	// acknowledged, and whether the sender is currently backing off - which is
	// also when the spool stops being consumed and starts growing. `pending`
	// above doubles as the unacknowledged count, since in push mode a folded
	// record stays pending until the receiver has taken it.
	if i.push != nil {
		h["push_url"] = i.push.url
		h["push_pod"] = i.push.pod
		h["push_sent"] = i.push.sent
		if i.push.fails > 0 {
			h["push_fails"] = i.push.fails
			h["push_retry_in_sec"] = int(time.Until(i.push.nextTry).Seconds())
		}
	}

	ok := true
	var why []string
	if i.appendFails > 0 {
		ok = false
		if i.push != nil {
			why = append(why, "pushes to the archiving pod are failing")
		} else {
			why = append(why, "archive appends are failing")
		}
	}
	if i.maxBytes > 0 && spool > i.maxBytes {
		ok = false
		why = append(why, "spool over SPOOL_MAX_MB")
	}
	// A failing reclaim is the reason the spool stays over, so say so. Without
	// this the operator sees "spool over SPOOL_MAX_MB" and reasonably concludes
	// ingest is behind, when in fact the bytes are archived and the viewer
	// simply cannot free them.
	if i.truncErr != "" {
		h["spool_truncate_error"] = i.truncErr
		ok = false
		why = append(why, "cannot truncate live spool file (check its mode; truncate needs write on the FILE)")
	}
	// Work waiting and nothing written recently. The window is generous
	// relative to stallAfter: below it, calls legitimately sit in pending while
	// they stream, and every one of them is archived or dropped by the deadline.
	if len(i.pending) > 0 {
		idle := 2 * i.stallAfter
		if i.lastAppend.IsZero() || time.Since(i.lastAppend) > idle {
			ok = false
			why = append(why, "calls pending but nothing archived recently")
		}
	}
	h["archive_ok"] = ok
	if len(why) > 0 {
		h["degraded"] = why
	}
	return h
}

func mtime(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}
