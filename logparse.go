package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// new-api writes one line per event, all tagged with the request id, e.g.
//
//	[DEBUG] 2026/08/08 - 12:16:33 | <rid> | text request body: {...}
//	[DEBUG] 2026/08/08 - 12:16:36 | <rid> | upstream response body: {...}
//	[INFO]  2026/08/08 - 12:16:36 | <rid> | record consume log: userId=1, params={...}
//	[GIN]   2026/08/08 - 12:16:36 | relay | <rid> | 200 | 3.05s | 1.2.3.4 | POST /v1/...
//
// Events for one call are interleaved with other calls, so we group by rid.
// Lines whose id column is SYSTEM (background jobs) carry no rid and are skipped.

var (
	lineRE = regexp.MustCompile(`^\[([A-Z]+)\]\s+(\d{4}/\d{2}/\d{2})\s+-\s+(\d{2}:\d{2}:\d{2})\s+\|\s+(.*)$`)
	// a request id is 24+ alphanumerics; "SYSTEM"/"relay"/"api" are not ids
	ridRE = regexp.MustCompile(`^([A-Za-z0-9]{24,})\s+\|\s+(.*)$`)
	ginRE = regexp.MustCompile(`^(relay|api)\s+\|\s+([A-Za-z0-9]{24,})\s+\|\s+(\d{3})\s+\|\s+(\S+)\s+\|\s+(\S+)\s+\|\s+([A-Z]+)\s+(\S+)`)
)

type LogErr struct {
	TS  string `json:"ts"`
	Msg string `json:"msg"`
}

