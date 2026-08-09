package main

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type server struct {
	cfg   Config
	st    *store
	auth  *authenticator
	index []byte
}

func newServer(cfg Config) *server {
	return &server{cfg: cfg, st: newStore(cfg), auth: newAuthenticator(cfg), index: indexHTML}
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
		s.writeJSON(w, 200, map[string]any{"ok": true})
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
	Latency   string `json:"latency"`
	IsStream  bool   `json:"is_stream"`
	HasTools  bool   `json:"has_tools"`
	Quota     *int64 `json:"quota"`
	Preview   string `json:"preview"`
	Errors    int    `json:"errors"`
	MsgCount  int    `json:"msg_count"`
	Turns     int    `json:"turns"`
	ToolCount int    `json:"tool_count"`
}

func toListItem(r *Record) listItem {
	return listItem{
		RequestID: r.RequestID, TS: r.TS, Epoch: r.Epoch,
		Model: r.Model, Status: r.Status, Latency: r.Latency,
		IsStream: r.IsStream, HasTools: r.HasTools, Quota: r.Quota,
		Preview: r.Preview, Errors: len(r.Errors),
		MsgCount: r.MsgCount, Turns: r.Turns, ToolCount: r.ToolCount,
	}
}

func (s *server) handleCalls(w http.ResponseWriter, r *http.Request, user authUser) {
	q := r.URL.Query()
	records := s.st.records(q.Get("refresh") == "1")
	filtered := applyFilters(records, q)

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
	start := (page - 1) * size
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + size
	if end > len(filtered) {
		end = len(filtered)
	}

	models := map[string]bool{}
	tools := map[string]bool{}
	for _, rec := range records {
		if rec.Model != "" {
			models[rec.Model] = true
		}
		for _, t := range rec.ToolNames {
			tools[t] = true
		}
	}

	items := make([]listItem, 0, end-start)
	for _, rec := range filtered[start:end] {
		items = append(items, toListItem(rec))
	}

	s.writeJSON(w, 200, map[string]any{
		"success":   true,
		"total":     len(filtered),
		"page":      page,
		"page_size": size,
		"models":    sortedKeys(models),
		"tools":     sortedKeys(tools),
		"user":      map[string]any{"username": user.Username, "role": user.Role},
		"auth_mode": s.cfg.AuthMode,
		"items":     items,
	})
}

func (s *server) handleCall(w http.ResponseWriter, r *http.Request) {
	rid := r.URL.Query().Get("id")
	for _, rec := range s.st.records(false) {
		if rec.RequestID == rid {
			s.writeJSON(w, 200, map[string]any{"success": true, "data": rec})
			return
		}
	}
	s.writeJSON(w, 404, map[string]any{"success": false, "message": "not found"})
}

func applyFilters(records []*Record, q map[string][]string) []*Record {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	model, status, stream := get("model"), get("status"), get("stream")
	tools, toolName := get("tools"), get("tool_name")
	search := strings.ToLower(get("search"))
	errorsOnly := get("errors") == "1"
	since := int64(atoiDef(get("since"), 0))
	until := int64(atoiDef(get("until"), 0))

	out := make([]*Record, 0, len(records))
	for _, r := range records {
		// only real LLM calls; /api/* health checks have neither body nor billing
		if r.Request.empty() && r.Billing.empty() {
			continue
		}
		if model != "" && r.Model != model {
			continue
		}
		if status == "ok" && (r.Status == nil || *r.Status != 200) {
			continue
		}
		if status == "err" && (r.Status == nil || *r.Status == 200) {
			continue
		}
		if stream == "1" && !r.IsStream {
			continue
		}
		if stream == "0" && r.IsStream {
			continue
		}
		if tools == "1" && !r.HasTools {
			continue
		}
		if toolName != "" && !contains(r.ToolNames, toolName) {
			continue
		}
		if since > 0 && r.Epoch > 0 && r.Epoch < since {
			continue
		}
		if until > 0 && r.Epoch > 0 && r.Epoch > until {
			continue
		}
		if errorsOnly && len(r.Errors) == 0 {
			continue
		}
		if search != "" && !strings.Contains(r.searchBlob, search) {
			continue
		}
		out = append(out, r)
	}
	return out
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
