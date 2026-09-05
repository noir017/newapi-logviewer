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

	// V is the archive record version. Absent (0) means v1: the request is
	// inline. v2 lifts the request's repeated bulk into the day's blob pool and
	// leaves pointers in its place; fetch reinlines them. See blob.go.
	V int `json:"v,omitempty"`

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

	// Outcome is whether the call succeeded, derived in finalize from whatever
	// evidence the log actually carried. Status alone could not answer this:
	// it comes only from the GIN line, which reaches a different sink than the
	// log file and was absent from every record on this deployment, so the UI
	// showed "?" for all of them.
	//
	// Kept separate from Status rather than synthesising an HTTP code, for the
	// same reason Stalled is separate from Incomplete: "the gateway billed this
	// as a completed stream" and "the gateway returned 200" are different
	// facts, and inventing a 200 would erase the distinction permanently.
	//
	// One of outcomeOK, outcomeErr, or "" when nothing in the log settles it.
	Outcome string `json:"outcome,omitempty"`

	// OutcomeReason is the short machine token behind Outcome - the billing
	// line's end_reason (done/eof/client_gone/scanner_error), or a marker for
	// the evidence used when billing was absent. Displayed on the detail pane
	// so a failure says why rather than only that.
	OutcomeReason string `json:"outcome_reason,omitempty"`

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

	// ChannelName is resolved at serve time from New API's admin API, not from
	// the log, which never carries it. It is deliberately never persisted:
	// omitempty keeps it out of the archive, so a renamed channel shows its
	// current name on old records rather than a stale one.
	ChannelName string `json:"channel_name,omitempty"`

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

	// billingSeen records that the billing line arrived. It is a completion
	// marker independent of the GIN line, which does not reach the log file on
	// every deployment - see the comment at the `record consume log:` arm.
	billingSeen bool

	// lastSeen is when a line for this call was last read, used by the
	// ingester to decide a call has stalled and will never complete.
	lastSeen time.Time

	// anth accumulates Anthropic tool calls across chunk lines and across
	// incremental read passes, since a tool's name and its arguments arrive on
	// different lines that may land in different passes.
	anth anthropicState

	// tacc does the same for OpenAI-shaped tool_call deltas. Both formats
	// stream a call as a name in one line and arguments in many, so both need
	// state that outlives a single chunk and a single read pass.
	tacc toolAcc

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

		// How the stream actually ended, as New API judged it at billing time.
		// This is the only success/failure signal that reliably reaches the log
		// FILE: the HTTP status lives on the GIN line, and gin writes that to
		// gin.DefaultWriter - a different sink. Measured on this deployment,
		// 0 GIN lines against 291 billing lines, which left Status nil on every
		// record and rendered the whole list as "?".
		//
		// status is "ok" or "error"; end_reason is done/eof on success and
		// client_gone/scanner_error on failure, with end_error carrying the
		// detail.
		StreamStatus *struct {
			Status    string `json:"status"`
			EndReason string `json:"end_reason"`
			EndError  string `json:"end_error"`
		} `json:"stream_status"`
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

	// Anthropic's native streaming shape, logged verbatim for Claude-family
	// channels on the /v1/messages path. Same lesson as Gemini above, learned
	// the same way: claude-opus-5 calls archived 69 unparsed chunks with an
	// empty body, because these decode as valid JSON with no `choices` at all -
	// so unlike a malformed line they were not even obviously broken.
	//
	// One event per line, discriminated by Type:
	//   message_start        - usage (input/cache tokens)
	//   content_block_start  - opens block Index; carries the tool name+id
	//   content_block_delta  - text_delta | thinking_delta | input_json_delta
	//   message_delta        - final usage (output_tokens)
	// Blocks are addressed by index, and a tool call's name arrives in its
	// _start while its arguments dribble in as input_json_delta fragments that
	// name only the index - so assembly needs state across lines, which is what
	// anthropicState below carries.
	Type  string `json:"type"`
	Index *int   `json:"index"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Message *struct {
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`

	// raw is the undecoded chunk body, not a wire field. Anthropic's final
	// usage sits at the top-level "usage" key - the same key OpenAI uses, but
	// with different member names - and two struct fields cannot share one json
	// tag: encoding/json silently drops BOTH on conflict. So message_delta
	// usage is decoded from raw on demand rather than bound to a second field.
	raw string
}

