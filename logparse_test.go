package main

import (
	"bytes"
	"encoding/json"
	"strings"
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

// OpenAI Responses API streams, relayed verbatim from an upstream that speaks
// /v1/responses natively. Shapes are copied from a production modelscope call
// that archived 1,056 completion tokens as an empty body: the text deltas'
// string-valued `delta` failed to decode, so every event was an unknown shape.
// Split across two read passes, as a live stream is.
func TestResponsesAPIStream(t *testing.T) {
	const rid = "ResponsesApiStreamAaaaBbbb"
	calls := map[string]*Record{}
	line := func(ts, msg string) string {
		return `[DEBUG] 2026/10/03 - 17:55:` + ts + ` | ` + rid + ` | ` + msg
	}
	data := func(ts, body string) string { return line(ts, "stream scanner data: data: "+body+" ") }
	pass := func(lines ...string) {
		for _, l := range lines {
			handleLine(l, calls)
		}
		calls[rid].finalize()
	}

	pass(
		line("01", `requestBody: {"model":"deepseek-v4-flash","input":[{"role":"user","content":"summarise"}],"instructions":"Report an error if unsure.","stream":true,"tools":[{"type":"function","name":"lookup","parameters":{}}]}`),
		data("02", `{"response":{"id":"resp_1","object":"response","output":[],"status":"queued","usage":null},"sequence_number":0,"type":"response.created"}`),
		data("02", `{"response":{"id":"resp_1","object":"response","output":[],"status":"in_progress","usage":null},"sequence_number":1,"type":"response.in_progress"}`),
		data("02", `{"item":{"id":"msg_r","summary":[],"type":"reasoning"},"output_index":0,"sequence_number":2,"type":"response.output_item.added"}`),
		data("03", `{"content_index":0,"delta":"好的，","item_id":"msg_r","output_index":0,"sequence_number":3,"type":"response.reasoning_text.delta"}`),
		data("03", `{"content_index":0,"delta":"读一遍。","item_id":"msg_r","output_index":0,"sequence_number":4,"type":"response.reasoning_text.delta"}`),
		line("03", "stream scanner data: "),
		data("03", `{"content_index":0,"item_id":"msg_r","output_index":0,"sequence_number":5,"text":"好的，读一遍。","type":"response.reasoning_text.done"}`),
		data("03", `{"item":{"id":"msg_r","summary":[{"text":"好的，读一遍。","type":"summary_text"}],"type":"reasoning"},"output_index":0,"sequence_number":6,"type":"response.output_item.done"}`),
		data("04", `{"item":{"content":[],"id":"msg_a","role":"assistant","status":"in_progress","type":"message"},"output_index":1,"sequence_number":7,"type":"response.output_item.added"}`),
		data("04", `{"content_index":0,"item_id":"msg_a","output_index":1,"part":{"annotations":[],"text":"","type":"output_text"},"sequence_number":8,"type":"response.content_part.added"}`),
		data("04", `{"content_index":0,"delta":"{\n","item_id":"msg_a","logprobs":[],"output_index":1,"sequence_number":9,"type":"response.output_text.delta"}`),
	)
	pass(
		data("05", `{"content_index":0,"delta":"  \"梗概\": \"…\"\n}","item_id":"msg_a","logprobs":[],"output_index":1,"sequence_number":10,"type":"response.output_text.delta"}`),
		data("05", `{"content_index":0,"item_id":"msg_a","logprobs":[],"output_index":1,"sequence_number":11,"text":"{\n  \"梗概\": \"…\"\n}","type":"response.output_text.done"}`),
		data("05", `{"content_index":0,"item_id":"msg_a","output_index":1,"part":{"annotations":[],"text":"{\n  \"梗概\": \"…\"\n}","type":"output_text"},"sequence_number":12,"type":"response.content_part.done"}`),
		data("05", `{"item":{"content":[{"annotations":[],"text":"{\n  \"梗概\": \"…\"\n}","type":"output_text"}],"id":"msg_a","role":"assistant","status":"completed","type":"message"},"output_index":1,"sequence_number":13,"type":"response.output_item.done"}`),
		data("06", `{"item":{"arguments":"","call_id":"call_9","id":"fc_9","name":"lookup","status":"in_progress","type":"function_call"},"output_index":2,"sequence_number":14,"type":"response.output_item.added"}`),
		data("06", `{"delta":"{\"q\":","item_id":"fc_9","output_index":2,"sequence_number":15,"type":"response.function_call_arguments.delta"}`),
		data("06", `{"delta":"1}","item_id":"fc_9","output_index":2,"sequence_number":16,"type":"response.function_call_arguments.delta"}`),
		data("06", `{"arguments":"{\"q\":1}","item_id":"fc_9","output_index":2,"sequence_number":17,"type":"response.function_call_arguments.done"}`),
		data("06", `{"response":{"id":"resp_1","object":"response","output":[],"status":"completed","usage":{"input_tokens":2940,"input_tokens_details":{"cached_tokens":128},"output_tokens":1056,"output_tokens_details":{"reasoning_tokens":501},"total_tokens":3996}},"sequence_number":18,"type":"response.completed"}`),
		line("06", "stream scanner data: data: [DONE]"),
		`[INFO] 2026/10/03 - 17:55:06 | `+rid+` | stream ended: reason=eof `,
		`[INFO] 2026/10/03 - 17:55:06 | `+rid+` | record consume log: userId=1, params={"channel_id":46,"prompt_tokens":2940,"completion_tokens":1056,"model_name":"deepseek-v4-flash","quota":628,"other":{"stream_status":{"end_reason":"eof","status":"ok"}}}`,
	)
	r := calls[rid]

	t.Run("text and reasoning are reassembled, once each", func(t *testing.T) {
		// The *.done events restate the whole text; counting them as well
		// would print every answer twice.
		if got, want := r.StreamContent, "{\n  \"梗概\": \"…\"\n}"; got != want {
			t.Errorf("stream_content = %q, want %q", got, want)
		}
		if got, want := r.StreamReasoning, "好的，读一遍。"; got != want {
			t.Errorf("stream_reasoning = %q, want %q", got, want)
		}
	})

	t.Run("no event is recorded as an unknown shape", func(t *testing.T) {
		if r.UnknownCount != 0 {
			t.Errorf("unknown_count = %d, want 0; samples: %v", r.UnknownCount, r.UnknownChunks)
		}
		if r.ChunkCount == 0 || !r.IsStream {
			t.Errorf("chunk_count = %d, is_stream = %v", r.ChunkCount, r.IsStream)
		}
		if len(r.Errors) != 0 {
			t.Errorf("errors = %v, want none", r.Errors)
		}
	})

	t.Run("function call is joined by output_index", func(t *testing.T) {
		if len(r.StreamToolCalls) != 1 {
			t.Fatalf("stream_tool_calls = %+v, want 1", r.StreamToolCalls)
		}
		tc := r.StreamToolCalls[0]
		if tc.ID != "call_9" || tc.Function.Name != "lookup" || tc.Function.Arguments != `{"q":1}` {
			t.Errorf("call = %q %q %q, want call_9 lookup {\"q\":1}", tc.ID, tc.Function.Name, tc.Function.Arguments)
		}
		if !contains(r.CalledTools, "lookup") {
			t.Errorf("called_tools = %v", r.CalledTools)
		}
	})

	t.Run("usage comes from response.completed, details included", func(t *testing.T) {
		u := r.Usage
		if u == nil || u.PromptTokens == nil || u.CompletionTokens == nil || u.TotalTokens == nil {
			t.Fatalf("usage = %+v", u)
		}
		// input_tokens already counts the cache here, unlike Anthropic's.
		if *u.PromptTokens != 2940 || *u.CompletionTokens != 1056 || *u.TotalTokens != 3996 {
			t.Errorf("usage = %d/%d/%d, want 2940/1056/3996", *u.PromptTokens, *u.CompletionTokens, *u.TotalTokens)
		}
		if u.PromptDetails == nil || u.PromptDetails.CachedTokens == nil || *u.PromptDetails.CachedTokens != 128 {
			t.Errorf("cached_tokens = %+v, want 128", u.PromptDetails)
		}
		if u.CompletionDetails == nil || u.CompletionDetails.ReasoningTokens == nil || *u.CompletionDetails.ReasoningTokens != 501 {
			t.Errorf("reasoning_tokens = %+v, want 501", u.CompletionDetails)
		}
	})
}

// The Responses API and Anthropic share the top-level "delta" key with
// different types. Both must keep decoding through the one field.
func TestChunkDeltaAcceptsStringAndObject(t *testing.T) {
	_, _, ok := streamChunkOf(`data: {"type":"response.output_text.delta","delta":"hi"}`)
	if !ok {
		t.Fatal("string delta failed to decode")
	}
	ch, _, ok := streamChunkOf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"yo"}}`)
	if !ok || ch.Delta == nil || ch.Delta.Text != "yo" {
		t.Fatalf("object delta: ok=%v delta=%+v", ok, ch.Delta)
	}
	if c, _, _ := ch.text(); c != "yo" {
		t.Errorf("anthropic text = %q, want yo", c)
	}
}

// Gemini's REST API pretty-prints every non-streaming response, and new-api
// logs it as received, so the body runs on over ~30 unprefixed lines. Read line
// by line, the record kept `Gemini response body: {` and nothing else - and the
// marker was not handled either - so no non-streaming Gemini call ever had its
// output archived. Layout copied from a production line (hindsight's
// gemini-3-flash-lite fact extraction); the content is synthetic.
//
// The answer contains the word "error" on purpose: a body that reaches the
// error catch-all instead of its own arm shows up as an entry in Errors.
const geminiPrettyLog = `[DEBUG] 2026/10/07 - 01:33:57 | GeminiPrettyBodyAaaaBbbbCc | text request body: {"contents":[{"role":"user","parts":[{"text":"Return valid json only."}]}],"generationConfig":{"temperature":0.1,"responseMimeType":"application/json"}}
[DEBUG] 2026/10/07 - 01:33:57 | GeminiPrettyBodyAaaaBbbbCc | fullRequestURL: https://generativelanguage.googleapis.com/v1beta/models/gemini-3.5-flash-lite:generateContent
[DEBUG] 2026/10/07 - 01:34:15 | GeminiPrettyBodyAaaaBbbbCc | Gemini response body: {
  "candidates": [
    {
      "content": {
        "parts": [
          {
            "text": "{\n  \"facts\": [\"the CLI reports an error when .env is not sourced\"]\n}",
            "thoughtSignature": "EmAKXgFpFH0TxS5t"
          },
          {
            "functionCall": {
              "name": "save_facts",
              "args": {
                "n": 1
              }
            }
          }
        ],
        "role": "model"
      },
      "finishReason": "STOP",
      "index": 0
    }
  ],
  "usageMetadata": {
    "promptTokenCount": 4753,
    "candidatesTokenCount": 373,
    "totalTokenCount": 5126,
    "promptTokensDetails": [
      {
        "modality": "TEXT",
        "tokenCount": 4753
      }
    ]
  },
  "modelVersion": "gemini-3.5-flash-lite",
  "responseId": "a1b2c3"
} 
[GIN-debug] redirecting request 301: /api/log/ --> /api/log/?p=1
[INFO] 2026/10/07 - 01:34:15 | GeminiPrettyBodyAaaaBbbbCc | record consume log: userId=1, params={"channel_id":4,"prompt_tokens":4753,"completion_tokens":373,"model_name":"gemini-3-flash-lite","token_name":"hindsight","quota":874}
[GIN] 2026/10/07 - 01:34:15 | relay | GeminiPrettyBodyAaaaBbbbCc | 200 | 18.567020844s |      172.31.0.5 |    POST /v1/chat/completions
`

func TestGeminiPrettyPrintedResponse(t *testing.T) {
	calls := map[string]*Record{}
	scanLines(strings.NewReader(geminiPrettyLog), calls)
	r := calls["GeminiPrettyBodyAaaaBbbbCc"]
	if r == nil {
		t.Fatal("record missing")
	}
	r.finalize()

	t.Run("the whole body is kept, compacted", func(t *testing.T) {
		if r.Response.empty() {
			t.Fatal("response is empty - only the marker line was read")
		}
		if bytes.IndexByte(r.Response, '\n') >= 0 {
			t.Errorf("response kept its indentation: %q", r.Response)
		}
		var v struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal(r.Response, &v); err != nil || len(v.Candidates) == 0 ||
			!strings.Contains(v.Candidates[0].Content.Parts[0].Text, "facts") {
			t.Errorf("response does not decode to the answer: %v %q", err, r.Response)
		}
	})

	t.Run("the body is not filed as an error", func(t *testing.T) {
		if len(r.Errors) != 0 {
			t.Errorf("errors = %v, want none", r.Errors)
		}
		if r.Outcome != outcomeOK {
			t.Errorf("outcome = %q, want ok", r.Outcome)
		}
	})

	t.Run("the events after the body still parse", func(t *testing.T) {
		// The GIN-debug line ends the run; billing and GIN are their own events.
		if r.Quota == nil || *r.Quota != 874 {
			t.Errorf("quota = %v, want 874 - the billing line was swallowed", r.Quota)
		}
		if r.Status == nil || *r.Status != 200 {
			t.Errorf("status = %v, want 200", r.Status)
		}
	})

	t.Run("usage and called tools come from the Gemini shape", func(t *testing.T) {
		u := r.Usage
		if u == nil || u.PromptTokens == nil || u.CompletionTokens == nil || u.TotalTokens == nil {
			t.Fatalf("usage = %+v", u)
		}
		if *u.PromptTokens != 4753 || *u.CompletionTokens != 373 || *u.TotalTokens != 5126 {
			t.Errorf("usage = %d/%d/%d, want 4753/373/5126", *u.PromptTokens, *u.CompletionTokens, *u.TotalTokens)
		}
		if len(r.CalledTools) != 1 || r.CalledTools[0] != "save_facts" {
			t.Errorf("called_tools = %v, want [save_facts]", r.CalledTools)
		}
		if r.IsStream {
			t.Error("is_stream = true for a generateContent call")
		}
	})
}

// Anthropic's non-streaming answer is logged as `responseBody:`, the response
// twin of the `requestBody:` marker above, and was just as unhandled. Its usage
// sits under the same "usage" key OpenAI uses, with different members: decoded
// into the OpenAI-shaped field it is a non-nil struct of nils, which blanks the
// usage pane and also suppresses the fall back to billing.
func TestAnthropicNonStreamResponse(t *testing.T) {
	const rid = "AnthropicNonStreamAaaaBbbb"
	calls := map[string]*Record{}
	scanLines(strings.NewReader(
		`[DEBUG] 2026/10/07 - 09:00:00 | `+rid+` | requestBody: {"model":"claude-opus-5-5","max_tokens":1024,"messages":[{"role":"user","content":"title this"}]}
[DEBUG] 2026/10/07 - 09:00:02 | `+rid+` | responseBody: {"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"thinking","thinking":"short"},{"type":"text","text":"Fix the error banner"},{"type":"tool_use","id":"toolu_1","name":"set_title","input":{"t":"x"}}],"stop_reason":"tool_use","usage":{"input_tokens":12,"cache_read_input_tokens":3000,"output_tokens":40}} 
[INFO] 2026/10/07 - 09:00:02 | `+rid+` | record consume log: userId=1, params={"channel_id":7,"prompt_tokens":3012,"completion_tokens":40,"model_name":"claude-opus-5-5","quota":90}
`), calls)
	r := calls[rid]
	if r == nil {
		t.Fatal("record missing")
	}
	r.finalize()

	if r.Response.empty() {
		t.Fatal("response is empty - responseBody: was not recognised")
	}
	if len(r.Errors) != 0 {
		t.Errorf("errors = %v, want none - the body fell through to the error arm", r.Errors)
	}
	u := r.Usage
	if u == nil || u.PromptTokens == nil || u.CompletionTokens == nil {
		t.Fatalf("usage = %+v - Anthropic usage decoded into the OpenAI shape", u)
	}
	if *u.PromptTokens != 3012 || *u.CompletionTokens != 40 {
		t.Errorf("usage = %d/%d, want 3012/40 (cache folded into prompt)", *u.PromptTokens, *u.CompletionTokens)
	}
	if len(r.CalledTools) != 1 || r.CalledTools[0] != "set_title" {
		t.Errorf("called_tools = %v, want [set_title]", r.CalledTools)
	}
}

// Every relay path's response marker lands in Response, single-line or not.
func TestResponseMarkers(t *testing.T) {
	for _, m := range responseMarkers {
		calls := map[string]*Record{}
		scanLines(strings.NewReader(`[DEBUG] 2026/10/07 - 09:00:00 | ResponseMarkerAaaaBbbbCccc | `+m+` {"candidates":[{"content":{"parts":[{"text":"error-free"}]}}]} `+"\n"), calls)
		r := calls["ResponseMarkerAaaaBbbbCccc"]
		if r == nil || r.Response.empty() {
			t.Errorf("%q: response not captured", m)
			continue
		}
		if len(r.Errors) != 0 {
			t.Errorf("%q: filed as an error: %v", m, r.Errors)
		}
	}
}

// Compaction applies only to bodies that arrived on several lines. A
// single-line body is stored byte-for-byte, spacing and all.
func TestParseRawCompactsOnlyMultiLine(t *testing.T) {
	if got := string(parseRaw(`{"a": 1, "b": [1, 2]}`)); got != `{"a": 1, "b": [1, 2]}` {
		t.Errorf("single-line body changed: %q", got)
	}
	if got := string(parseRaw("{\n  \"a\": \"x y\",\n  \"b\": [\n    1\n  ]\n}")); got != `{"a":"x y","b":[1]}` {
		t.Errorf("multi-line body = %q", got)
	}
	if parseRaw("{\n  \"a\": ") != nil {
		t.Error("a body cut short must be rejected, not stored")
	}
}

// Gemini's candidatesTokenCount excludes thinking. new-api bills thoughts as
// output, so the viewer has to as well, or a call that spent its budget
// thinking reports no output at all. Metadata copied from a production
// gemini-3-flash-lite call that stopped at MAX_TOKENS mid-thought.
func TestGeminiUsageCountsThoughts(t *testing.T) {
	var g geminiUsage
	if err := json.Unmarshal([]byte(`{"promptTokenCount":267,"totalTokenCount":284,"promptTokensDetails":[{"modality":"TEXT","tokenCount":9},{"modality":"IMAGE","tokenCount":258}],"thoughtsTokenCount":17,"serviceTier":"standard"}`), &g); err != nil {
		t.Fatal(err)
	}
	u := g.toUsage()
	if u.CompletionTokens == nil || *u.CompletionTokens != 17 {
		t.Fatalf("completion_tokens = %v, want 17 (thoughts only)", u.CompletionTokens)
	}
	if u.CompletionDetails == nil || u.CompletionDetails.ReasoningTokens == nil || *u.CompletionDetails.ReasoningTokens != 17 {
		t.Errorf("reasoning_tokens = %+v, want 17", u.CompletionDetails)
	}
	if *u.PromptTokens+*u.CompletionTokens != *u.TotalTokens {
		t.Errorf("prompt %d + completion %d != total %d", *u.PromptTokens, *u.CompletionTokens, *u.TotalTokens)
	}

	// And a plain answer with no thinking keeps exactly candidatesTokenCount.
	var plain geminiUsage
	json.Unmarshal([]byte(`{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}`), &plain)
	if p := plain.toUsage(); *p.CompletionTokens != 5 || p.CompletionDetails != nil {
		t.Errorf("plain = %+v", p)
	}
}

// Gemini's request is logged in its native shape - contents with roles
// user/model, text in parts, functions under functionDeclarations - and
// nothing derived from it: every Gemini call listed with no preview, 0
// messages and no tools. The native path's own marker, `Gemini request body:`,
// was not handled at all.
func TestGeminiRequestShape(t *testing.T) {
	const rid = "GeminiRequestShapeAaaaBbbb"
	calls := map[string]*Record{}
	scanLines(strings.NewReader(`[DEBUG] 2026/10/07 - 09:00:00 | `+rid+` | Gemini request body: {"systemInstruction":{"parts":[{"text":"Report an error if unsure."}]},"contents":[{"role":"user","parts":[{"text":"weather in Paris?"}]},{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"get_weather","response":{"c":21}}}]}],"tools":[{"functionDeclarations":[{"name":"get_weather","parameters":{"type":"OBJECT"}},{"name":"get_time"}]},{"googleSearch":{}}]}
