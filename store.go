package main

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// store re-parses the log directory at most every CacheTTL, and only when the
// files actually changed. The Python version re-read the logs on a bare timer;
// checking mtime+size first means an idle tab with auto-refresh on costs a few
// stat() calls instead of re-parsing 40MB every 3 seconds.
type store struct {
	dir        string
	ttl        time.Duration
	limitBytes int64

	mu      sync.RWMutex
	data    []*Record
	checked time.Time
	sig     string
}

func newStore(cfg Config) *store {
	return &store{dir: cfg.LogDir, ttl: cfg.CacheTTL, limitBytes: cfg.LimitBytes}
}

func (s *store) signature() string {
	files, _ := filepath.Glob(filepath.Join(s.dir, "*.log"))
	var sb []byte
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			continue
		}
		sb = append(sb, f...)
		sb = append(sb, byte(':'))
		sb = st.ModTime().AppendFormat(sb, time.RFC3339Nano)
		sb = append(sb, byte('/'))
		sb = appendInt(sb, st.Size())
		sb = append(sb, byte(';'))
	}
	return string(sb)
}

func appendInt(b []byte, n int64) []byte {
	if n == 0 {
		return append(b, '0')
	}
	var tmp [20]byte
	i := len(tmp)
	for n > 0 {
		i--
		tmp[i] = byte('0' + n%10)
		n /= 10
	}
	return append(b, tmp[i:]...)
}

func (s *store) records(force bool) []*Record {
	s.mu.RLock()
	fresh := !force && s.data != nil && time.Since(s.checked) < s.ttl
	data := s.data
	s.mu.RUnlock()
	if fresh {
		return data
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// re-check: another goroutine may have refreshed while we waited
	if !force && s.data != nil && time.Since(s.checked) < s.ttl {
		return s.data
	}
	sig := s.signature()
	s.checked = time.Now()
	if !force && s.data != nil && sig == s.sig {
		return s.data // nothing on disk moved
	}
	s.data = Load(s.dir, s.limitBytes)
	s.sig = sig
	return s.data
}
