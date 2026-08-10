package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"https://integrate.api.nvidia.com/v1/chat/completions": "integrate.api.nvidia.com",
		"https://open.bigmodel.cn/api/coding/paas/v4":          "open.bigmodel.cn",
		"http://new-api:3000/v1":                               "new-api:3000",
		"api.example.com/v1":                                   "api.example.com",
		"":                                                     "",
		"   ":                                                  "",
		"::not a url::":                                        "",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// New API has shipped both shapes for this endpoint. Getting it wrong costs the
// channel name silently, so both are pinned.
func TestParseChannelPageBothShapes(t *testing.T) {
	paged := `{"success":true,"data":{"items":[
		{"id":6,"name":"modelscope-17689349223","base_url":"https://api-inference.modelscope.cn"},
		{"id":4,"name":"google-ai-studio","base_url":""}]}}`
	flat := `{"success":true,"data":[
		{"id":6,"name":"modelscope-17689349223","base_url":"https://api-inference.modelscope.cn"},
		{"id":4,"name":"google-ai-studio","base_url":""}]}`

	for label, body := range map[string]string{"paged": paged, "flat": flat} {
		rows, err := parseChannelPage([]byte(body))
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if len(rows) != 2 || rows[0].ID != 6 || rows[0].Name != "modelscope-17689349223" {
			t.Fatalf("%s: got %+v", label, rows)
		}
	}
}

func TestParseChannelPageRejected(t *testing.T) {
	_, err := parseChannelPage([]byte(`{"success":false,"message":"无权进行此操作"}`))
	if err == nil || !strings.Contains(err.Error(), "无权进行此操作") {
		t.Fatalf("want the gateway's reason surfaced, got %v", err)
	}
}

// A channel with no name must still be labelled, or it is indistinguishable
// from one the lookup failed on.
func TestResolverFallsBackToHostForNamelessChannel(t *testing.T) {
	srv := channelServer(t, nil, []channelRow{
		{ID: 1, Name: "", BaseURL: "https://api-inference.modelscope.cn"},
		{ID: 2, Name: "  ", BaseURL: ""},
	})
	defer srv.Close()

	c := newChannelResolver(srv.URL, "tok")
	if got := c.name(1); got != "api-inference.modelscope.cn" {
		t.Errorf("nameless channel: got %q", got)
	}
	if got := c.name(2); got != "" {
		t.Errorf("nameless and addressless channel: got %q, want empty", got)
	}
}

func TestResolverDisabledWithoutToken(t *testing.T) {
	var hits int32
	srv := channelServer(t, &hits, []channelRow{{ID: 1, Name: "should-not-be-fetched"}})
	defer srv.Close()

	c := newChannelResolver(srv.URL, "")
	if got := c.name(1); got != "" {
		t.Errorf("got %q, want empty without a token", got)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("made %d requests with no token configured, want 0", n)
	}
	// The nil resolver is the reingest/test path and must not panic.
	var nilc *channelResolver
	if got := nilc.name(1); got != "" {
		t.Errorf("nil resolver returned %q", got)
	}
}

// Every row of every list request must not cost an HTTP call.
func TestResolverCaches(t *testing.T) {
	var hits int32
	srv := channelServer(t, &hits, []channelRow{{ID: 6, Name: "modelscope"}})
	defer srv.Close()

	c := newChannelResolver(srv.URL, "tok")
	for i := 0; i < 50; i++ {
		if got := c.name(6); got != "modelscope" {
			t.Fatalf("call %d: got %q", i, got)
		}
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("made %d requests for 50 lookups, want 1", n)
	}
}

// A gateway that is down must degrade to no label, not to a timeout per row.
func TestResolverBacksOffWhenGatewayFails(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "boom", 500)
	}))
	defer srv.Close()

	c := newChannelResolver(srv.URL, "tok")
	for i := 0; i < 20; i++ {
		if got := c.name(6); got != "" {
			t.Fatalf("got %q from a failing gateway", got)
		}
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("retried %d times inside the backoff window, want 1", n)
	}
}

