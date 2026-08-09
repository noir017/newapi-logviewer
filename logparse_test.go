package main

import (
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
		shown, total, _, _ := q.list(listFilter{}, 1, 100)
		for _, r := range shown {
			if r.RequestID == "3fJ7kQmZxR2wLpVnB8sTyHdA" {
				t.Error("/api/status health check should not be listed")
			}
		}
		if total != 5 {
			t.Errorf("shown = %d, want 5", total)
		}
	})

	t.Run("filters", func(t *testing.T) {
		q := archivedQuery(t, recs)
		cases := []struct {
			name string
			f    listFilter
			want int
		}{
			{"tools", listFilter{Tools: "1"}, 2},
			{"tool_name", listFilter{ToolName: "send_email"}, 1},
			{"tool_name missing", listFilter{ToolName: "nope"}, 0},
			{"stream", listFilter{Stream: "1"}, 1},
			{"non-stream", listFilter{Stream: "0"}, 4},
			{"ok", listFilter{Status: "ok"}, 4},
			{"err", listFilter{Status: "err"}, 1},
			{"errors", listFilter{ErrorsOnly: true}, 1},
			{"model", listFilter{Model: "demo/chat-mini"}, 2},
			{"search hit", listFilter{Search: "berlin"}, 1},
			{"search miss", listFilter{Search: "ZZZ"}, 0},
			{"until", listFilter{Until: 1}, 0},
			{"since", listFilter{Since: 1}, 5},
		}
		for _, c := range cases {
			_, got, _, _ := q.list(c.f, 1, 100)
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
