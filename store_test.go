package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The store reads only what was appended since last time. That is the whole
// point (a 300MB log dir must not be re-parsed per request) and also the part
// most likely to go subtly wrong: a call's lines arrive across several reads,
// so derived fields have to be recomputed as later lines land.
func TestIncrementalParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.log")
	rid := "IncrementalAaaaBbbbCcccDddd"

	append := func(lines ...string) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range lines {
			fmt.Fprintln(f, l)
		}
		f.Close()
	}

	cfg := loadConfig()
	cfg.LogDir = dir
	cfg.CacheTTL = 0 // refresh on every call so the test controls timing
	st := newStore(cfg)

	// --- first read: request only ---
	append(`[DEBUG] 2026/01/15 - 10:00:00 | ` + rid + ` | text request body: ` +
		`{"model":"m1","messages":[{"role":"user","content":"hello"}],` +
		`"tools":[{"type":"function","function":{"name":"t1"}}]}`)
	recs := st.records(false)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.Model != "m1" || r.Preview != "hello" || r.ToolCount != 1 {
		t.Errorf("after first read: model=%q preview=%q tools=%d", r.Model, r.Preview, r.ToolCount)
	}
	if r.Status != nil {
		t.Errorf("status should still be unknown, got %v", *r.Status)
	}
	firstOffset := st.offsets[path]
	if firstOffset == 0 {
		t.Error("offset not recorded")
	}

	// --- second read: response + billing + gin line appended ---
	append(
		`[DEBUG] 2026/01/15 - 10:00:02 | `+rid+` | upstream response body: `+
			`{"choices":[{"index":0,"message":{"role":"assistant","content":"hi there",`+
			`"tool_calls":[{"id":"c1","type":"function","function":{"name":"t1","arguments":"{}"}}]}}],`+
			`"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
		`[INFO]  2026/01/15 - 10:00:02 | `+rid+` | record consume log: userId=1, params=`+
			`{"model_name":"m1","quota":7,"channel_id":2,"token_name":"tk"}`,
		`[GIN]   2026/01/15 - 10:00:02 | relay | `+rid+` | 200 | 2.0s | 10.0.0.9 | POST /v1/chat/completions`,
	)
	recs = st.records(false)
	if len(recs) != 1 {
		t.Fatalf("got %d records after append, want still 1", len(recs))
	}
	r = recs[0]

	// Everything derived must reflect the LATER lines, not the state captured
	// on the first pass.
	if r.Status == nil || *r.Status != 200 {
		t.Errorf("status = %v, want 200", r.Status)
	}
	if r.Latency != "2.0s" {
		t.Errorf("latency = %q", r.Latency)
	}
	if r.Quota == nil || *r.Quota != 7 {
		t.Errorf("quota = %v, want 7", r.Quota)
	}
	if r.Usage == nil || r.Usage.TotalTokens == nil || *r.Usage.TotalTokens != 8 {
		t.Errorf("usage = %+v", r.Usage)
	}
	if len(r.CalledTools) != 1 || r.CalledTools[0] != "t1" {
		t.Errorf("called_tools = %v (must be re-derived after the response arrives)", r.CalledTools)
	}
	// and the first read's fields must survive
	if r.Model != "m1" || r.Preview != "hello" {
		t.Errorf("earlier fields lost: model=%q preview=%q", r.Model, r.Preview)
	}
	if st.offsets[path] <= firstOffset {
		t.Errorf("offset did not advance: %d -> %d", firstOffset, st.offsets[path])
	}

	// --- third read: nothing appended, must be a no-op ---
	before := st.offsets[path]
	recs = st.records(false)
	if len(recs) != 1 || st.offsets[path] != before {
		t.Errorf("idle refresh changed state: %d records, offset %d -> %d",
			len(recs), before, st.offsets[path])
	}

	// --- a second file appears (log rotation) ---
	rid2 := "SecondFileAaaaBbbbCcccDddd"
	time.Sleep(10 * time.Millisecond) // distinct mtime for ordering
	if err := os.WriteFile(filepath.Join(dir, "next.log"), []byte(
		`[DEBUG] 2026/01/15 - 11:00:00 | `+rid2+` | text request body: {"model":"m2","messages":[]}`+"\n"+
			`[GIN]   2026/01/15 - 11:00:01 | relay | `+rid2+` | 200 | 1.0s | 10.0.0.9 | POST /v1/chat/completions`+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	recs = st.records(false)
	if len(recs) != 2 {
		t.Fatalf("got %d records after rotation, want 2", len(recs))
	}
	// newest first
	if recs[0].RequestID != rid2 {
		t.Errorf("sort order wrong: first = %s", recs[0].RequestID)
	}

	// --- truncation in place must not leave stale offsets ---
	if err := os.WriteFile(path, []byte(
		`[DEBUG] 2026/01/15 - 12:00:00 | TruncatedAaaaBbbbCcccDddd | text request body: {"model":"m3","messages":[]}`+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	recs = st.records(false)
	found := false
	for _, rr := range recs {
		if rr.Model == "m3" {
			found = true
		}
	}
	if !found {
		t.Error("record from a truncated-and-rewritten file was not picked up")
	}
}

// A record whose lines span two reads must not accumulate duplicate stream
// chunks or duplicate errors when finalize runs more than once.
func TestFinalizeIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.log")
	rid := "StreamingAaaaBbbbCcccDddd"

	write := func(s string) {
		f, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		f.WriteString(s)
		f.Close()
	}
	chunk := func(txt string) string {
		return `[DEBUG] 2026/01/15 - 10:00:00 | ` + rid + ` | stream scanner data: data: ` +
			`{"choices":[{"index":0,"delta":{"content":"` + txt + `"}}]}` + "\n"
	}

	cfg := loadConfig()
	cfg.LogDir = dir
	cfg.CacheTTL = 0
	st := newStore(cfg)

	write(`[DEBUG] 2026/01/15 - 10:00:00 | ` + rid + ` | text request body: {"model":"m","stream":true,"messages":[]}` + "\n")
	write(chunk("Hello "))
	if got := st.records(false)[0].StreamContent; got != "Hello " {
		t.Fatalf("first read content = %q", got)
	}

	write(chunk("world"))
	got := st.records(false)[0].StreamContent
	if got != "Hello world" {
		t.Errorf("content = %q, want %q (chunks must accumulate exactly once)", got, "Hello world")
	}

	// no new bytes: content must not double
	if got := st.records(false)[0].StreamContent; got != "Hello world" {
		t.Errorf("idle refresh changed content to %q", got)
	}
}