// Pagination has to terminate on the last partial page; a resolver that keeps
// asking would hammer the gateway on every refresh.
func TestResolverPaginates(t *testing.T) {
	var pages int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&pages, 1)
		p := r.URL.Query().Get("p")
		var rows []channelRow
		switch p {
		case "0":
			for i := 0; i < channelPageSize; i++ {
				rows = append(rows, channelRow{ID: i, Name: fmt.Sprintf("ch-%d", i)})
			}
		case "1":
			rows = []channelRow{{ID: 999, Name: "last"}}
		default:
			t.Errorf("asked for page %s after a partial page", p)
		}
		writeChannelPage(w, rows)
	}))
	defer srv.Close()

	c := newChannelResolver(srv.URL, "tok")
	if got := c.name(999); got != "last" {
		t.Errorf("got %q, want the row from page 1", got)
	}
	if n := atomic.LoadInt32(&pages); n != 2 {
		t.Errorf("fetched %d pages, want 2", n)
	}
}

func TestResolverSendsBearerToken(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		writeChannelPage(w, []channelRow{{ID: 1, Name: "x"}})
	}))
	defer srv.Close()

	newChannelResolver(srv.URL, "f7f7a15e").name(1)
	if seen != "Bearer f7f7a15e" {
		t.Errorf("Authorization = %q", seen)
	}
}

// ---- index carriage -------------------------------------------------------

// The list reads the index, never the bodies, so the channel has to survive
// into the .idx or the column is blank however good the resolver is.
func TestIndexCarriesChannel(t *testing.T) {
	dir := t.TempDir()
	arc := newArchive(dir)
	id := 6
	st := 200
	rec := &Record{
		RequestID: "20260810rid", TS: "2026/08/10 14:21:48", Status: &st,
		Model: "m", ChannelID: &id, Preview: "p",
		UpstreamURL: "https://integrate.api.nvidia.com/v1/chat/completions",
		Request:     Raw(`{"model":"m"}`),
	}
	if err := arc.Append(rec); err != nil {
		t.Fatal(err)
	}
	arc.Close()

	entries, err := newArchive(dir).index("20260810")
	if err != nil || len(entries) != 1 {
		t.Fatalf("index: %v %d", err, len(entries))
	}
	if entries[0].Chan == nil || *entries[0].Chan != 6 {
		t.Errorf("channel id lost: %+v", entries[0].Chan)
	}
	if entries[0].Up != "integrate.api.nvidia.com" {
		t.Errorf("upstream host = %q", entries[0].Up)
	}

	// And it must reach the wire without a resolver configured.
	item := newQuery(newArchive(dir), 0).entryToItem(entries[0])
	if item.Upstream != "integrate.api.nvidia.com" || item.Channel != "" {
		t.Errorf("listItem = %+v", item)
	}
	if item.ChannelID == nil || *item.ChannelID != 6 {
		t.Errorf("listItem channel id = %v", item.ChannelID)
	}
}

// A call rejected before channel selection - the distributor finding no channel
// for the model - has no channel at all, and must not be labelled with one.
func TestIndexOmitsChannelWhenNoneWasChosen(t *testing.T) {
	dir := t.TempDir()
	arc := newArchive(dir)
	st := 503
	rec := &Record{
		RequestID: "20260810none", TS: "2026/08/10 14:21:48", Status: &st,
		Errors: []LogErr{{TS: "2026/08/10 14:21:48", Msg: "No available channel"}},
	}
	if err := arc.Append(rec); err != nil {
		t.Fatal(err)
	}
	arc.Close()

	entries, _ := newArchive(dir).index("20260810")
	if entries[0].Chan != nil || entries[0].Up != "" {
		t.Fatalf("invented a channel: chan=%v up=%q", entries[0].Chan, entries[0].Up)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "arc-20260810.idx"))
	if strings.Contains(string(raw), `"c":`) || strings.Contains(string(raw), `"u":`) {
		t.Errorf("empty channel fields written to the index: %s", raw)
	}
}

// ---- reindex --------------------------------------------------------------

