package main

import (
	"encoding/json"
	"testing"
)

// Golden test over testdata/sample.log, which is synthetic: it exercises the
// shapes that matter (non-stream, stream with tools + reasoning, an upstream
// error, a truncated body, and non-call noise) without shipping real traffic.
func TestParseSample(t *testing.T) {
	recs := Load("./testdata", 0)
	byID := map[string]*Record{}
	for _, r := range recs {
		byID[r.RequestID] = r
	}

	t.Run("non-stream call", func(t *testing.T) {
		r := byID["7hKpR3nWqZ4mTxVbY9cLsFgD"]
		if r == nil {
			t.Fatal("record missing")
		}
		want := map[string]any{
			"model":    "demo/chat-mini",
			"stream":   false,
			"status":   200,
			"latency":  "2.01s",
			"preview":  "What is 2+2?",
			"upstream": "https://upstream.example.com/v1/chat/completions",
			"quota":    int64(4),
		}
		if r.Model != want["model"] {
			t.Errorf("model = %q", r.Model)
		}
		if r.IsStream != want["stream"] {
			t.Errorf("is_stream = %v", r.IsStream)
		}
		if r.Status == nil || *r.Status != want["status"] {
			t.Errorf("status = %v", r.Status)
		}
		if r.Latency != want["latency"] {
			t.Errorf("latency = %q", r.Latency)
		}
		if r.Preview != want["preview"] {
			t.Errorf("preview = %q", r.Preview)
		}
		if r.UpstreamURL != want["upstream"] {
			t.Errorf("upstream_url = %q", r.UpstreamURL)
		}
		if r.Quota == nil || *r.Quota != want["quota"] {
			t.Errorf("quota = %v", r.Quota)
		}
		if r.Usage == nil || r.Usage.TotalTokens == nil || *r.Usage.TotalTokens != 31 {
			t.Errorf("usage = %+v", r.Usage)
		}
		if r.Incomplete {
			t.Error("should not be incomplete")
		}
	})

	t.Run("stream with tools reassembles", func(t *testing.T) {
		r := byID["Qw8ErTyUiOpAsDfGhJkLzXcV"]
		if r == nil {
			t.Fatal("record missing")
		}
		if !r.IsStream {
			t.Error("is_stream = false")
		}
		if got := r.StreamContent; got != "Let me look that up." {
			t.Errorf("stream_content = %q", got)
		}
		if got := r.StreamReasoning; got != "The user wants weather first, so call get_weather." {
			t.Errorf("stream_reasoning = %q", got)
		}
		// tools defined vs actually invoked - the distinction the list view shows
		if got := r.ToolNames; len(got) != 2 || got[0] != "get_weather" || got[1] != "send_email" {
			t.Errorf("tool_names = %v", got)
		}
		if got := r.CalledTools; len(got) != 1 || got[0] != "get_weather" {
			t.Errorf("called_tools = %v", got)
		}
		if len(r.StreamToolCalls) != 1 ||
			r.StreamToolCalls[0].Function.Arguments != `{"city":"Berlin"}` {
			t.Errorf("stream_tool_calls = %+v", r.StreamToolCalls)
		}
		// usage rides on a trailing chunk with an empty choices array
		if r.Usage == nil || r.Usage.CompletionDetails == nil ||
			*r.Usage.CompletionDetails.ReasoningTokens != 21 {
			t.Errorf("usage = %+v", r.Usage)
		}
		if r.StreamEnd != "done" {
			t.Errorf("stream_end = %q", r.StreamEnd)
		}
	})

	t.Run("multimodal preview and upstream error", func(t *testing.T) {
		r := byID["ZxCvBnMaSdFgHjKlQwErTyUi"]
		if r == nil {
			t.Fatal("record missing")
		}
		if r.Preview != "Describe this picture." {
			t.Errorf("preview = %q (multimodal blocks should flatten to text)", r.Preview)
		}
		if r.Status == nil || *r.Status != 502 {
			t.Errorf("status = %v", r.Status)
		}
		if len(r.Errors) != 1 {
			t.Errorf("errors = %v", r.Errors)
		}
	})

	t.Run("truncated body marked incomplete", func(t *testing.T) {
		r := byID["MnBvCxZaSdFgHjKlPoIuYtRe"]
		if r == nil {
			t.Fatal("record missing")
		}
		if !r.Incomplete {
			t.Error("truncated request body should set incomplete")
		}
		// billing survives, so the call is still listed rather than vanishing
		if r.Model != "demo/chat-mini" {
			t.Errorf("model should fall back to billing, got %q", r.Model)
		}
	})

	t.Run("health checks excluded from the list", func(t *testing.T) {
		q := archivedQuery(t, recs)
		shown, total, _, _, _ := q.list(listFilter{}, 1, 100)
		for _, r := range shown {
			if r.RequestID == "3fJ7kQmZxR2wLpVnB8sTyHdA" {
				t.Error("/api/status health check should not be listed")
			}
		}
		if total != 7 {
			t.Errorf("shown = %d, want 7", total)
		}
	})

	t.Run("filters", func(t *testing.T) {
		q := archivedQuery(t, recs)
		cases := []struct {
			name string
			f    listFilter
			want int
		}{
			{"tools", listFilter{Tools: "1"}, 4},
			{"tool_name", listFilter{ToolName: "send_email"}, 1},
			{"tool_name missing", listFilter{ToolName: "nope"}, 0},
			{"stream", listFilter{Stream: "1"}, 3},
			{"non-stream", listFilter{Stream: "0"}, 4},
			{"ok", listFilter{Status: "ok"}, 6},
			{"err", listFilter{Status: "err"}, 1},
			{"errors", listFilter{ErrorsOnly: true}, 1},
			{"model", listFilter{Model: "demo/chat-mini"}, 2},
			{"search hit", listFilter{Search: "berlin"}, 1},
			{"search miss", listFilter{Search: "ZZZ"}, 0},
			{"until", listFilter{Until: 1}, 0},
			{"since", listFilter{Since: 1}, 7},
		}
		for _, c := range cases {
			_, got, _, _, _ := q.list(c.f, 1, 100)
			if got != c.want {
				t.Errorf("filter %s = %d, want %d", c.name, got, c.want)
			}
		}
	})

	t.Run("newest first", func(t *testing.T) {
		for i := 1; i < len(recs); i++ {
			if recs[i-1].TS < recs[i].TS {
				t.Fatalf("not sorted descending at %d: %s < %s", i, recs[i-1].TS, recs[i].TS)
			}
		}
	})
}

