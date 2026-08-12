package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fragTool builds one streaming tool-call fragment the way a provider sends it.
func fragTool(idx int, id, name, args string) ToolCall {
	var tc ToolCall
	i := idx
	tc.Index = &i
	tc.ID, tc.Type = id, "function"
	tc.Function.Name, tc.Function.Arguments = name, args
	return tc
}

func TestRepairRejoinsToolCallFragments(t *testing.T) {
	r := &Record{
		RequestID: "RepairFragAaaaBbbbCccc00",
		TS:        "2026/08/12 10:00:00",
		IsStream:  true,
		Request:   Raw(`{"model":"chat-pro","messages":[{"role":"user","content":"go"}]}`),
		StreamToolCalls: []ToolCall{
			fragTool(0, "call_a", "write", ""),
			fragTool(0, "", "", `{"p"`),
			fragTool(0, "", "", `:"b.sh"}`),
			fragTool(1, "call_b", "read", `{"q":1}`),
		},
		CalledTools: []string{"write", "read"},
	}
	if !repairRecord(r) {
		t.Fatal("repairRecord reported no change on a fragmented record")
	}
	if n := len(r.StreamToolCalls); n != 2 {
		t.Fatalf("stream_tool_calls = %d, want 2", n)
	}
	got := r.StreamToolCalls[0]
	if got.ID != "call_a" || got.Function.Name != "write" || got.Function.Arguments != `{"p":"b.sh"}` {
		t.Errorf("call 0 = %+v; arguments should be the fragments concatenated", got)
	}
	if r.StreamToolCalls[1].Function.Arguments != `{"q":1}` {
		t.Errorf("call 1 arguments = %q", r.StreamToolCalls[1].Function.Arguments)
	}
}

// A record whose tool calls are already whole must be left exactly alone -
// otherwise a repair run rewrites the entire archive and every id gains a
// duplicate member for no reason.
func TestRepairLeavesWholeRecordsAlone(t *testing.T) {
	r := &Record{
		RequestID: "RepairCleanAaaaBbbbCcc00",
		TS:        "2026/08/12 10:00:00",
		IsStream:  true,
		Request: Raw(`{"model":"chat-pro","messages":[` +
			`{"role":"user","content":"go"},{"role":"assistant","content":"ok"}],` +
			`"tools":[{"function":{"name":"write"}}]}`),
		StreamToolCalls: []ToolCall{fragTool(0, "call_a", "write", `{"p":1}`)},
		CalledTools:     []string{"write"},
		MsgCount:        2,
		Turns:           1,
		ToolCount:       1,
	}
	if repairRecord(r) {
		t.Errorf("repairRecord changed an undamaged record: turns=%d msgs=%d tools=%d calls=%+v",
			r.Turns, r.MsgCount, r.ToolCount, r.StreamToolCalls)
	}
}

func TestRepairRecomputesInflatedCounts(t *testing.T) {
	r := &Record{
		RequestID: "RepairCountAaaaBbbbCcc00",
		TS:        "2026/08/12 10:00:00",
		Request: Raw(`{"model":"chat-pro","messages":[` +
			`{"role":"user","content":"a"},{"role":"assistant","content":"b"},` +
			`{"role":"user","content":"c"},{"role":"assistant","content":"d"}],` +
			`"tools":[{"function":{"name":"write"}},{"name":"read"}]}`),
		MsgCount:  52, // 4 x 13 passes
		Turns:     26, // 2 x 13 passes
		ToolCount: 26, // 2 x 13 passes
	}
	if !repairRecord(r) {
		t.Fatal("repairRecord reported no change on inflated counts")
	}
	if r.Turns != 2 || r.MsgCount != 4 || r.ToolCount != 2 {
		t.Errorf("turns=%d msgs=%d tools=%d, want 2/4/2", r.Turns, r.MsgCount, r.ToolCount)
	}
}

// No request body means nothing to recount from. The stored counts are then the
// only ones that will ever exist, and guessing would be worse than keeping them.
func TestRepairKeepsCountsWithoutRequestBody(t *testing.T) {
	r := &Record{
		RequestID: "RepairNoBodyAaaaBbbbCc00",
		TS:        "2026/08/12 10:00:00",
		Turns:     26,
		MsgCount:  52,
	}
	repairRecord(r)
	if r.Turns != 26 || r.MsgCount != 52 {
		t.Errorf("turns=%d msgs=%d; counts must survive a missing body", r.Turns, r.MsgCount)
	}
}

