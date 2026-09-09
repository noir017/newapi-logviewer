package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// receiverFor builds a viewer configured as the archiving pod: it receives
// pushes into its own ARCHIVE_DIR and answers the list API from it. Its spool
// points at an empty directory so its own ingester contributes nothing and the
// records under test can only have arrived by push.
func receiverFor(t *testing.T, token string, pods ...string) (*server, *httptest.Server, string) {
	t.Helper()
	arcDir := t.TempDir()
	cfg := Config{
		LogDir: t.TempDir(), ArchiveDir: arcDir, AuthMode: "none",
		PushToken: token, PushPods: pods, PushMaxBytes: 8 << 20,
	}
	srv := newServer(cfg)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return srv, ts, arcDir
}

func pushRecord(rid, ts, model string, quota int64) *Record {
	st := 200
	return &Record{
		RequestID: rid, TS: ts, Epoch: epochOf(ts), Status: &st,
		Model: model, Outcome: outcomeOK, Quota: &quota,
		Request: Raw(`{"model":"` + model + `","messages":[{"role":"user","content":` +
			`"` + strings.Repeat("push me ", 40) + `"}]}`),
		Response: Raw(`{"choices":[{"message":{"content":"ok"}}]}`),
		Preview:  "push me",
	}
}

func epochOf(ts string) int64 {
	t, err := time.ParseInLocation("2006/01/02 15:04:05", ts, time.Local)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// canonJSON re-encodes a body with its keys sorted, so two bodies can be
// compared for content rather than for spelling.
func canonJSON(t *testing.T, raw Raw) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// callsTotal reads the list API's match count, which is what the UI shows and
// what a double-counted record would inflate.
func callsTotal(t *testing.T, srv *server) int {
	t.Helper()
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("GET", "/api/calls?page_size=200", nil))
	if w.Code != 200 {
		t.Fatalf("/api/calls -> %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	return res.Total
}

// The whole point of the feature: records folded on one pod are archived by
// another, are visible in its list, and can be read back in full.
func TestPushRoundTrip(t *testing.T) {
	srv, ts, arcDir := receiverFor(t, "s3cret")
	p := newPusher(Config{
		PushURL: pushEndpoint(ts.URL), PodName: "oracle", PushToken: "s3cret",
		PushTimeout: 10 * time.Second,
	})
	if p == nil {
		t.Fatal("newPusher returned nil for a set PUSH_URL")
	}

	recs := []*Record{
		pushRecord("20260901aaaaaaaaaaaaaaaa", "2026/09/01 10:00:00", "gpt-4o", 120),
		pushRecord("20260901bbbbbbbbbbbbbbbb", "2026/09/01 10:00:05", "claude", 340),
	}
	for _, r := range recs {
		r.Pod = "oracle"
	}
	if err := p.send(recs); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := callsTotal(t, srv); got != 2 {
		t.Fatalf("receiver lists %d calls, want 2", got)
	}

	// The record must come back intact - request body included, which on the
	// wire went through gzip and on disk through the v2 blob pool.
	rec := newQuery(newArchive(arcDir), 0).get("20260901aaaaaaaaaaaaaaaa")
	if rec == nil {
		t.Fatal("pushed record not readable from the receiver's archive")
	}
	// Semantically, not byte for byte: v2's blob lifting re-marshals the top
	// level object, so the keys come back sorted (as they already do for any
	// locally archived record).
	if got, want := canonJSON(t, rec.Request), canonJSON(t, recs[0].Request); got != want {
		t.Errorf("request body changed in transit:\n got %s\nwant %s", got, want)
	}
	if rec.Model != "gpt-4o" || rec.Quota == nil || *rec.Quota != 120 {
		t.Errorf("record fields lost: model=%q quota=%v", rec.Model, rec.Quota)
	}
	if rec.Pod != "oracle" {
		t.Errorf("pod label = %q, want oracle", rec.Pod)
	}
}

// A push that lands and then loses its ACK is re-sent. It must not produce a
// second copy: not a second index entry, and above all not a second
// contribution to the spend total.
func TestPushIsIdempotent(t *testing.T) {
	srv, ts, arcDir := receiverFor(t, "s3cret")
	p := newPusher(Config{
		PushURL: pushEndpoint(ts.URL), PodName: "oracle", PushToken: "s3cret",
		PushTimeout: 10 * time.Second,
	})
	recs := []*Record{
		pushRecord("20260901aaaaaaaaaaaaaaaa", "2026/09/01 10:00:00", "gpt-4o", 120),
		pushRecord("20260901bbbbbbbbbbbbbbbb", "2026/09/01 10:00:05", "claude", 340),
	}

	for i := 0; i < 3; i++ {
		if err := p.send(recs); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if got := callsTotal(t, srv); got != 2 {
		t.Errorf("after 3 identical pushes the receiver lists %d calls, want 2", got)
	}

	entries, err := newArchive(arcDir).index("20260901")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("index has %d entries after 3 identical pushes, want 2", len(entries))
	}

	// Spend is the number a duplicate would corrupt invisibly, so assert on it
	// directly rather than trusting the entry count to imply it.
	res := newQuery(newArchive(arcDir), 0).stats(statsFilter{
		Since: epochOf("2026/09/01 00:00:00"), Until: epochOf("2026/09/01 23:59:59"),
	}, time.Local)
	if res.Requests != 2 {
		t.Errorf("stats counted %d requests, want 2", res.Requests)
	}
	if res.Quota != 460 {
		t.Errorf("stats totalled %d quota, want 460 (120+340, each counted once)", res.Quota)
	}

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	var h struct {
		Received  int64 `json:"push_received"`
		Duplicate int64 `json:"push_duplicate"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if h.Received != 2 || h.Duplicate != 4 {
		t.Errorf("healthz reports received=%d duplicate=%d, want 2 and 4", h.Received, h.Duplicate)
	}
}

// The primary pod folds its own log AND receives pushes into the same day file.
// Neither writer may let a duplicate past the other, in either order.
func TestPushDeduplicatesAgainstLocalAppends(t *testing.T) {
	_, ts, arcDir := receiverFor(t, "s3cret")
	p := newPusher(Config{
		PushURL: pushEndpoint(ts.URL), PushToken: "s3cret", PodName: "oracle",
		PushTimeout: 10 * time.Second,
	})

	// The receiving pod archives a record of its own first, through the plain
	// local path...
	arc := newArchive(arcDir)
	local := pushRecord("20260901cccccccccccccccc", "2026/09/01 11:00:00", "local", 7)
	if err := arc.Append(local); err != nil {
		t.Fatal(err)
	}
	arc.Close()

	// ...and the same id then arrives by push, which must be recognised.
	if err := p.send([]*Record{local}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// The reverse order too: pushed first, then appended locally into the day
	// whose dedup set is already live.
	pushed := pushRecord("20260901dddddddddddddddd", "2026/09/01 11:05:00", "remote", 9)
	if err := p.send([]*Record{pushed}); err != nil {
		t.Fatalf("send: %v", err)
	}

	entries, err := newArchive(arcDir).index("20260901")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("index has %d entries, want 2 (one local, one pushed)", len(entries))
	}
}

// Everything the receiver must refuse. An unauthenticated write into the
// permanent store is the one failure mode with no way back.
func TestPushRejections(t *testing.T) {
	_, ts, _ := receiverFor(t, "s3cret", "oracle")
	body := func() io.Reader {
		b, err := encodePush([]*Record{
			pushRecord("20260901eeeeeeeeeeeeeeee", "2026/09/01 12:00:00", "m", 1)})
		if err != nil {
			t.Fatal(err)
		}
		return bytes.NewReader(b)
	}
	post := func(token, pod string) int {
		req, err := http.NewRequest("POST", ts.URL+pushPath, body())
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Encoding", "gzip")
		if token != "" {
			req.Header.Set(pushTokenHeader, token)
		}
		if pod != "" {
			req.Header.Set(pushPodHeader, pod)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	if code := post("", "oracle"); code != 401 {
		t.Errorf("no token -> %d, want 401", code)
	}
	if code := post("wrong", "oracle"); code != 401 {
		t.Errorf("wrong token -> %d, want 401", code)
	}
	if code := post("s3cret", ""); code != 400 {
		t.Errorf("no pod name -> %d, want 400", code)
	}
	if code := post("s3cret", "impostor"); code != 403 {
		t.Errorf("pod outside PUSH_PODS -> %d, want 403", code)
	}
	if code := post("s3cret", "oracle"); code != 200 {
		t.Errorf("valid push -> %d, want 200", code)
	}

	// GET is not a push, and a receiver with no PUSH_TOKEN must refuse rather
	// than accept anonymous writes.
	res, err := http.Get(ts.URL + pushPath)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 405 {
		t.Errorf("GET /api/push -> %d, want 405", res.StatusCode)
	}

	_, off, _ := receiverFor(t, "")
	req, _ := http.NewRequest("POST", off.URL+pushPath, body())
	req.Header.Set(pushTokenHeader, "anything")
	req.Header.Set(pushPodHeader, "oracle")
	res2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode != 503 {
		t.Errorf("push to a receiver with no PUSH_TOKEN -> %d, want 503", res2.StatusCode)
	}
}

// The wire format is gzipped NDJSON. It is asserted here rather than left to
// the round trip because compression is the reason this link is affordable at
// all, and a silently uncompressed sender would still pass every other test.
func TestPushBodyIsGzippedNDJSON(t *testing.T) {
	var enc, ctype string
	var raw []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enc, ctype = r.Header.Get("Content-Encoding"), r.Header.Get("Content-Type")
		raw, _ = io.ReadAll(r.Body)
		json.NewEncoder(w).Encode(pushAck{Success: true, Stored: 2})
	}))
	defer ts.Close()

	p := newPusher(Config{PushURL: pushEndpoint(ts.URL), PodName: "oracle", PushTimeout: 5 * time.Second})
	recs := []*Record{
		pushRecord("20260901aaaaaaaaaaaaaaaa", "2026/09/01 10:00:00", "gpt-4o", 1),
		pushRecord("20260901bbbbbbbbbbbbbbbb", "2026/09/01 10:00:05", "claude", 2),
	}
	if err := p.send(recs); err != nil {
		t.Fatalf("send: %v", err)
	}
	if enc != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", enc)
	}
	if ctype != pushContentType {
		t.Errorf("Content-Type = %q, want %s", ctype, pushContentType)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= len(plain) {
		t.Errorf("compression gained nothing: %d bytes on the wire for %d of JSON", len(raw), len(plain))
	}
	lines := strings.Split(strings.TrimRight(string(plain), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("body has %d lines, want one per record", len(lines))
	}
	for i, l := range lines {
		var rec Record
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("line %d is not a record: %v", i, err)
		}
		if rec.RequestID != recs[i].RequestID {
			t.Errorf("line %d carries %q, want %q", i, rec.RequestID, recs[i].RequestID)
		}
	}
}

// A 200 from something that is not our receiver must not be taken for an ACK:
// between these pods sit a reverse proxy and the internet, and consuming the
// spool on a proxy's error page would lose the batch silently.
func TestPushRequiresRealAck(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"html error page": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("<html>200 OK</html>"))
		},
		"success false": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(pushAck{Success: false})
		},
		"short count": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(pushAck{Success: true, Stored: 1})
		},
	}
	for name, h := range cases {
		ts := httptest.NewServer(h)
		p := newPusher(Config{PushURL: pushEndpoint(ts.URL), PodName: "oracle", PushTimeout: 5 * time.Second})
		err := p.send([]*Record{
			pushRecord("20260901aaaaaaaaaaaaaaaa", "2026/09/01 10:00:00", "m", 1),
			pushRecord("20260901bbbbbbbbbbbbbbbb", "2026/09/01 10:00:05", "m", 1),
		})
		if err == nil {
			t.Errorf("%s: accepted as an ACK", name)
		}
		ts.Close()
	}
}

// While a push is failing the spool must not be consumed: it is the only
// buffer, and the read offsets are in memory, so bytes read but not
// acknowledged are lost on a restart.
func TestPushFailureHoldsTheSpool(t *testing.T) {
	spool, arcDir := t.TempDir(), t.TempDir()
	logPath := filepath.Join(spool, "gateway.log")
	sample, err := os.ReadFile("testdata/sample.log")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, sample, 0o644); err != nil {
		t.Fatal(err)
	}

	// A receiver that fails until told otherwise, so the sender's whole
	// backoff-and-hold path runs against a real HTTP round trip.
	down := true
	var attempts int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if down {
			http.Error(w, "receiver down", 502)
			return
		}
		var n int
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		dec := json.NewDecoder(zr)
		for {
			var rec Record
			if err := dec.Decode(&rec); err == io.EOF {
				break
			} else if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			n++
		}
		json.NewEncoder(w).Encode(pushAck{Success: true, Stored: n})
	}))
	defer ts.Close()

	arc := newArchive(arcDir)
	ing := newIngester(spool, arc, time.Hour, 0).withPusher(newPusher(Config{
		PushURL: pushEndpoint(ts.URL), PodName: "oracle", PushTimeout: 5 * time.Second,
	}))

	ing.once()
	if ing.push.fails != 1 {
		t.Fatalf("after a failed push, fails = %d, want 1", ing.push.fails)
	}
	waiting := len(ing.pending)
	if waiting == 0 {
		t.Fatal("nothing pending after a failed push - records were dropped")
	}
	consumed := ing.offsets[logPath]
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("spool file removed while the push was failing: %v", err)
	}
	// Nothing may be archived locally in push mode, failing or not.
	if days := arc.days(); len(days) != 0 {
		t.Errorf("push mode wrote a local archive: %v", days)
	}

	// New lines arrive while the receiver is still down. The ingester must not
	// read them, and must not retry before the backoff has elapsed.
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(sample)
	f.Close()

	before := attempts
	ing.once()
	if attempts != before {
		t.Errorf("retried inside the backoff window (%d attempts, want %d)", attempts, before)
	}
	if got := ing.offsets[logPath]; got != consumed {
		t.Errorf("consumed %d more spool bytes while the receiver was down", got-consumed)
	}
	if len(ing.pending) != waiting {
		t.Errorf("pending changed from %d to %d while paused", waiting, len(ing.pending))
	}

	// The receiver comes back. The held records go out, and only then is the
	// spool consumed again.
	down = false
	ing.push.nextTry = time.Time{}
	ing.once()
	if ing.push.fails != 0 {
		t.Fatalf("still failing after the receiver recovered: %v", ing.push.lastErr)
	}
	if ing.push.sent != int64(waiting) {
		t.Errorf("sent %d records, want the %d that were held", ing.push.sent, waiting)
	}
	if len(ing.pending) != 0 {
		t.Errorf("%d records still pending after an ACK", len(ing.pending))
	}
	ing.once() // now unpaused, so the appended lines are read
	if got := ing.offsets[logPath]; got <= consumed {
		t.Errorf("spool not consumed after recovery: offset %d, was %d", got, consumed)
	}
}

// Backoff has to grow and to stop growing: the minimum covers a redeploy of the
// primary pod, and the ceiling is what makes the sender notice recovery on its
// own during a real outage.
func TestPushBackoff(t *testing.T) {
	p := &pusher{}
	now := time.Now()
	var last time.Duration
	for i := 1; i <= 12; i++ {
		p.failed(now, io.ErrUnexpectedEOF)
		d := p.nextTry.Sub(now)
		if d < pushBackoffMin || d > pushBackoffMax {
			t.Fatalf("attempt %d: delay %s outside [%s,%s]", i, d, pushBackoffMin, pushBackoffMax)
		}
		if i > 1 && d < last {
			t.Errorf("attempt %d: delay shrank from %s to %s", i, last, d)
		}
		last = d
		if p.ready(now) {
			t.Errorf("attempt %d: ready immediately after a failure", i)
		}
		if !p.ready(now.Add(d)) {
			t.Errorf("attempt %d: not ready once the delay elapsed", i)
		}
	}
	if last != pushBackoffMax {
		t.Errorf("backoff settled at %s, want the %s ceiling", last, pushBackoffMax)
	}
	p.succeeded(3)
	if p.failing() || !p.ready(now) || p.sent != 3 {
		t.Errorf("success did not clear the backoff: %+v", p)
	}
}

// PUSH_URL is written by hand, and the viewer's mount root and the endpoint
// itself are equally natural things to write. Both must configure the same
// thing: getting it wrong is a silent 404 loop.
func TestPushEndpointNormalization(t *testing.T) {
	cases := map[string]string{
		"":                                   "",
		"  ":                                 "",
		"https://host/logviewer":             "https://host/logviewer/api/push",
		"https://host/logviewer/":            "https://host/logviewer/api/push",
		"https://host/logviewer/api/push":    "https://host/logviewer/api/push",
		"https://host/logviewer/api/push/":   "https://host/logviewer/api/push",
		"http://127.0.0.1:7070":              "http://127.0.0.1:7070/api/push",
		" https://host/logviewer/api/push  ": "https://host/logviewer/api/push",
	}
	for in, want := range cases {
		if got := pushEndpoint(in); got != want {
			t.Errorf("pushEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
	// And an unset PUSH_URL must leave the ingester on its single-machine path.
	if newPusher(Config{}) != nil {
		t.Error("newPusher returned a pusher for an unset PUSH_URL")
	}
}

// The push settings are what an operator actually edits, so cover the env
// plumbing itself: a typo'd variable name is a viewer that silently stays in
// single-machine mode.
func TestPushConfigFromEnv(t *testing.T) {
	for k, v := range map[string]string{
		"PUSH_URL": "https://primary.example/logviewer/", "POD_NAME": "oracle",
		"PUSH_TOKEN": "from-the-environment", "PUSH_PODS": "oracle, unraid ,",
		"PUSH_TIMEOUT_SEC": "12", "PUSH_MAX_MB": "3",
	} {
		t.Setenv(k, v)
	}
	cfg := loadConfig()
	if cfg.PushURL != "https://primary.example/logviewer/api/push" {
		t.Errorf("PushURL = %q", cfg.PushURL)
	}
	if cfg.PodName != "oracle" || cfg.PushToken != "from-the-environment" {
		t.Errorf("pod = %q token set = %v", cfg.PodName, cfg.PushToken != "")
	}
	if len(cfg.PushPods) != 2 || cfg.PushPods[0] != "oracle" || cfg.PushPods[1] != "unraid" {
		t.Errorf("PushPods = %q, want the list trimmed and blanks dropped", cfg.PushPods)
	}
	if cfg.PushTimeout != 12*time.Second || cfg.PushMaxBytes != 3*1024*1024 {
		t.Errorf("timeout = %s max = %d", cfg.PushTimeout, cfg.PushMaxBytes)
	}

	// Unset, every push field is inert - and POD_NAME still resolves, since a
	// push carries it and the receiver requires it.
	for _, k := range []string{"PUSH_URL", "POD_NAME", "PUSH_TOKEN", "PUSH_PODS",
		"PUSH_TIMEOUT_SEC", "PUSH_MAX_MB"} {
		os.Unsetenv(k)
	}
	bare := loadConfig()
	if bare.PushURL != "" || bare.PushToken != "" || bare.PushPods != nil {
		t.Errorf("unset push config is not inert: %+v", bare)
	}
	if bare.PodName == "" {
		t.Error("POD_NAME unset did not fall back to the hostname")
	}
}

// A gzip bomb must not be decoded into memory just because it is small on the
// wire. The endpoint reads the batch to append it, so the inflated bound is the
// one that protects it.
func TestPushInflationIsBounded(t *testing.T) {
	arcDir := t.TempDir()
	srv := newServer(Config{
		LogDir: t.TempDir(), ArchiveDir: arcDir, AuthMode: "none",
		PushToken: "s3cret", PushMaxBytes: 1024,
	})
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	// One valid record, then far more zeroes than the inflated bound allows.
	json.NewEncoder(zw).Encode(pushRecord("20260901ffffffffffffffff", "2026/09/01 13:00:00", "m", 1))
	zw.Write(bytes.Repeat([]byte("0"), 1024*pushInflateRatio*2))
	zw.Close()

	req := httptest.NewRequest("POST", pushPath, bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set(pushTokenHeader, "s3cret")
	req.Header.Set(pushPodHeader, "oracle")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code == 200 {
		t.Errorf("an over-long inflated batch was accepted: %s", w.Body.String())
	}
}