[DEBUG] 2026/10/07 - 09:00:02 | `+rid+` | Gemini native response body: {"candidates":[{"content":{"role":"model","parts":[{"text":"21C"}]}}],"usageMetadata":{"promptTokenCount":50,"candidatesTokenCount":2,"totalTokenCount":52}}
[INFO] 2026/10/07 - 09:00:02 | `+rid+` | record consume log: userId=1, params={"channel_id":4,"prompt_tokens":50,"completion_tokens":2,"model_name":"gemini-3.5-flash","quota":3}
`), calls)
	r := calls[rid]
	if r == nil {
		t.Fatal("record missing")
	}
	r.finalize()

	if r.Incomplete || r.Request.empty() {
		t.Fatal("request missing - `Gemini request body:` was not recognised")
	}
	if len(r.Errors) != 0 {
		t.Errorf("errors = %v - the request fell through to the error arm", r.Errors)
	}
	// The functionResponse-only content is a tool result, not the prompt.
	if r.Preview != "weather in Paris?" {
		t.Errorf("preview = %q, want the user's question", r.Preview)
	}
	if r.MsgCount != 3 || r.Turns != 1 {
		t.Errorf("msg_count/turns = %d/%d, want 3/1", r.MsgCount, r.Turns)
	}
	if strings.Join(r.ToolNames, ",") != "get_weather,get_time" || !r.HasTools {
		t.Errorf("tool_names = %v, has_tools = %v", r.ToolNames, r.HasTools)
	}
	if r.Model != "gemini-3.5-flash" {
		t.Errorf("model = %q, want billing's - the native request names none", r.Model)
	}
}

// Rerank calls logged both bodies under markers nothing handled, so every one
// archived with no request - and so flagged 请求体不在当前日志文件中 - and no
// result. The request is logged after model mapping, so the model name has to
// keep coming from billing or the model's history splits in two.
func TestRerankCall(t *testing.T) {
	const rid = "RerankCallAaaaBbbbCcccDddd"
	calls := map[string]*Record{}
	scanLines(strings.NewReader(`[DEBUG] 2026/10/05 - 21:19:24 | `+rid+` | Rerank request body: {"documents":["the build failed with an error","sunny in Paris"],"model":"BAAI/bge-reranker-v2-m3","query":"why did the build fail","return_documents":false,"top_n":2}