type Usage struct {
	PromptTokens     *int `json:"prompt_tokens,omitempty"`
	CompletionTokens *int `json:"completion_tokens,omitempty"`
	TotalTokens      *int `json:"total_tokens,omitempty"`
	PromptDetails    *struct {
		CachedTokens *int `json:"cached_tokens,omitempty"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionDetails *struct {
		ReasoningTokens *int `json:"reasoning_tokens,omitempty"`
	} `json:"completion_tokens_details,omitempty"`
}

type ToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// Record is one LLM call, assembled from every log line sharing its request id.
// Field names and shapes are the wire contract with the browser UI.
type Record struct {
	RequestID string `json:"request_id"`
	TS        string `json:"ts"`
	Epoch     int64  `json:"epoch"`

	Request  Raw `json:"request"`
	Response Raw `json:"response"`
	Billing  Raw `json:"billing"`

	UpstreamURL string   `json:"upstream_url"`
	Status      *int     `json:"status"`
	Latency     string   `json:"latency"`
	IP          string   `json:"ip"`
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	Kind        string   `json:"kind"`
	StreamEnd   string   `json:"stream_end"`
	Errors      []LogErr `json:"errors"`

	Model    string `json:"model"`
	IsStream bool   `json:"is_stream"`
	HasTools bool   `json:"has_tools"`

	StreamContent   string     `json:"stream_content"`
	StreamReasoning string     `json:"stream_reasoning"`
	StreamToolCalls []ToolCall `json:"stream_tool_calls"`
	Usage           *Usage     `json:"usage"`

	// Streaming shape, kept after the chunk lines themselves are folded away.
	// Measured on production, chunk lines are 76% of all log bytes and ~98% of
	// each one is a repeated envelope, so the archive stores the concatenated
	// text plus these two numbers instead of the chunks. ChunkCount is what
	// makes "did this stream at all, and how finely" answerable afterwards;
	// FirstChunkMS is time-to-first-token, the one latency number the envelope
	// timestamps were actually good for.
	ChunkCount   int `json:"chunk_count"`
	FirstChunkMS int `json:"first_chunk_ms,omitempty"`

	// UnknownChunks samples stream lines the parser could not interpret, with
	// UnknownCount recording how many there were. Folding is lossy by design,
	// so an unrecognised provider format must leave evidence rather than
	// disappear: a non-zero UnknownCount with empty StreamContent is the
	// signature of a shape this parser has not learned yet.
	UnknownChunks []string `json:"unknown_chunks,omitempty"`
	UnknownCount  int      `json:"unknown_count,omitempty"`

	Quota           *int64   `json:"quota"`
	ModelRatio      *float64 `json:"model_ratio"`
	CompletionRatio *float64 `json:"completion_ratio"`
	FRT             *float64 `json:"frt"`
	ChannelID       *int     `json:"channel_id"`
	TokenName       string   `json:"token_name"`

	ToolNames   []string `json:"tool_names"`
	CalledTools []string `json:"called_tools"`
	Preview     string   `json:"preview"`
	Incomplete  bool     `json:"incomplete"`

	// Stalled marks a call archived without its GIN line: the client
	// disconnected, the gateway restarted, or the response never finished.
	// Distinct from Incomplete, which means the request body itself was
	// missing from the log - the two need different words in the UI.
	Stalled bool `json:"stalled,omitempty"`

	// Agent transcripts arrive as one call carrying the whole conversation, so
	// the list row needs to say how big it is. Turns = assistant messages,
	// which is the number of model round-trips the transcript represents.
	MsgCount  int `json:"msg_count"`
	Turns     int `json:"turns"`
	ToolCount int `json:"tool_count"`

	// sawChunks records that this call streamed, even after chunks have been
	// drained by a previous finalize pass.
	sawChunks bool

	// lastSeen is when a line for this call was last read, used by the
	// ingester to decide a call has stalled and will never complete.
	lastSeen time.Time

	chunks []streamChunk // dropped by finalize
}

// ---- partial views over the raw bodies -------------------------------------

type reqView struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
		Name string `json:"name"` // bare-schema tools (no "function" wrapper)
	} `json:"tools"`
}

type respView struct {
	Choices []struct {
		Message struct {
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

type billView struct {
	ModelName        string `json:"model_name"`
	Quota            *int64 `json:"quota"`
	ChannelID        *int   `json:"channel_id"`
	TokenName        string `json:"token_name"`
	PromptTokens     *int   `json:"prompt_tokens"`
	CompletionTokens *int   `json:"completion_tokens"`
	Other            *struct {
		ModelRatio      *float64 `json:"model_ratio"`
		CompletionRatio *float64 `json:"completion_ratio"`
		FRT             *float64 `json:"frt"`
	} `json:"other"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string     `json:"content"`
			ReasoningContent string     `json:"reasoning_content"`
			ToolCalls        []ToolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`

	// Gemini's native streaming shape, which New API logs verbatim for
	// Google-family channels rather than translating to OpenAI's.
	//
	// This matters far more here than it did when the raw log was kept: folding
	// discards the chunk lines, so a shape we cannot parse is not merely
	// displayed oddly, it is lost. Found in production - a gemma-4-31b-it call
	// archived 17 chunks and 338 completion tokens with an empty body.
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
				// Gemini marks chain-of-thought parts; they belong in the
				// reasoning field, not mixed into the answer.
				Thought bool `json:"thought"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     *int `json:"promptTokenCount"`
		CandidatesTokenCount *int `json:"candidatesTokenCount"`
		TotalTokenCount      *int `json:"totalTokenCount"`
		ThoughtsTokenCount   *int `json:"thoughtsTokenCount"`
	} `json:"usageMetadata"`
}

// text pulls the visible and reasoning text out of a chunk, whichever wire
// format it arrived in.
func (ch *streamChunk) text() (content, reasoning string, tools []ToolCall) {
	var c, r strings.Builder
	for _, ci := range ch.Choices {
		c.WriteString(ci.Delta.Content)
		r.WriteString(ci.Delta.ReasoningContent)
		tools = append(tools, ci.Delta.ToolCalls...)
	}
	for _, cand := range ch.Candidates {
		for _, p := range cand.Content.Parts {
			if p.Thought {
				r.WriteString(p.Text)
			} else {
				c.WriteString(p.Text)
			}
		}
	}
	return c.String(), r.String(), tools
}

// usage normalises whichever usage block the chunk carried.
func (ch *streamChunk) usage() *Usage {
	if ch.Usage != nil {
		return ch.Usage
	}
	if ch.UsageMetadata == nil {
		return nil
	}
	u := &Usage{
		PromptTokens:     ch.UsageMetadata.PromptTokenCount,
		CompletionTokens: ch.UsageMetadata.CandidatesTokenCount,
		TotalTokens:      ch.UsageMetadata.TotalTokenCount,
	}
	return u
}