// End to end: the correction is an APPEND, the reader takes the last version,
// and the rewritten .idx carries the corrected counts so the list row agrees
// with the detail pane.
func TestRepairDayAppendsCorrection(t *testing.T) {
	dir := t.TempDir()
	const day = "20260812"
	const rid = "RepairDayAaaaBbbbCccc000"

	arc := newArchive(dir)
	bad := &Record{
		RequestID: rid,
		TS:        "2026/08/12 10:00:00",
		Epoch:     1786550400,
		IsStream:  true,
		Request: Raw(`{"model":"chat-pro","messages":[` +
			`{"role":"user","content":"go"},{"role":"assistant","content":"ok"}]}`),
		StreamToolCalls: []ToolCall{
			fragTool(0, "call_a", "write", `{"p"`),
			fragTool(0, "", "", `:1}`),
		},
		Turns:    13,
		MsgCount: 26,
	}
	if err := arc.Append(bad); err != nil {
		t.Fatal(err)
	}
	arc.Close()

	dataBefore := fileSize(t, filepath.Join(dir, "arc-"+day+".jsonl.gz"))

	t.Run("dry run writes nothing", func(t *testing.T) {
		scanned, fixed, err := repairDay(dir, day, true)
		if err != nil {
			t.Fatal(err)
		}
		if scanned != 1 || fixed != 1 {
			t.Errorf("scanned=%d fixed=%d, want 1/1", scanned, fixed)
		}
		if got := fileSize(t, filepath.Join(dir, "arc-"+day+".jsonl.gz")); got != dataBefore {
			t.Errorf("data file grew during a dry run: %d -> %d", dataBefore, got)
		}
	})

	scanned, fixed, err := repairDay(dir, day, false)
	if err != nil {
		t.Fatal(err)
	}
	if scanned != 1 || fixed != 1 {
		t.Fatalf("scanned=%d fixed=%d, want 1/1", scanned, fixed)
	}

	// The original member is still there: a correction appends, never rewrites.
	if got := fileSize(t, filepath.Join(dir, "arc-"+day+".jsonl.gz")); got <= dataBefore {
		t.Errorf("data file did not grow: %d -> %d; the fix must be appended", dataBefore, got)
	}

	q := newQuery(newArchive(dir), 0)
	got := q.get(rid)
	if got == nil {
		t.Fatal("record missing after repair")
	}
	if n := len(got.StreamToolCalls); n != 1 {
		t.Fatalf("stream_tool_calls = %d, want 1", n)
	}
	if a := got.StreamToolCalls[0].Function.Arguments; a != `{"p":1}` {
		t.Errorf("arguments = %q, want {\"p\":1}", a)
	}
	if got.Turns != 1 || got.MsgCount != 2 {
		t.Errorf("turns=%d msgs=%d, want 1/2", got.Turns, got.MsgCount)
	}

	// And the list row, which reads only the .idx, must agree.
	rows, _, _, _ := q.list(listFilter{}, 1, 10)
	if len(rows) == 0 {
		t.Fatal("no list rows")
	}
	if rows[0].Turns != 1 || rows[0].MsgCount != 2 {
		t.Errorf("idx row turns=%d msgs=%d, want 1/2 - Append must write a fresh index entry",
			rows[0].Turns, rows[0].MsgCount)
	}

	// Idempotent: a second run finds nothing left to do.
	_, fixed2, err := repairDay(dir, day, false)
	if err != nil {
		t.Fatal(err)
	}
	if fixed2 != 0 {
		t.Errorf("second repair fixed %d records, want 0 - repair must be idempotent", fixed2)
	}
	_ = fixed
}

// An unreadable member must not abort the day: the remaining records are still
// repairable, and skipping leaves the bad one as readable as it was.
func TestRepairSkipsUnreadableMember(t *testing.T) {
	dir := t.TempDir()
	const day = "20260812"

	arc := newArchive(dir)
	for _, rid := range []string{"RepairSkipAaaaBbbbCcc001", "RepairSkipAaaaBbbbCcc002"} {
		if err := arc.Append(&Record{
			RequestID: rid,
			TS:        "2026/08/12 10:00:00",
			IsStream:  true,
			Request:   Raw(`{"messages":[{"role":"assistant","content":"x"}]}`),
			Turns:     9,
			MsgCount:  9,
		}); err != nil {
			t.Fatal(err)
		}
	}
	arc.Close()

	// Point the first index entry at an offset that holds no gzip member.
	ip := filepath.Join(dir, "arc-"+day+".idx")
	raw, err := os.ReadFile(ip)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("index has %d lines, want 2", len(lines))
	}
	lines[0] = strings.Replace(lines[0], `"o":0`, `"o":999999`, 1)
	if err := os.WriteFile(ip, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	scanned, fixed, err := repairDay(dir, day, false)
	if err != nil {
		t.Fatalf("repairDay aborted on one bad member: %v", err)
	}
	if scanned != 1 || fixed != 1 {
		t.Errorf("scanned=%d fixed=%d, want 1/1 - the readable record should still be repaired", scanned, fixed)
	}
}