[DEBUG] 2026/10/05 - 21:19:25 | `+rid+` | reranker response body: {"id":"01a1","results":[{"index":0,"document":null,"relevance_score":0.91},{"index":1,"document":null,"relevance_score":0.02}],"meta":{"tokens":{"input_tokens":30,"output_tokens":0}}}
[INFO] 2026/10/05 - 21:19:25 | `+rid+` | record consume log: userId=1, params={"channel_id":58,"prompt_tokens":30,"completion_tokens":0,"model_name":"bge-reranker-v2-m3","quota":1}
`), calls)
	r := calls[rid]
	if r == nil {
		t.Fatal("record missing")
	}
	r.finalize()

	if r.Request.empty() || r.Incomplete {
		t.Error("request missing - `Rerank request body:` was not recognised")
	}
	if r.Response.empty() {
		t.Error("response missing - `reranker response body:` was not recognised")
	}
	if len(r.Errors) != 0 {
		t.Errorf("errors = %v - a body containing \"error\" reached the catch-all", r.Errors)
	}
	if r.Model != "bge-reranker-v2-m3" {
		t.Errorf("model = %q, want billing's bge-reranker-v2-m3", r.Model)
	}
	if r.Preview != "why did the build fail" {
		t.Errorf("preview = %q, want the query", r.Preview)
	}
	if r.Usage == nil || r.Usage.PromptTokens == nil || *r.Usage.PromptTokens != 30 {
		t.Errorf("usage = %+v, want billing's 30 prompt tokens", r.Usage)
	}
}

// Gemini streams a function call whole, in one part. Fed through the fragment
// joiner without an index, a second call to the same tool was glued onto the
// first. Before that, they were not read at all.
func TestGeminiStreamFunctionCalls(t *testing.T) {
	const rid = "GeminiStreamCallsAaaaBbbb"
	calls := map[string]*Record{}
	data := func(body string) string {
		return `[DEBUG] 2026/10/07 - 09:00:01 | ` + rid + ` | stream scanner data: data: ` + body + "\n"
	}
	scanLines(strings.NewReader(
		`[DEBUG] 2026/10/07 - 09:00:00 | `+rid+` | text request body: {"contents":[{"role":"user","parts":[{"text":"read both"}]}]}`+"\n"+
			data(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Reading."}]}}]}`)+
			data(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read","args":{"p":"a"}}},{"functionCall":{"name":"read","args":{"p":"b"}}}]}}]}`)+
			data(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read","args":{"p":"c"}}}]}}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":20,"totalTokenCount":29}}`)), calls)
	r := calls[rid]
	r.finalize()

	if len(r.StreamToolCalls) != 3 {
		t.Fatalf("stream_tool_calls = %+v, want 3 separate calls", r.StreamToolCalls)
	}
	for i, want := range []string{`{"p":"a"}`, `{"p":"b"}`, `{"p":"c"}`} {
		if got := r.StreamToolCalls[i].Function; got.Name != "read" || got.Arguments != want {
			t.Errorf("call %d = %s %s, want read %s", i, got.Name, got.Arguments, want)
		}
	}
	if r.StreamContent != "Reading." || r.UnknownCount != 0 {
		t.Errorf("content = %q, unknown = %d", r.StreamContent, r.UnknownCount)
	}
	if strings.Join(r.CalledTools, ",") != "read" {
		t.Errorf("called_tools = %v", r.CalledTools)
	}
}