// anthropicUsage is Anthropic's token accounting. Cache reads and writes are
// billed differently from fresh input, and for a cached agent transcript they
// dwarf it - 519,620 cache-read against 94 input tokens on a real call here -
// so prompt_tokens must include them or the number is meaningless.
type anthropicUsage struct {
	InputTokens         *int `json:"input_tokens"`
	OutputTokens        *int `json:"output_tokens"`
	CacheCreationTokens *int `json:"cache_creation_input_tokens"`
	CacheReadTokens     *int `json:"cache_read_input_tokens"`
}

// toUsage normalises Anthropic's counts onto the OpenAI-shaped Usage the UI
// renders. Cached tokens are surfaced separately as well as folded into the
// prompt total, matching what prompt_tokens_details.cached_tokens means.
func (a *anthropicUsage) toUsage() *Usage {
	if a == nil {
		return nil
	}
	var u Usage
	in := 0
	if a.InputTokens != nil {
		in = *a.InputTokens
	}
	cached := 0
	if a.CacheReadTokens != nil {
		cached += *a.CacheReadTokens
	}
	if a.CacheCreationTokens != nil {
		cached += *a.CacheCreationTokens
	}
	total := in + cached
	u.PromptTokens = &total
	if cached > 0 {
		u.PromptDetails = &struct {
			CachedTokens *int `json:"cached_tokens,omitempty"`
		}{CachedTokens: &cached}
	}
	if a.OutputTokens != nil {
		out := *a.OutputTokens
		u.CompletionTokens = &out
		sum := total + out
		u.TotalTokens = &sum
	}
	return &u
}

// anthropicState carries the cross-line state Anthropic's format needs: which
// block index is which tool, so input_json_delta fragments (which name only an
// index) can be attributed. Held per-record by the parser.
type anthropicState struct {
	tools map[int]*ToolCall // block index -> accumulating tool call
	order []int             // first-seen order, so output is deterministic
}

// isAnthropic reports whether this chunk is an Anthropic-native event.
func (ch *streamChunk) isAnthropic() bool {
	switch ch.Type {
	case "message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop":
		return true
	}
	return false
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
	// Anthropic: text and thinking deltas. Tool calls are NOT returned here -
	// their arguments span many lines and are assembled by anthropicState, so
	// emitting them per-chunk would produce one fragment-sized call per line.
	if ch.Type == "content_block_delta" && ch.Delta != nil {
		switch ch.Delta.Type {
		case "text_delta":
			c.WriteString(ch.Delta.Text)
		case "thinking_delta":
			r.WriteString(ch.Delta.Thinking)
		}
	}
	return c.String(), r.String(), tools
}