// hasPayload reports whether the chunk carried anything worth counting.
// Keepalives and comment lines decode fine but say nothing.
func (ch *streamChunk) hasPayload() bool {
	return len(ch.Choices) > 0 || len(ch.Candidates) > 0 || ch.Usage != nil || ch.UsageMetadata != nil
}

// ---- parsing ---------------------------------------------------------------

func blank(rid string) *Record {
	return &Record{RequestID: rid, Errors: []LogErr{}}
}

// ParseFiles reads each file once, newest content last, and groups by rid.
// limitBytes tails huge files: full history is rarely needed and reading a
// multi-GB log would blow up memory.
func ParseFiles(paths []string, limitBytes int64) map[string]*Record {
	calls := map[string]*Record{}
	for _, p := range paths {
		parseOne(p, limitBytes, calls)
	}
	for _, rec := range calls {
		rec.finalize()
	}
	return calls
}

func parseOne(path string, limitBytes int64, calls map[string]*Record) {
	fh, err := os.Open(path)
	if err != nil {
		return
	}
	defer fh.Close()

	if st, err := fh.Stat(); err == nil && limitBytes > 0 && st.Size() > limitBytes {
		if _, err := fh.Seek(st.Size()-limitBytes, 0); err == nil {
			br := bufio.NewReader(fh)
			br.ReadString('\n') // discard the partial line we landed in
			scanLines(br, calls)
			return
		}
	}
	scanLines(bufio.NewReaderSize(fh, 1<<16), calls)
}

// parseRange reads path from byte offset `from` to EOF, merging events into
// calls. Records it touched are re-finalized, so derived fields stay correct
// when a call's lines arrive across several reads (request now, response and
// billing seconds later).
//
// Returns true if any record was created or changed.
func parseRange(path string, from int64, partial bool, calls map[string]*Record) bool {
	fh, err := os.Open(path)
	if err != nil {
		return false
	}
	defer fh.Close()

	if from > 0 {
		if _, err := fh.Seek(from, 0); err != nil {
			return false
		}
	}
	br := bufio.NewReaderSize(fh, 1<<16)
	if partial {
		// Only when we deliberately jumped into the middle of the file (the
		// LIMIT_MB tail). A resumed read starts exactly at a line boundary -
		// discarding a line there would drop a real event.
		br.ReadString('\n')
	}

	touched := map[string]bool{}
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 0, 1<<16), 16<<20)
	for sc.Scan() {
		if rid := handleLine(sc.Text(), calls); rid != "" {
			touched[rid] = true
		}
	}
	now := time.Now()
	for rid := range touched {
		if rec := calls[rid]; rec != nil {
			rec.lastSeen = now
			rec.finalize()
		}
	}
	return len(touched) > 0
}

func scanLines(r io.Reader, calls map[string]*Record) {
	sc := bufio.NewScanner(r)
	// request bodies with long contexts routinely exceed the 64KB default
	sc.Buffer(make([]byte, 0, 1<<16), 16<<20)
	for sc.Scan() {
		handleLine(sc.Text(), calls)
	}
}

