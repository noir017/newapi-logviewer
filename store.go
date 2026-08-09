package main

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// store keeps the parsed records in memory and refreshes them incrementally.
//
// A busy gateway with DEBUG=true writes hundreds of MB per day, and the active
// file grows continuously. Re-reading the whole directory on every refresh
// meant a 7-second response on a 325MB log dir, on every single request,
// because the file changed between requests so a mtime check never helped.
//
// Instead we remember the byte offset reached in each file and only read what
// was appended since. A refresh during live traffic reads kilobytes.
type store struct {
	dir        string
	ttl        time.Duration
	limitBytes int64

	mu      sync.RWMutex
	calls   map[string]*Record // by request id, across all files
	sorted  []*Record          // newest first; rebuilt only when calls change
	offsets map[string]int64   // file -> bytes already consumed
	checked time.Time
}

func newStore(cfg Config) *store {
	return &store{
		dir: cfg.LogDir, ttl: cfg.CacheTTL, limitBytes: cfg.LimitBytes,
		calls: map[string]*Record{}, offsets: map[string]int64{},
	}
}

// scan reads whatever is new in each *.log and merges it into s.calls.
// Returns true if anything changed.
func (s *store) scan(full bool) bool {
	files, _ := filepath.Glob(filepath.Join(s.dir, "*.log"))
	sort.Slice(files, func(i, j int) bool { return mtime(files[i]).Before(mtime(files[j])) })

	changed := false
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			continue
		}
		size := st.Size()
		from := s.offsets[f]

		switch {
		case full:
			from = 0
		case from > size:
			// truncated or rotated in place - re-read from the start
			from = 0
		case from == size:
			continue // nothing appended
		}

		// First sight of a large file: skip to the tail rather than parsing
		// history nobody asked for. LIMIT_MB bounds this. Landing mid-line is
		// expected here, unlike a resumed read.
		partial := false
		if from == 0 && s.limitBytes > 0 && size > s.limitBytes {
			from = size - s.limitBytes
			partial = true
		}

		if parseRange(f, from, partial, s.calls) {
			changed = true
		}
		s.offsets[f] = size
	}

	// Drop files that disappeared, so a rotated-away name does not pin state.
	if len(s.offsets) > len(files) {
		live := map[string]bool{}
		for _, f := range files {
			live[f] = true
		}
		for f := range s.offsets {
			if !live[f] {
				delete(s.offsets, f)
			}
		}
	}
	return changed
}

func (s *store) rebuild() {
	out := make([]*Record, 0, len(s.calls))
	for _, r := range s.calls {
		if r.TS != "" {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TS != out[j].TS {
			return out[i].TS > out[j].TS
		}
		return out[i].RequestID > out[j].RequestID
	})
	s.sorted = out
}

func (s *store) records(force bool) []*Record {
	s.mu.RLock()
	fresh := !force && s.sorted != nil && time.Since(s.checked) < s.ttl
	data := s.sorted
	s.mu.RUnlock()
	if fresh {
		return data
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !force && s.sorted != nil && time.Since(s.checked) < s.ttl {
		return s.sorted
	}
	s.checked = time.Now()
	if s.scan(false) || s.sorted == nil {
		s.rebuild()
	}
	return s.sorted
}

func mtime(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}