// usage normalises whichever usage block the chunk carried.
func (ch *streamChunk) usage() *Usage {
	// Anthropic reports usage twice: input and cache counts in message_start,
	// output_tokens in the closing message_delta. Both are decoded from raw
	// (see the field comment) and merged by the caller.
	//
	// This must be tested BEFORE ch.Usage. Anthropic's message_delta puts its
	// usage at the top-level "usage" key - the same key OpenAI uses - so the
	// OpenAI-shaped ch.Usage decodes to a non-nil struct with every member nil.
	// Checking ch.Usage first therefore returned an empty usage and threw away
	// output_tokens entirely.
	if ch.Type == "message_start" && ch.Message != nil {
		return ch.Message.Usage.toUsage()
	}
	if ch.Type == "message_delta" && ch.raw != "" {
		var v struct {
			Usage *anthropicUsage `json:"usage"`
		}
		if json.Unmarshal([]byte(ch.raw), &v) == nil {
			return v.Usage.toUsage()
		}
	}
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
//
// The Anthropic arm is why this is not just a nil-check on Choices: a
// content_block_delta is valid JSON with no choices, so it read as "unknown
// shape" and its text went to UnknownChunks - which meant a bounded 20-line
// sample survived and the rest of the response was destroyed by folding.
func (ch *streamChunk) hasPayload() bool {
	if len(ch.Choices) > 0 || len(ch.Candidates) > 0 || ch.Usage != nil || ch.UsageMetadata != nil {
		return true
	}
	switch ch.Type {
	case "message_start", "content_block_start", "content_block_delta", "message_delta":
		return true
	}
	// "ping", "message_stop" and content_block_stop carry no data. They are
	// real events rather than unknown shapes though, so report them as
	// payload-free instead of letting them accumulate as evidence of a format
	// we failed to parse.
	return false
}

// isKnownEmpty distinguishes a recognised but empty Anthropic/SSE event from a
// genuinely unrecognised chunk shape. Only the latter is worth sampling.
func (ch *streamChunk) isKnownEmpty() bool {
	switch ch.Type {
	case "ping", "message_stop", "content_block_stop", "error":
		return true
	}
	return false
}

// applyAnthropic folds a chunk's tool-call fragments into st. Anthropic opens a
// tool block with its name and id, then streams the arguments as partial_json
// fragments that identify only the block index, so the two have to be joined
// here rather than per-chunk.
func (st *anthropicState) applyAnthropic(ch *streamChunk) {
	if ch.Index == nil {
		return
	}
	idx := *ch.Index
	switch ch.Type {
	case "content_block_start":
		if ch.ContentBlock == nil || ch.ContentBlock.Type != "tool_use" {
			return
		}
		if st.tools == nil {
			st.tools = map[int]*ToolCall{}
		}
		if _, seen := st.tools[idx]; !seen {
			st.order = append(st.order, idx)
		}
		i := idx
		tc := &ToolCall{Index: &i, ID: ch.ContentBlock.ID, Type: "function"}
		tc.Function.Name = ch.ContentBlock.Name
		st.tools[idx] = tc
	case "content_block_delta":
		if ch.Delta == nil || ch.Delta.Type != "input_json_delta" {
			return
		}
		// Arguments for a block we never saw opened - a call whose start line
		// rotated out of the spool. Keep the fragments under a nameless call
		// rather than discarding them.
		tc, ok := st.tools[idx]
		if !ok {
			if st.tools == nil {
				st.tools = map[int]*ToolCall{}
			}
			i := idx
			tc = &ToolCall{Index: &i, Type: "function"}
			st.tools[idx] = tc
			st.order = append(st.order, idx)
		}
		tc.Function.Arguments += ch.Delta.PartialJSON
	}
}

// calls returns the assembled tool calls in the order their blocks opened.
func (st *anthropicState) calls() []ToolCall {
	out := make([]ToolCall, 0, len(st.order))
	for _, idx := range st.order {
		if tc := st.tools[idx]; tc != nil {
			out = append(out, *tc)
		}
	}
	return out
}

// toolAcc folds OpenAI-shaped streaming tool_call deltas into whole calls.
//
// OpenAI streams a tool call the same way it streams text: the first delta
// carries id, name and index, and every following delta carries the index plus
// one more slice of the arguments JSON. So the fragments have to be joined by
// index, exactly like Anthropic's input_json_delta - the difference is only
// which field names the block.
//
// Without this the fragments were appended to StreamToolCalls verbatim, so one
// tool call became one "call" per delta: a single `write` with a file body in
// its arguments rendered as 540 nameless calls holding a character or two each.
type toolAcc struct {
	byIdx map[int]*ToolCall
	order []int // first-seen order, so output is deterministic
	last  int   // index of the call being streamed, for fragments without one
	seen  bool
}

// add merges one tool_call delta into the accumulating set.
func (ta *toolAcc) add(frag ToolCall) {
	idx := 0
	switch {
	case frag.Index != nil:
		idx = *frag.Index
	case ta.seen:
		// No index. Providers that omit it stream one call at a time, so keep
		// appending to the current one - unless this fragment names a
		// different tool, which can only mean the next call has started.
		idx = ta.last
		if n := frag.Function.Name; n != "" {
			if cur := ta.byIdx[idx]; cur != nil && cur.Function.Name != "" && cur.Function.Name != n {
				idx = ta.last + 1
			}
		}
	}
	ta.last, ta.seen = idx, true

	if ta.byIdx == nil {
		ta.byIdx = map[int]*ToolCall{}
	}
	tc, ok := ta.byIdx[idx]
	if !ok {
		i := idx
		tc = &ToolCall{Index: &i}
		ta.byIdx[idx] = tc
		ta.order = append(ta.order, idx)
	}
	// Identity fields arrive once, on the opening delta; later fragments leave
	// them empty and must not blank out what the opener established.
	if frag.ID != "" {
		tc.ID = frag.ID
	}
	if frag.Type != "" {
		tc.Type = frag.Type
	}
	if frag.Function.Name != "" {
		tc.Function.Name = frag.Function.Name
	}
	tc.Function.Arguments += frag.Function.Arguments
}

// calls returns the assembled tool calls in first-seen index order.
func (ta *toolAcc) calls() []ToolCall {
	out := make([]ToolCall, 0, len(ta.order))
	for _, idx := range ta.order {
		if tc := ta.byIdx[idx]; tc != nil {
			out = append(out, *tc)
		}
	}
	return out
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
	// Anthropic-native relays log the request under a different marker. Missing
	// it did more than blank the request pane: the body fell through to the
	// error arm below, which matches any message containing "error" - and the
	// Claude Code system prompt contains the word. Every claude-opus-5 prompt
	// was being filed as an error. Both markers must be handled here, ahead of
	// that catch-all.
	case strings.HasPrefix(msg, "requestBody:"):
		rec.Request = parseRaw(strings.TrimSpace(msg[len("requestBody:"):]))
	case strings.HasPrefix(msg, "upstream response body:"):
		rec.Response = parseRaw(strings.TrimSpace(msg[len("upstream response body:"):]))
	case strings.HasPrefix(msg, "stream scanner data:"):
		ch, body, ok := streamChunkOf(msg[len("stream scanner data:"):])
		switch {
		case ok && ch.hasPayload():
			// Anthropic tool arguments span many lines and are joined by index,
			// so they are folded into per-record state as they arrive rather
			// than carried on the chunk.
			if ch.isAnthropic() {
				rec.anth.applyAnthropic(&ch)
			}
			rec.chunks = append(rec.chunks, ch)
		case ok && ch.isKnownEmpty():
			return r[1] // ping / message_stop / content_block_stop
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
			// Billing is written after the response is fully delivered, which
			// makes it a completion marker in its own right - and the only one
			// that reliably reaches the log FILE.
			//
			// The GIN line cannot be relied on: gin's access logger writes to
			// gin.DefaultWriter, which is a different sink from the file New API
			// opens for its own logger. Measured on this deployment - 347,863
			// DEBUG lines in the live log file and not one GIN line, while
			// `docker logs` showed them arriving on stdout the whole time.
			// Archived history confirms it changed under us: Aug 10 had 2,052
			// records carrying a GIN-only field, Aug 11 onward had none.
			//
			// Without this, every call waits out stallAfter and is filed as
			// Stalled - a 10-minute archive delay on every record, and a
			// "response never finished" badge on calls that finished fine.
			rec.billingSeen = true
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
	ch.raw = body
	return ch, body, true
}

// mergeUsage combines usage seen across chunks, preferring later non-nil
// values. Needed because Anthropic splits the accounting in two: message_start
// carries input and cache tokens, and the closing message_delta carries
// output_tokens with input_tokens repeated but cache counts absent. Replacing
// wholesale - which is what the OpenAI path did, since it reports usage once -
// would drop the cache read of a 519k-token agent transcript.
func mergeUsage(old, next *Usage) *Usage {
	if old == nil {
		return next
	}
	if next == nil {
		return old
	}
	out := *old
	if next.PromptTokens != nil {
		// Keep the larger prompt count: message_delta repeats input_tokens
		// without the cache totals, so taking it verbatim would shrink a
		// 519,714-token prompt to 94.
		if out.PromptTokens == nil || *next.PromptTokens > *out.PromptTokens {
			out.PromptTokens = next.PromptTokens
		}
	}
	if next.CompletionTokens != nil {
		out.CompletionTokens = next.CompletionTokens
	}
	if next.PromptDetails != nil {
		out.PromptDetails = next.PromptDetails
	}
	if next.CompletionDetails != nil {
		out.CompletionDetails = next.CompletionDetails
	}
	// Recompute rather than trust either event's total: neither Anthropic event
	// carries a total, and toUsage's is derived from whatever that event knew.
	if out.PromptTokens != nil && out.CompletionTokens != nil {
		sum := *out.PromptTokens + *out.CompletionTokens
		out.TotalTokens = &sum
	} else if next.TotalTokens != nil {
		out.TotalTokens = next.TotalTokens
	}
	return &out
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
			for _, frag := range tools {
				r.tacc.add(frag)
			}
			if u := ch.usage(); u != nil {
				r.Usage = mergeUsage(r.Usage, u)
			}
		}
		r.StreamContent += content.String()
		r.StreamReasoning += reasoning.String()
		r.sawChunks = true
	} else if !r.sawChunks {
		r.Usage = resp.Usage
	}
	// Both accumulators hold cumulative state keyed by call index, so their
	// output is ASSIGNED rather than appended: finalize runs once per read pass,
	// and appending would re-add every call already assembled.
	if calls := r.tacc.calls(); len(calls) > 0 {
		r.StreamToolCalls = calls
	}
	if calls := r.anth.calls(); len(calls) > 0 {
		r.StreamToolCalls = calls
	}
	if r.Usage == nil && hasBill {
		r.Usage = &Usage{PromptTokens: bill.PromptTokens, CompletionTokens: bill.CompletionTokens}
	}

	r.Quota, r.ChannelID, r.TokenName = bill.Quota, bill.ChannelID, bill.TokenName
	if bill.Other != nil {
		r.ModelRatio, r.CompletionRatio, r.FRT = bill.Other.ModelRatio, bill.Other.CompletionRatio, bill.Other.FRT
	}
	r.deriveOutcome(&bill)

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
	// Assign, never accumulate. finalize runs once per read pass - a streaming
	// call is finalized on every pass that brings it new chunks - and these are
	// all derived wholly from req, which is re-decoded each time. `r.Turns++`
	// therefore multiplied the true count by the number of passes: a 256-message
	// transcript read across 13 passes reported 3,328 turns.
	r.MsgCount = len(req.Messages)
	r.ToolCount = len(r.ToolNames)
	turns := 0
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			turns++
		}
	}
	r.Turns = turns

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