// handleLine merges one log line into calls and returns the request id it
// touched (empty when the line carries none), so an incremental read knows
// which records need re-deriving.
func handleLine(line string, calls map[string]*Record) string {
	m := lineRE.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	level, rest := m[1], m[4]
	ts := m[2] + " " + m[3]

	if level == "GIN" {
		g := ginRE.FindStringSubmatch(rest)
		if g == nil {
			return ""
		}
		rec := get(calls, g[2])
		if n, err := strconv.Atoi(g[3]); err == nil {
			rec.Status = &n
		}
		rec.Kind, rec.Latency, rec.IP, rec.Method, rec.Path = g[1], g[4], g[5], g[6], g[7]
		if rec.TS == "" {
			rec.TS = ts
		}
		return g[2]
	}

	r := ridRE.FindStringSubmatch(rest)
	if r == nil {
		return "" // SYSTEM / unkeyed background lines
	}
	rec := get(calls, r[1])
	msg := r[2]
	if rec.TS == "" {
		rec.TS = ts
	}

	switch {
	case strings.HasPrefix(msg, "text request body:"):
		rec.Request = parseRaw(strings.TrimSpace(msg[len("text request body:"):]))
	case strings.HasPrefix(msg, "upstream response body:"):
		rec.Response = parseRaw(strings.TrimSpace(msg[len("upstream response body:"):]))
	case strings.HasPrefix(msg, "stream scanner data:"):
		ch, body, ok := streamChunkOf(msg[len("stream scanner data:"):])
		switch {
		case ok && ch.hasPayload():
			rec.chunks = append(rec.chunks, ch)
		case body != "":
			// Parsed but empty of anything we recognise, or not JSON at all.
			// Folding throws the raw lines away, so an unknown provider shape
			// would vanish without trace - keep a bounded sample instead, which
			// is enough to see what happened and to teach the parser later.
			if len(rec.UnknownChunks) < 20 {
				rec.UnknownChunks = append(rec.UnknownChunks, clip(body, 4000))
			}
			rec.UnknownCount++
			return r[1]
		default:
			return r[1] // blank keepalive or [DONE]
		}
		// Time-to-first-token, measured before the chunks are folded away.
		// Second resolution is all the log line carries.
		if rec.ChunkCount == 0 && rec.TS != "" {
			if t0, e0 := time.ParseInLocation("2006/01/02 15:04:05", rec.TS, time.Local); e0 == nil {
				if t1, e1 := time.ParseInLocation("2006/01/02 15:04:05", ts, time.Local); e1 == nil {
					if d := t1.Sub(t0); d >= 0 {
						rec.FirstChunkMS = int(d / time.Millisecond)
					}
				}
			}
		}
		rec.ChunkCount++
	case strings.HasPrefix(msg, "fullRequestURL:"):
		rec.UpstreamURL = strings.TrimSpace(msg[len("fullRequestURL:"):])
	case strings.HasPrefix(msg, "record consume log:"):
		if i := strings.Index(msg, "params="); i >= 0 {
			rec.Billing = parseRaw(strings.TrimSpace(msg[i+len("params="):]))
		}
	case strings.Contains(msg, "stream ended"):
		if i := strings.LastIndex(msg, "reason="); i >= 0 {
			rec.StreamEnd = strings.TrimSpace(msg[i+len("reason="):])
		} else {
			rec.StreamEnd = strings.TrimSpace(msg)
		}
	case level == "ERR" || strings.Contains(strings.ToLower(msg), "error"):
		rec.Errors = append(rec.Errors, LogErr{TS: ts, Msg: clip(msg, 2000)})
	}
	return r[1]
}

func get(calls map[string]*Record, rid string) *Record {
	if r, ok := calls[rid]; ok {
		return r
	}
	r := blank(rid)
	calls[rid] = r
	return r
}