// Anthropic-native shapes, both of which reached production unparsed.
//
// The prompt one is the nastier of the two: `requestBody:` fell through to the
// error arm, which matches any message containing "error" - and Claude Code's
// system prompt contains the word. So the prompt was not merely missing from
// the request pane, it was being archived as an error message. The fixture's
// system prompt says "Report an error if the build fails" to keep that path
// live: if the marker arm is removed, Errors gains an entry and Request is nil.
func TestAnthropicNative(t *testing.T) {
	recs := Load("./testdata", 0)
	var r *Record
	for _, rec := range recs {
		if rec.RequestID == "AnthropicNativeCallAaaaBbbb" {
			r = rec
		}
	}
	if r == nil {
		t.Fatal("record missing")
	}

	t.Run("requestBody marker parses, and is not an error", func(t *testing.T) {
		if r.Request.empty() {
			t.Fatal("request body not parsed from `requestBody:` marker")
		}
		if r.Incomplete {
			t.Error("incomplete = true, want false")
		}
		if r.Model != "claude-opus-5" {
			t.Errorf("model = %q, want claude-opus-5", r.Model)
		}
		// The whole point: a prompt containing "error" must not be filed as one.
		if len(r.Errors) != 0 {
			t.Errorf("errors = %d (%v), want 0 - prompt misfiled as an error",
				len(r.Errors), r.Errors)
		}
		if r.Preview != "List the repo files." {
			t.Errorf("preview = %q", r.Preview)
		}
		if !r.HasTools {
			t.Error("has_tools = false; Anthropic tools use a bare `name`")
		}
	})

	t.Run("stream content and reasoning are separated", func(t *testing.T) {
		if got, want := r.StreamContent, "I'll list them now."; got != want {
			t.Errorf("stream_content = %q, want %q", got, want)
		}
		if got, want := r.StreamReasoning, "Let me check the directory."; got != want {
			t.Errorf("stream_reasoning = %q, want %q", got, want)
		}
		if !r.IsStream {
			t.Error("is_stream = false")
		}
	})

	t.Run("no chunk is recorded as an unknown shape", func(t *testing.T) {
		// ping/message_stop/content_block_stop/signature_delta are recognised
		// events, not unknown formats - counting them would keep the "unparsed
		// shape" signal permanently lit and hide the next real gap.
		if r.UnknownCount != 0 {
			t.Errorf("unknown_count = %d, want 0; samples: %v",
				r.UnknownCount, r.UnknownChunks)
		}
	})

	t.Run("tool call is assembled across lines by block index", func(t *testing.T) {
		if len(r.StreamToolCalls) != 1 {
			t.Fatalf("stream_tool_calls = %d, want 1: %+v",
				len(r.StreamToolCalls), r.StreamToolCalls)
		}
		tc := r.StreamToolCalls[0]
		if tc.Function.Name != "Bash" {
			t.Errorf("tool name = %q, want Bash", tc.Function.Name)
		}
		// The name arrives in content_block_start, the arguments as separate
		// input_json_delta fragments naming only the index.
		if got, want := tc.Function.Arguments, `{"command": "ls -la"}`; got != want {
			t.Errorf("tool arguments = %q, want %q", got, want)
		}
		if tc.ID != "toolu_01Test" {
			t.Errorf("tool id = %q", tc.ID)
		}
		if !contains(r.CalledTools, "Bash") {
			t.Errorf("called_tools = %v, want to contain Bash", r.CalledTools)
		}
	})

	t.Run("usage merges across message_start and message_delta", func(t *testing.T) {
		if r.Usage == nil {
			t.Fatal("usage nil")
		}
		// message_start: 94 input + 934 cache-create + 519620 cache-read.
		// message_delta repeats input_tokens:94 with no cache counts, so a
		// last-write-wins merge would report 94 and lose a 519k-token prompt.
		if r.Usage.PromptTokens == nil || *r.Usage.PromptTokens != 520648 {
			t.Errorf("prompt_tokens = %v, want 520648", r.Usage.PromptTokens)
		}
		if r.Usage.CompletionTokens == nil || *r.Usage.CompletionTokens != 92 {
			t.Errorf("completion_tokens = %v, want 92", r.Usage.CompletionTokens)
		}
		if r.Usage.PromptDetails == nil || r.Usage.PromptDetails.CachedTokens == nil {
			t.Fatal("cached_tokens missing")
		}
		if got := *r.Usage.PromptDetails.CachedTokens; got != 520554 {
			t.Errorf("cached_tokens = %d, want 520554", got)
		}
		if r.Usage.TotalTokens == nil || *r.Usage.TotalTokens != 520740 {
			t.Errorf("total_tokens = %v, want 520740", r.Usage.TotalTokens)
		}
	})
}

