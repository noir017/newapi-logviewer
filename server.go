package main

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type server struct {
	cfg   Config
	q     *query
	ing   *ingester
	auth  *authenticator
	ch    *channelResolver
	index []byte
}

func newServer(cfg Config) *server {
	arc := newArchive(cfg.ArchiveDir)
	ing := newIngester(cfg.LogDir, arc, cfg.SpoolKeep, cfg.SpoolMaxBytes)
	ch := newChannelResolver(cfg.NewAPIURL, cfg.NewAPIToken)
	return &server{
		cfg: cfg, q: newQuery(arc, cfg.SearchDays).withChannels(ch), ing: ing,
		auth: newAuthenticator(cfg), ch: ch, index: indexHTML,
	}
}

func (s *server) writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"success":false,"message":"encode failed"}`, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	w.Write(b)
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if s.cfg.Base != "" && strings.HasPrefix(path, s.cfg.Base) {
		path = path[len(s.cfg.Base):]
	}
	if path == "" {
		path = "/"
	}

	if path == "/healthz" {
		// Reports write-side health, not just liveness. A pending-only check was
		// green through both of this repo's real outages: reads need no write
		// permission, so a viewer that archives nothing still serves perfect
		// pages. Returns 503 when ingest is broken so a container healthcheck or
		// an uptime monitor actually fires.
		h := s.ing.health()
		code := 200
		if ok, _ := h["archive_ok"].(bool); !ok {
			code = 503
		}
		h["ok"] = code == 200
		s.writeJSON(w, code, h)
		return
	}

	ok, user, reason := s.auth.check(r)
	if !ok {
		if path == "/" || path == "/index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(200)
			w.Write([]byte(strings.ReplaceAll(loginHTML, "{{REASON}}", htmlEscape(reason))))
			return
		}
		s.writeJSON(w, 401, map[string]any{"success": false, "message": reason})
		return
	}

	switch path {
	case "/", "/index.html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(s.index)
	case "/api/calls":
		s.handleCalls(w, r, user)
	case "/api/call":
		s.handleCall(w, r)
	case "/api/stats":
		s.handleStats(w, r)
	default:
		s.writeJSON(w, 404, map[string]any{"success": false, "message": "not found"})
	}
}

// listItem is the row-sized projection of a Record.
//
// The full Record carries the entire request and response bodies. On agent
// traffic one record is ~1MB, so returning 30 of them made /api/calls a 25MB,
// 20-second response just to draw a list. The browser fetches the full record
// from /api/call only when a row is opened.
type listItem struct {
	RequestID string `json:"request_id"`
	TS        string `json:"ts"`
	Epoch     int64  `json:"epoch"`
	Model     string `json:"model"`
	Status    *int   `json:"status"`
	Outcome   string `json:"outcome,omitempty"`
	Latency   string `json:"latency"`
	IsStream  bool   `json:"is_stream"`
	HasTools  bool   `json:"has_tools"`
	Quota     *int64 `json:"quota"`
	Preview   string `json:"preview"`
	Errors    int    `json:"errors"`
	MsgCount  int    `json:"msg_count"`
	Turns     int    `json:"turns"`
	ToolCount int    `json:"tool_count"`

	// Which channel served the call. Channel is the resolved name and may be
	// empty; Upstream is the host from the log and is the fallback label.
	ChannelID *int   `json:"channel_id,omitempty"`
	Channel   string `json:"channel,omitempty"`
	Upstream  string `json:"upstream,omitempty"`
}

func toListItem(r *Record) listItem {
	return listItem{
		RequestID: r.RequestID, TS: r.TS, Epoch: r.Epoch,
		Model: r.Model, Status: r.Status, Latency: r.Latency,
		Outcome:  r.Outcome,
		IsStream: r.IsStream, HasTools: r.HasTools, Quota: r.Quota,
		Preview: r.Preview, Errors: len(r.Errors),
		MsgCount: r.MsgCount, Turns: r.Turns, ToolCount: r.ToolCount,
	}
}

func (s *server) handleCalls(w http.ResponseWriter, r *http.Request, user authUser) {
	q := r.URL.Query()
	page := atoiDef(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	size := atoiDef(q.Get("page_size"), 20)
	if size < 1 {
		size = 1
	}
	if size > 200 {
		size = 200
	}

	f := listFilter{
		Model: q.Get("model"), Status: q.Get("status"), Stream: q.Get("stream"),
		Tools: q.Get("tools"), ToolName: q.Get("tool_name"), Search: q.Get("search"),
		ErrorsOnly: q.Get("errors") == "1",
		Since:      int64(atoiDef(q.Get("since"), 0)),
		Until:      int64(atoiDef(q.Get("until"), 0)),
	}
	items, total, models, tools := s.q.list(f, page, size)

	s.writeJSON(w, 200, map[string]any{
		"success":   true,
		"total":     total,
		"page":      page,
		"page_size": size,
		"models":    models,
		"tools":     tools,
		"user":      map[string]any{"username": user.Username, "role": user.Role},
		"auth_mode": s.cfg.AuthMode,
		"items":     items,
	})
}

func (s *server) handleCall(w http.ResponseWriter, r *http.Request) {
	if rec := s.q.get(r.URL.Query().Get("id")); rec != nil {
		// Safe to mutate: this is a freshly inflated copy, not the archived one.
		if rec.ChannelID != nil {
			rec.ChannelName = s.ch.name(*rec.ChannelID)
		}
		s.writeJSON(w, 200, map[string]any{"success": true, "data": rec})
		return
	}
	s.writeJSON(w, 404, map[string]any{"success": false, "message": "not found"})
}

// resolveRange turns a named range into epoch bounds, in the server's location.
//
// The calendar ranges are computed here rather than in the browser on purpose.
// A relative offset ("last 7 days") is the same instant everywhere, but "today"
// and "this year" are calendar boundaries and depend on which clock draws them.
// There are already two clocks in play - the log's, which names the day files,
// and the viewer's, which computed every Epoch at ingest - and main.go requires
// them to match. Letting the browser supply a third would make the numbers
// depend on where the reader happens to be sitting.
//
// Both bounds are inclusive, matching matchIndex. The upper bound is the last
// second of the final day rather than the next midnight, so a call logged at
// exactly 00:00:00 is not counted in two adjacent ranges.
func resolveRange(name string, now time.Time) (int64, int64, bool) {
	y, m, d := now.Date()
	loc := now.Location()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, loc)
	endOfToday := midnight.AddDate(0, 0, 1).Add(-time.Second)

	switch name {
	case "today":
		return midnight.Unix(), endOfToday.Unix(), true
	case "7d":
		return midnight.AddDate(0, 0, -6).Unix(), endOfToday.Unix(), true
	case "30d":
		return midnight.AddDate(0, 0, -29).Unix(), endOfToday.Unix(), true
	case "ytd":
		return time.Date(y, 1, 1, 0, 0, 0, 0, loc).Unix(), endOfToday.Unix(), true
	}
	return 0, 0, false
}

// parseEpoch is the strict counterpart to atoiDef.
//
// atoiDef swallows a malformed value and returns its default, which for a time
// bound means silently widening the query to all of history - 70ms today, but
// seconds once the archive spans a year, and wrong either way. A stats query
// with a broken range is a bug in the caller, so it is reported as one.
func parseEpoch(v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, errBadRange
	}
	// Epoch seconds, not milliseconds. A caller that forgets to divide by 1000
	// would otherwise ask for a window in the year 58000 and get an empty page
	// with no hint as to why.
	if n > 1<<34 {
		return 0, errBadRange
	}
	return n, nil
}

var errBadRange = errors.New("range out of bounds")

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := time.Now()

	var since, until int64
	name := q.Get("range")
	if name != "" && name != "custom" {
		var ok bool
		since, until, ok = resolveRange(name, now)
		if !ok {
			s.writeJSON(w, 400, map[string]any{
				"success": false, "message": "unknown range: " + name})
			return
		}
	} else {
		var err error
		if since, err = parseEpoch(q.Get("since")); err != nil {
			s.writeJSON(w, 400, map[string]any{"success": false, "message": "bad since"})
			return
		}
		if until, err = parseEpoch(q.Get("until")); err != nil {
			s.writeJSON(w, 400, map[string]any{"success": false, "message": "bad until"})
			return
		}
		if until == 0 {
			until = now.Unix()
		}
		if since == 0 {
			// An open-ended custom range still needs a lower bound to draw an
			// axis against; fall back to the archive's own start.
			since = s.q.earliest(until)
		}
		if until < since {
			s.writeJSON(w, 400, map[string]any{"success": false, "message": "until before since"})
			return
		}
	}

	res := s.q.stats(statsFilter{Since: since, Until: until, Model: q.Get("model")}, now.Location())

	s.writeJSON(w, 200, map[string]any{
		"success": true,
		"data":    res,
		// Echo the resolved window and the clock that resolved it, so the page
		// can state the range it actually drew rather than the one it asked for.
		"range": map[string]any{
			"name": name, "since": since, "until": until, "tz": now.Location().String(),
		},
	})
}

func atoiDef(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	if float64(n) > math.MaxInt32 {
		return def
	}
	return n
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var htmlEscaper = strings.NewReplacer(
	"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")

func htmlEscape(s string) string { return htmlEscaper.Replace(s) }

const loginHTML = `<!doctype html><html lang="zh"><meta charset="utf-8">
<title>需要鉴权 · New API 调用日志</title>
<style>
body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;background:#0f1115;color:#e6e8eb;
display:flex;align-items:center;justify-content:center;height:100vh;margin:0}
.box{background:#171a21;border:1px solid #262b36;border-radius:12px;padding:32px 40px;max-width:480px}
h1{margin:0 0 12px;font-size:18px}p{color:#9aa4b2;line-height:1.7;font-size:14px;margin:8px 0}
a{color:#5b9dff;text-decoration:none}
code{background:#0f1115;padding:2px 6px;border-radius:4px;font-size:12px;word-break:break-all}
</style>
<div class="box">
<h1>需要鉴权</h1>
<p>本页要求 New API 的访问令牌。请求需带上：</p>
<p><code>Authorization: Bearer &lt;access_token&gt;</code></p>
<p style="color:#5c6675;font-size:12px">原因：<code>{{REASON}}</code></p>
</div></html>`