// The point of the derived index: a column added today can be backfilled onto
// records archived before it existed, without the raw logs.
func TestReindexBackfillsChannel(t *testing.T) {
	dir := t.TempDir()
	arc := newArchive(dir)
	id := 9
	st := 200
	for i := 0; i < 5; i++ {
		rec := &Record{
			RequestID: fmt.Sprintf("20260810r%d", i), TS: "2026/08/10 10:00:00",
			Status: &st, Model: "m", ChannelID: &id, Preview: "p",
			UpstreamURL: "https://integrate.api.nvidia.com/v1/chat/completions",
			Request:     Raw(`{"model":"m"}`),
		}
		if err := arc.Append(rec); err != nil {
			t.Fatal(err)
		}
	}
	arc.Close()

	// Simulate the pre-upgrade index: same offsets, no channel fields. This is
	// exactly what the deployed archive looks like before this change.
	ip := filepath.Join(dir, "arc-20260810.idx")
	entries, err := newArchive(dir).index("20260810")
	if err != nil {
		t.Fatal(err)
	}
	var old strings.Builder
	for _, e := range entries {
		e.Chan, e.Up = nil, ""
		line, _ := json.Marshal(e)
		old.Write(line)
		old.WriteByte('\n')
	}
	if err := os.WriteFile(ip, []byte(old.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := newArchive(dir).index("20260810")
	if before[0].Chan != nil {
		t.Fatal("fixture is not a pre-upgrade index")
	}

	n, err := reindexDay(dir, "20260810")
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if n != 5 {
		t.Errorf("rebuilt %d entries, want 5", n)
	}

	after, err := newArchive(dir).index("20260810")
	if err != nil || len(after) != 5 {
		t.Fatalf("index after reindex: %v %d", err, len(after))
	}
	for i, e := range after {
		if e.Chan == nil || *e.Chan != 9 || e.Up != "integrate.api.nvidia.com" {
			t.Errorf("entry %d not backfilled: chan=%v up=%q", i, e.Chan, e.Up)
		}
		// Offsets are the one thing reindex cannot recompute, so they must be
		// carried over untouched - a wrong one silently serves another record.
		if e.Off != before[i].Off || e.Len != before[i].Len {
			t.Errorf("entry %d offset moved: %d+%d -> %d+%d",
				i, before[i].Off, before[i].Len, e.Off, e.Len)
		}
		// And the rebuilt index must still address the right member.
		rec, err := newArchive(dir).fetch("20260810", e)
		if err != nil || rec.RequestID != e.RID {
			t.Errorf("entry %d unreadable after reindex: %v", i, err)
		}
	}
	if _, err := os.Stat(ip + ".rebuild"); !os.IsNotExist(err) {
		t.Error("temporary index left behind")
	}
}

// A failure must leave the old index in place rather than a truncated one.
func TestReindexLeavesIndexIntactOnFailure(t *testing.T) {
	dir := t.TempDir()
	arc := newArchive(dir)
	st := 200
	if err := arc.Append(&Record{
		RequestID: "20260810ok", TS: "2026/08/10 10:00:00", Status: &st,
		Model: "m", Request: Raw(`{"model":"m"}`),
	}); err != nil {
		t.Fatal(err)
	}
	arc.Close()

	ip := filepath.Join(dir, "arc-20260810.idx")
	good, _ := os.ReadFile(ip)

	// Point an entry at bytes that are not a gzip member.
	entries, _ := newArchive(dir).index("20260810")
	entries[0].Off = 1
	line, _ := json.Marshal(entries[0])
	if err := os.WriteFile(ip, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	broken, _ := os.ReadFile(ip)

	if _, err := reindexDay(dir, "20260810"); err == nil {
		t.Fatal("reindex accepted an unreadable member")
	}
	now, _ := os.ReadFile(ip)
	if string(now) != string(broken) {
		t.Errorf("index was modified by a failed reindex")
	}
	if _, err := os.Stat(ip + ".rebuild"); !os.IsNotExist(err) {
		t.Error("temporary index left behind after a failure")
	}
	_ = good
}

// ---- helpers ---------------------------------------------------------------

func writeChannelPage(w http.ResponseWriter, rows []channelRow) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"data":    map[string]any{"items": rows},
	})
}

func channelServer(t *testing.T, hits *int32, rows []channelRow) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		if !strings.HasPrefix(r.URL.Path, "/api/channel") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		writeChannelPage(w, rows)
	}))
}