// streamChunkOf extracts a delta payload from one `stream scanner data:` line.
// The raw body is returned alongside so a shape we do not understand can still
// be preserved rather than silently dropped.
func streamChunkOf(raw string) (streamChunk, string, bool) {
	var ch streamChunk
	raw = strings.TrimSpace(raw)
	// ": x-omniroute-..." comment lines and blank keepalives carry no delta
	if !strings.HasPrefix(raw, "data:") {
		return ch, "", false
	}
	body := strings.TrimSpace(raw[len("data:"):])
	if body == "" || body == "[DONE]" {
		return ch, "", false
	}
	if json.Unmarshal([]byte(body), &ch) != nil {
		return ch, body, false
	}
	return ch, body, true
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---- derivation ------------------------------------------------------------

// finalize derives the summary fields the UI lists on, then drops raw chunks.
func (r *Record) finalize() {
	var req reqView
	if !r.Request.empty() {
		json.Unmarshal(r.Request, &req)
	}
	var resp respView
	if !r.Response.empty() {
		json.Unmarshal(r.Response, &resp)
	}
	var bill billView
	hasBill := !r.Billing.empty()
	if hasBill {
		json.Unmarshal(r.Billing, &bill)
	}

	r.Model = req.Model
	if r.Model == "" {
		r.Model = bill.ModelName
	}
	r.IsStream = req.Stream || len(r.chunks) > 0 || r.sawChunks
	r.HasTools = len(req.Tools) > 0

	// Reassemble streamed output so streaming calls show real content, not a
	// pile of chunks. Reasoning and visible text are tracked separately.
	//
	// finalize can run several times for one record: an incremental read sees
	// a call's chunks across multiple passes. So APPEND newly-seen chunks to
	// what was already assembled rather than rebuilding from r.chunks, which
	// is drained at the end of every pass.
	if r.StreamToolCalls == nil {
		r.StreamToolCalls = []ToolCall{}
	}
	if len(r.chunks) > 0 {
		var content, reasoning strings.Builder
		for _, ch := range r.chunks {
			c, rs, tools := ch.text()
			content.WriteString(c)
			reasoning.WriteString(rs)
			r.StreamToolCalls = append(r.StreamToolCalls, tools...)
			if u := ch.usage(); u != nil {
				r.Usage = u
			}
		}
		r.StreamContent += content.String()
		r.StreamReasoning += reasoning.String()
		r.sawChunks = true
	} else if !r.sawChunks {
		r.Usage = resp.Usage
	}
	if r.Usage == nil && hasBill {
		r.Usage = &Usage{PromptTokens: bill.PromptTokens, CompletionTokens: bill.CompletionTokens}
	}

	r.Quota, r.ChannelID, r.TokenName = bill.Quota, bill.ChannelID, bill.TokenName
	if bill.Other != nil {
		r.ModelRatio, r.CompletionRatio, r.FRT = bill.Other.ModelRatio, bill.Other.CompletionRatio, bill.Other.FRT
	}

	// Epoch seconds for time-range filtering (TS is a display string).
	if t, err := time.ParseInLocation("2006/01/02 15:04:05", r.TS, time.Local); err == nil {
		r.Epoch = t.Unix()
	}

	// Tool names surfaced on the list row, so filtering/scanning does not
	// require opening each call.
	r.ToolNames = []string{}
	for _, t := range req.Tools {
		n := t.Function.Name
		if n == "" {
			n = t.Name
		}
		if n != "" {
			r.ToolNames = append(r.ToolNames, n)
		}
	}
	called := r.StreamToolCalls
	if !r.IsStream && len(resp.Choices) > 0 {
		called = resp.Choices[0].Message.ToolCalls
	}
	r.CalledTools = []string{}
	for _, tc := range called {
		n := tc.Function.Name
		if n != "" && !contains(r.CalledTools, n) {
			r.CalledTools = append(r.CalledTools, n)
		}
	}

	// Conversation size. An agent transcript is a single call carrying dozens
	// of prior turns, so counts belong on the list row.
	r.MsgCount = len(req.Messages)
	r.ToolCount = len(r.ToolNames)
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			r.Turns++
		}
	}

	// Preview text for the collapsed row: last user message.
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			r.Preview = clipRunes(contentText(req.Messages[i].Content), 200)
			break
		}
	}
	r.Incomplete = r.Request.empty() // truncated/rotated-out request line

	// No search index is built here. Search reads bodies on demand from the
	// archive instead: keeping a lowercased copy of every request and response
	// resident doubled the memory cost of every record, to speed up a query
	// this viewer serves a few times a day.

	r.chunks = nil
}

// contentText flattens a message body, which is either a plain string or a
// list of multimodal blocks.
func contentText(c json.RawMessage) string {
	if len(c) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(c, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(c, &parts) == nil {
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			// image_url / audio blocks carry no text; joining them in would
			// pad the row preview with stray separators.
			if p.Text != "" {
				out = append(out, p.Text)
			}
		}
		return strings.Join(out, " ")
	}
	return string(c)
}

func clipRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// Load parses every *.log in dir, newest record first.
func Load(dir string, limitBytes int64) []*Record {
	files, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	sort.Slice(files, func(i, j int) bool {
		return mtime(files[i]).Before(mtime(files[j]))
	})
	calls := ParseFiles(files, limitBytes)
	out := make([]*Record, 0, len(calls))
	for _, r := range calls {
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
	return out
}