func fileSize(t *testing.T, p string) int64 {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

// Two DISTINCT calls can legally share an index - some providers restart the
// index per call - and each carries complete JSON arguments. Merging them would
// concatenate into `{"p":1}{"p":2}`, which parses as neither: a repair that
// corrupts good records is worse than one that leaves a wrong count alone.
func TestRepairKeepsWholeCallsSharingAnIndex(t *testing.T) {
	r := &Record{
		RequestID: "RepairSameIdxAaaaBbbbCc0",
		TS:        "2026/08/12 10:00:00",
		IsStream:  true,
		StreamToolCalls: []ToolCall{
			fragTool(0, "call_a", "write", `{"p":1}`),
			fragTool(0, "call_b", "write", `{"p":2}`),
		},
		CalledTools: []string{"write"},
	}
	repairRecord(r)
	if n := len(r.StreamToolCalls); n != 2 {
		t.Fatalf("stream_tool_calls = %d, want 2: two complete calls were merged into one", n)
	}
	for i, tc := range r.StreamToolCalls {
		if !json.Valid([]byte(tc.Function.Arguments)) {
			t.Errorf("call %d arguments are no longer valid JSON: %q", i, tc.Function.Arguments)
		}
	}
}

// The repair must join fragments whose arguments happen to be valid JSON on
// their own. `{"a":1}` followed by a nameless `{"b":2}` fragment is still a
// fragment, because a continuation never carries a name.
func TestRepairJoinsFragmentsWithParseableSlices(t *testing.T) {
	r := &Record{
		RequestID: "RepairValidFragAaaaBbbb0",
		TS:        "2026/08/12 10:00:00",
		IsStream:  true,
		StreamToolCalls: []ToolCall{
			fragTool(0, "call_a", "write", `"x"`),
			fragTool(0, "", "", `"y"`),
		},
	}
	if !repairRecord(r) {
		t.Fatal("nameless continuation fragment was not recognised")
	}
	if n := len(r.StreamToolCalls); n != 1 {
		t.Fatalf("stream_tool_calls = %d, want 1", n)
	}
}

// Some providers repeat the function name on every delta instead of only the
// opener. Then no fragment is nameless, and the only remaining evidence is that
// the arguments are a slice of JSON rather than a whole value.
func TestRepairJoinsFragmentsThatRepeatTheName(t *testing.T) {
	r := &Record{
		RequestID: "RepairRepeatNameAaaaBbb0",
		TS:        "2026/08/12 10:00:00",
		IsStream:  true,
		StreamToolCalls: []ToolCall{
			fragTool(0, "call_a", "write", `{"p"`),
			fragTool(0, "", "write", `:1}`),
		},
	}
	if !repairRecord(r) {
		t.Fatal("fragments were not recognised when every one carries the name")
	}
	if n := len(r.StreamToolCalls); n != 1 {
		t.Fatalf("stream_tool_calls = %d, want 1", n)
	}
	if a := r.StreamToolCalls[0].Function.Arguments; a != `{"p":1}` {
		t.Errorf("arguments = %q, want {\"p\":1}", a)
	}
}

// The damaged records contain the same fragment sequence more than once - the
// ingester re-read spool bytes it had already folded, which is the same
// double-counting that multiplied Turns. Joining every copy yields
// `{"a":1}{"a":1}`, so the tail has to go: arguments are exactly one JSON value.
func TestTrimReread(t *testing.T) {
	cases := []struct{ in, want, why string }{
		{`{"a":1}`, `{"a":1}`, "a single value is untouched"},
		{`{"a":1}{"a":1}`, `{"a":1}`, "an exact duplicate is dropped"},
		{`{"a":1}{"b":2}`, `{"a":1}`, "a differing tail is still a tail"},
		{`{"a":1}{"a":`, `{"a":1}`, "a partial tail is dropped"},
		{"", "", "empty stays empty"},
		{`{"a":`, `{"a":`, "an incomplete value is left for a human to see"},
		{`not json at all`, `not json at all`, "unparseable input is not guessed at"},
		{`{"cmd":"a{\"b\":1}"}`, `{"cmd":"a{\"b\":1}"}`, "JSON nested in a string is not a second value"},
	}
	for _, c := range cases {
		if got := trimReread(c.in); got != c.want {
			t.Errorf("trimReread(%q) = %q, want %q - %s", c.in, got, c.want, c.why)
		}
	}
}

// End to end through repairRecord: fragments that were captured twice must
// produce one call whose arguments parse.
func TestRepairDropsRereadDuplication(t *testing.T) {
	r := &Record{
		RequestID: "RepairRereadAaaaBbbbCcc0",
		TS:        "2026/08/12 10:00:00",
		IsStream:  true,
		StreamToolCalls: []ToolCall{
			fragTool(0, "call_a", "Bash", `{"cmd"`),
			fragTool(0, "", "", `:"ls"}`),
			fragTool(0, "", "", `{"cmd"`), // the re-read copy
			fragTool(0, "", "", `:"ls"}`),
		},
	}
	if !repairRecord(r) {
		t.Fatal("no change reported")
	}
	if n := len(r.StreamToolCalls); n != 1 {
		t.Fatalf("stream_tool_calls = %d, want 1", n)
	}
	got := r.StreamToolCalls[0].Function.Arguments
	if got != `{"cmd":"ls"}` {
		t.Errorf("arguments = %q, want {\"cmd\":\"ls\"}", got)
	}
	if !json.Valid([]byte(got)) {
		t.Errorf("arguments are not valid JSON: %q", got)
	}
}

// The second re-read shape: the first capture was cut off mid-value and the
// whole value then restarted. Recovering it requires the dropped prefix to be a
// genuine prefix of what follows, which is what distinguishes a restart from two
// unrelated values.
func TestTrimRereadRecoversTruncatedRestart(t *testing.T) {
	cases := []struct{ in, want, why string }{
		{`{"a{"a":1}`, `{"a":1}`, "partial capture then a complete restart"},
		{`{"cmd": "c{"cmd": "cd x"}`, `{"cmd": "cd x"}`, "the production shape"},
		// Must NOT fire: the leading text is not a prefix of the trailing value,
		// so this is not a restart and nothing may be dropped.
		{`{"z":9}x{"a":1}`, `{"z":9}`, "a complete first value still wins"},
		{`{"b{"a":1}`, `{"b{"a":1}`, "prefix does not match: left alone"},
	}
	for _, c := range cases {
		if got := trimReread(c.in); got != c.want {
			t.Errorf("trimReread(%q) = %q, want %q - %s", c.in, got, c.want, c.why)
		}
	}
}

// The third re-read shape: the duplicate resumed partway through the value, so
// the head and the tail overlap and the tail alone is missing a middle slice.
// This is the shape 8 production records were left in after the first two
// shapes were handled.
func TestTrimRereadSplicesOverlappingReread(t *testing.T) {
	whole := `{"cmd": "pytest tests/control/state_machine/test_x.py -q", "n": 4}`

	// The corruption is whole[:a] + whole[b:] with b < a: the capture broke at
	// a, and the re-read resumed from the earlier offset b, so the bytes between
	// b and a appear twice. Splitting inside the trailing structure (rather than
	// inside a string literal) is what makes the damage visible as invalid JSON,
	// which is the case worth repairing - a duplicate confined to a string
	// literal still parses and is left alone by design.
	a := strings.Index(whole, `"n": 4`) + 3
	b := a - 12
	damaged := whole[:a] + whole[b:]
	if json.Valid([]byte(damaged)) {
		t.Fatalf("the fixture is supposed to be damaged: %q", damaged)
	}

	got := trimReread(damaged)
	if !json.Valid([]byte(got)) {
		t.Fatalf("trimReread left invalid JSON: %q", got)
	}
	if got != whole {
		t.Errorf("trimReread = %q, want %q", got, whole)
	}
}

// Two genuinely different values that happen to share a boundary byte must not
// be spliced into a plausible-looking third thing.
func TestTrimRereadDoesNotInventValues(t *testing.T) {
	// No overlap relationship: any splice here would be fabrication.
	in := `{"a":1} garbage {"b":`
	if got := trimReread(in); got != `{"a":1}` {
		t.Errorf("trimReread(%q) = %q; only the complete leading value may be kept", in, got)
	}
}

// Bounded scan: a large argument string must not send the overlap search
// quadratic. Correctness is covered above; this is about not hanging.
func TestTrimRereadIsBoundedOnLargeInput(t *testing.T) {
	big := `{"content":"` + strings.Repeat("x", 300_000) + `"}`
	damaged := big[:len(big)-1] + big[:len(big)-1] // no clean recovery available
	done := make(chan string, 1)
	go func() { done <- trimReread(damaged) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("trimReread did not finish within 20s on a 600KB argument string")
	}
}