// Outcome values. Deliberately not HTTP codes: the log does not always carry
// one, and synthesising a 200 would be indistinguishable from having observed
// one.
const (
	outcomeOK  = "ok"
	outcomeErr = "error"
)

// deriveOutcome decides whether the call succeeded, from the best evidence the
// log actually carried.
//
// The evidence is ranked, strongest first, because the sources disagree in
// predictable ways:
//
//  1. The GIN status line, when present - an observed HTTP response code.
//  2. The billing line's stream_status - written by New API after delivery, and
//     the only outcome signal that reaches the log FILE on every deployment.
//  3. A logged [ERR] line, or a stall with no completion marker at all.
//
// Billing is consulted before r.Errors because a call can log a recoverable
// error - a retried upstream, a channel failover - and still be delivered and
// charged as a success. Trusting the error list first would file those as
// failures. Conversely a call with no billing and no GIN line never completed,
// whatever else it logged.
func (r *Record) deriveOutcome(bill *billView) {
	// Assign, never accumulate: finalize runs once per read pass, and every
	// input here is re-derived from the record each time.
	r.Outcome, r.OutcomeReason = "", ""

	if r.Status != nil {
		if *r.Status < 400 {
			r.Outcome = outcomeOK
		} else {
			r.Outcome = outcomeErr
		}
		r.OutcomeReason = "http_" + strconv.Itoa(*r.Status)
		return
	}

	if bill.Other != nil && bill.Other.StreamStatus != nil {
		ss := bill.Other.StreamStatus
		switch ss.Status {
		case "ok":
			r.Outcome = outcomeOK
		case "error":
			r.Outcome = outcomeErr
		}
		if r.Outcome != "" {
			r.OutcomeReason = ss.EndReason
			if ss.EndError != "" {
				r.OutcomeReason = strings.TrimSpace(ss.EndReason + ": " + ss.EndError)
			}
			return
		}
	}

	// A billing line with no stream_status still means the response was
	// delivered and charged - non-streaming calls carry no stream status at all.
	if r.billingSeen || !r.Billing.empty() {
		r.Outcome, r.OutcomeReason = outcomeOK, "billed"
		return
	}

	// No completion marker of any kind. Stalled is set by the ingester when the
	// call went quiet past its deadline; either way nothing says it finished.
	if r.Stalled {
		r.Outcome, r.OutcomeReason = outcomeErr, "stalled"
		return
	}
	if len(r.Errors) > 0 {
		r.Outcome, r.OutcomeReason = outcomeErr, "logged_error"
		return
	}
	// Leave empty: the UI shows "?" only when the log genuinely does not say.
}

// refreshOutcome re-derives Outcome for a record whose bodies are already
// assembled, decoding the billing payload rather than taking it from a parse
// pass.
//
// Two callers need this and neither can use finalize. The ingester marks a call
// Stalled after finalize has already run, and reindex fetches records straight
// out of the archive: a full finalize there would rebuild derived fields from
// bodies alone, and since sawChunks is not persisted it would overwrite the
// stored Usage of every streaming record with the non-streaming response's
// (nil). This touches only the two outcome fields.
func (r *Record) refreshOutcome() {
	var bill billView
	if !r.Billing.empty() {
		json.Unmarshal(r.Billing, &bill)
	}
	r.deriveOutcome(&bill)
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