// findRec is a small helper: these tests care about one fixture record each.
func findRec(t *testing.T, rid string) *Record {
	t.Helper()
	for _, rec := range Load("./testdata", 0) {
		if rec.RequestID == rid {
			return rec
		}
	}
	t.Fatalf("record %s missing from fixtures", rid)
	return nil
}

// A streaming tool call arrives as an opener carrying id+name+index followed by
// deltas carrying only index and a slice of the arguments JSON. They have to be
// joined by index; appending each fragment turned one call into one call per
// delta. Production showed a single `write` as 540 nameless calls.
func TestStreamToolCallFragmentsAreJoined(t *testing.T) {
	r := findRec(t, "StreamFragmentToolsAaaaBbbb")

	if n := len(r.StreamToolCalls); n != 2 {
		t.Fatalf("stream_tool_calls = %d, want 2 (one per index, not one per delta)", n)
	}
	for i, want := range []struct{ id, name, args string }{
		{"call_frag1", "write", `{"path":"b.sh"}`},
		{"call_frag2", "read", `{"p":1}`},
	} {
		got := r.StreamToolCalls[i]
		if got.ID != want.id {
			t.Errorf("call %d id = %q, want %q - the opener's id must survive later fragments", i, got.ID, want.id)
		}
		if got.Function.Name != want.name {
			t.Errorf("call %d name = %q, want %q", i, got.Function.Name, want.name)
		}
		// The whole point: fragments concatenate into parseable JSON.
		if got.Function.Arguments != want.args {
			t.Errorf("call %d arguments = %q, want %q", i, got.Function.Arguments, want.args)
		}
		if !json.Valid([]byte(got.Function.Arguments)) {
			t.Errorf("call %d arguments are not valid JSON: %q", i, got.Function.Arguments)
		}
	}
	// And the derived list-row field stays a set of names, not a pile.
	if len(r.CalledTools) != 2 {
		t.Errorf("called_tools = %v, want 2 entries", r.CalledTools)
	}
}

