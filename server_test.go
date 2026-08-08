package main

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The store is shared by every handler goroutine and mutates itself on refresh.
// Run with -race to prove concurrent readers and a forced re-parse do not
// collide.
func TestConcurrentAccess(t *testing.T) {
	cfg := loadConfig()
	cfg.LogDir = "./testdata"
	cfg.AuthMode = "none"
	cfg.Base = "/logviewer"
	srv := newServer(cfg)

	urls := []string{
		"/logviewer/api/calls?page_size=50",
		"/logviewer/api/calls?refresh=1",
		"/logviewer/api/calls?search=berlin&tool_name=get_weather",
		"/logviewer/api/calls?since=1&until=99999999999",
		"/logviewer/",
		"/logviewer/healthz",
	}
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := httptest.NewRequest("GET", urls[i%len(urls)], nil)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Errorf("%s -> %d", urls[i%len(urls)], w.Code)
			}
		}(i)
	}
	wg.Wait()
}

// Malformed input must be skipped, never panic the parser.
func TestGarbageInput(t *testing.T) {
	calls := map[string]*Record{}
	for _, line := range []string{
		"", "not a log line", "[DEBUG] garbage",
		"[DEBUG] 2026/08/08 - 12:00:00 | SYSTEM | doing things",
		"[DEBUG] 2026/08/08 - 12:00:00 | abcdefghijklmnopqrstuvwxyz | text request body: {broken",
		"[DEBUG] 2026/08/08 - 12:00:00 | abcdefghijklmnopqrstuvwxyz | stream scanner data: data: {nope",
		"[DEBUG] 2026/08/08 - 12:00:00 | abcdefghijklmnopqrstuvwxyz | stream scanner data: [DONE]",
		"[GIN] 2026/08/08 - 12:00:00 | relay | abcdefghijklmnopqrstuvwxyz | 999 | x | y | GET /z",
		"[INFO] 2026/08/08 - 12:00:00 | abcdefghijklmnopqrstuvwxyz | record consume log: userId=1, params=not-json",
		"[DEBUG] 2026/08/08 - 12:00:00 | abcdefghijklmnopqrstuvwxyz | text request body: " +
			`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[{"function":{"name":"t"}}]}`,
	} {
		handleLine(line, calls)
	}
	for _, r := range calls {
		r.finalize()
	}
	rec := calls["abcdefghijklmnopqrstuvwxyz"]
	if rec == nil {
		t.Fatal("record missing")
	}
	if rec.Model != "m" {
		t.Errorf("model = %q, want m", rec.Model)
	}
	if rec.Preview != "hi" {
		t.Errorf("preview = %q, want hi", rec.Preview)
	}
	if len(rec.ToolNames) != 1 || rec.ToolNames[0] != "t" {
		t.Errorf("tool_names = %v", rec.ToolNames)
	}
	if rec.Billing != nil {
		t.Errorf("non-JSON billing should be dropped, got %s", rec.Billing)
	}
}

// A line larger than bufio's default 64KB must still parse: long-context
// request bodies routinely exceed it.
func TestLongLine(t *testing.T) {
	big := strings.Repeat("x", 300000)
	line := "[DEBUG] 2026/08/08 - 12:00:00 | abcdefghijklmnopqrstuvwxyz | text request body: " +
		`{"model":"m","messages":[{"role":"user","content":"` + big + `"}]}`
	calls := map[string]*Record{}
	scanLines(strings.NewReader(line+"\n"), calls)
	for _, r := range calls {
		r.finalize()
	}
	rec := calls["abcdefghijklmnopqrstuvwxyz"]
	if rec == nil || rec.Model != "m" {
		t.Fatalf("long line dropped: %+v", rec)
	}
	if len([]rune(rec.Preview)) != 200 {
		t.Errorf("preview len = %d, want 200", len([]rune(rec.Preview)))
	}
}