// Turns/MsgCount/ToolCount are derived wholly from the request body, which is
// re-decoded on every finalize pass. finalize runs once per read pass, so
// accumulating instead of assigning multiplied them by the number of passes -
// a 256-message transcript read across 13 passes reported 3,328 turns.
func TestDerivedCountsSurviveRepeatedFinalize(t *testing.T) {
	r := findRec(t, "StreamFragmentToolsAaaaBbbb")

	const (
		wantTurns = 3 // assistant messages in the fixture
		wantMsgs  = 6
		wantTools = 2
	)
	if r.Turns != wantTurns || r.MsgCount != wantMsgs || r.ToolCount != wantTools {
		t.Fatalf("after parse: turns=%d msgs=%d tools=%d, want %d/%d/%d",
			r.Turns, r.MsgCount, r.ToolCount, wantTurns, wantMsgs, wantTools)
	}

	// Ten more passes must not move them. This is the actual regression: one
	// pass looks correct, which is why it shipped.
	for i := 0; i < 10; i++ {
		r.finalize()
	}
	if r.Turns != wantTurns {
		t.Errorf("turns = %d after 11 finalize passes, want %d (accumulated instead of assigned)", r.Turns, wantTurns)
	}
	if r.MsgCount != wantMsgs {
		t.Errorf("msg_count = %d, want %d", r.MsgCount, wantMsgs)
	}
	if r.ToolCount != wantTools {
		t.Errorf("tool_count = %d, want %d", r.ToolCount, wantTools)
	}
	// Tool calls are assembled from cumulative state, so they must also be
	// assigned rather than appended across passes.
	if n := len(r.StreamToolCalls); n != wantTools {
		t.Errorf("stream_tool_calls = %d after repeated finalize, want %d", n, wantTools)
	}
}

// The fragments for one call can land in different read passes: the opener in
// one, the arguments in the next. Joining has to happen in state that outlives
// a pass, since finalize drains r.chunks at the end of each one.
func TestStreamToolCallsJoinAcrossReadPasses(t *testing.T) {
	const rid = "SplitPassToolsAaaaBbbbCcc"
	calls := map[string]*Record{}

	pass := func(lines ...string) {
		for _, l := range lines {
			handleLine(l, calls)
		}
		calls[rid].finalize()
	}

	pass(`[DEBUG] 2026/01/15 - 14:00:00 | `+rid+` | text request body: {"model":"chat-pro","stream":true,"messages":[{"role":"user","content":"go"},{"role":"assistant","content":"ok"}]}`,
		`[DEBUG] 2026/01/15 - 14:00:01 | `+rid+` | stream scanner data: data: {"choices":[{"delta":{"tool_calls":[{"id":"call_x","type":"function","function":{"name":"write","arguments":"{\"a\""},"index":0}]}}]}`)
	pass(`[DEBUG] 2026/01/15 - 14:00:02 | ` + rid + ` | stream scanner data: data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":":1}"},"index":0}]}}]}`)

	r := calls[rid]
	if n := len(r.StreamToolCalls); n != 1 {
		t.Fatalf("stream_tool_calls = %d, want 1 - fragments split across passes must still join", n)
	}
	tc := r.StreamToolCalls[0]
	if tc.Function.Name != "write" || tc.Function.Arguments != `{"a":1}` {
		t.Errorf("call = %q %q, want write {\"a\":1}", tc.Function.Name, tc.Function.Arguments)
	}
	if r.Turns != 1 {
		t.Errorf("turns = %d after 2 passes, want 1", r.Turns)
	}
}

// Some providers omit the index and stream one call at a time. Those fragments
// must still concatenate rather than each becoming its own call - and a new
// tool name is the only signal that the next call has begun.
func TestStreamToolCallsWithoutIndex(t *testing.T) {
	var ta toolAcc
	frag := func(name, args string) ToolCall {
		var tc ToolCall
		tc.Function.Name, tc.Function.Arguments = name, args
		return tc
	}
	ta.add(frag("write", `{"p"`))
	ta.add(frag("", `:1}`))
	ta.add(frag("read", `{"q":2}`))

	got := ta.calls()
	if len(got) != 2 {
		t.Fatalf("calls = %d, want 2", len(got))
	}
	if got[0].Function.Name != "write" || got[0].Function.Arguments != `{"p":1}` {
		t.Errorf("call 0 = %q %q", got[0].Function.Name, got[0].Function.Arguments)
	}
	if got[1].Function.Name != "read" || got[1].Function.Arguments != `{"q":2}` {
		t.Errorf("call 1 = %q %q", got[1].Function.Name, got[1].Function.Arguments)
	}
}
