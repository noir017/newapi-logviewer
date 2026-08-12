package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// archivedQuery writes recs through the real archive and returns a query over
// it. Filter tests run against the full write-compress-index-read path rather
// than an in-memory slice, so a format bug shows up as a failing filter.
func archivedQuery(t *testing.T, recs []*Record) *query {
	t.Helper()
	dir := t.TempDir()
	arc := newArchive(dir)
	defer arc.Close()
	for _, r := range recs {
		// Health-check lines carry neither body nor billing; the ingester never
		// archives them, so the fixture must not either.
		if r.Request.empty() && r.Billing.empty() {
			continue
		}
		if err := arc.Append(r); err != nil {
			t.Fatalf("append %s: %v", r.RequestID, err)
		}
	}
	return newQuery(newArchive(dir), 0)
}

// A record must survive compression and come back byte-identical in the fields
// the UI renders. Concatenated gzip members are the risky part: a reader that
// runs past its member returns the *next* record's body.
func TestArchiveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	arc := newArchive(dir)
	defer arc.Close()

	mk := func(i int, body string) *Record {
		st := 200
		return &Record{
			RequestID: fmt.Sprintf("Rec%021d", i),
			TS:        fmt.Sprintf("2026/03/04 10:00:%02d", i),
			Status:    &st,
			Model:     "m",
			Request:   Raw(`{"model":"m","messages":[{"role":"user","content":"` + body + `"}]}`),
			Preview:   body,
			Errors:    []LogErr{},
		}
	}
	const n = 12
	for i := 0; i < n; i++ {
		if err := arc.Append(mk(i, strings.Repeat("body", i+1))); err != nil {
			t.Fatal(err)
		}
	}

	rd := newArchive(dir)
	entries, err := rd.index("20260304")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		t.Fatalf("index has %d entries, want %d", len(entries), n)
	}
	// Read in reverse so a stale seek or an over-reading gzip member cannot be
	// masked by sequential access.
	for i := n - 1; i >= 0; i-- {
		rec, err := rd.fetch("20260304", entries[i])
		if err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
		want := strings.Repeat("body", i+1)
		if rec.Preview != want {
			t.Errorf("record %d preview = %q, want %q", i, rec.Preview, want)
		}
		if rec.RequestID != fmt.Sprintf("Rec%021d", i) {
			t.Errorf("record %d rid = %q (wrong member decoded)", i, rec.RequestID)
		}
	}

	// The whole file must still be one valid gzip stream, so an operator can
	// `gunzip -c` a day without this tool.
	dp, _ := rd.paths("20260304")
	raw, err := os.ReadFile(dp)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("archive file is empty")
	}
	if got := countMembers(t, raw); got != n {
		t.Errorf("gunzip yields %d records, want %d", got, n)
	}

	// ...and it must be line-oriented. json.Decoder happily reads back-to-back
	// objects with no separator, so decoding alone does not prove the file is
	// JSONL - `gunzip -c ... | wc -l` returned 0 in production while every
	// record still decoded correctly.
	plain, err := inflateAll(raw)
	if err != nil {
		t.Fatal(err)
	}
	if lines := bytes.Count(plain, []byte("\n")); lines != n {
		t.Errorf("decompressed day has %d newlines, want %d: `gunzip -c | jq` would see one record", lines, n)
	}

	// The index offsets must exactly tile the file. json.Decode stops at the
	// first complete value, so a fetch would still "work" with a wrong length -
	// the bound has to be asserted directly or a drifting offset goes unnoticed
	// until some later change depends on it.
	var want int64
	for i, e := range entries {
		if e.Off != want {
			t.Errorf("entry %d offset = %d, want %d (members must be contiguous)", i, e.Off, want)
		}
		want += e.Len
	}
	if want != int64(len(raw)) {
		t.Errorf("index covers %d bytes, file is %d", want, len(raw))
	}

	// Each recorded extent must be a complete, self-contained gzip member:
	// inflating it alone yields exactly one record and no trailing bytes.
	for i, e := range entries {
		body, err := inflateExact(raw[e.Off : e.Off+e.Len])
		if err != nil {
			t.Fatalf("entry %d is not a standalone gzip member: %v", i, err)
		}
		var rec Record
		if err := json.Unmarshal(body, &rec); err != nil {
			t.Errorf("entry %d does not hold exactly one record: %v", i, err)
		}
	}
}

// inflateAll decompresses every concatenated member as one stream - the
// `gunzip -c whole-file` view.
func inflateAll(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// inflateExact decompresses one member and fails if the extent holds more or
// less than a single complete member.
func inflateExact(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	zr.Multistream(false)
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	if err := zr.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func countMembers(t *testing.T, raw []byte) int {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "a.gz")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := newMultiReader(f)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(zr)
	n := 0
	for {
		var v map[string]any
		if err := dec.Decode(&v); err != nil {
			return n
		}
		n++
	}
}

// Folding is the whole point of the format: chunk lines are 76% of log volume
// and each repeats a ~250-byte envelope around a few characters. The folded
// record must carry the concatenated text, the chunk count, and nothing else
// from the envelope.
func TestIngestFoldsChunks(t *testing.T) {
	spool, arcDir := t.TempDir(), t.TempDir()
	rid := "FoldingAaaaBbbbCcccDddd0"
	path := filepath.Join(spool, "oneapi-20260304100000.log")

	var sb strings.Builder
	sb.WriteString(`[DEBUG] 2026/03/04 - 10:00:00 | ` + rid + ` | text request body: ` +
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}` + "\n")
	words := []string{"第一百", "二十六", "章", " 上狼", "王常"}
	for i, w := range words {
		sb.WriteString(`[DEBUG] 2026/03/04 - 10:00:0` + fmt.Sprint(i+1) + ` | ` + rid +
			` | stream scanner data: data: {"id":"chatcmpl-abc","choices":[{"index":0,` +
			`"delta":{"content":"` + w + `","role":"assistant"},"finish_reason":null,` +
			`"logprobs":null}],"created":1786257871,"model":"m","service_tier":null,` +
			`"system_fingerprint":null,"object":"chat.completion.chunk"}` + "\n")
		sb.WriteString(`[DEBUG] 2026/03/04 - 10:00:0` + fmt.Sprint(i+1) + ` | ` + rid +
			` | stream scanner data:  ` + "\n") // blank keepalive: 18% of real bytes
	}
	sb.WriteString(`[GIN]   2026/03/04 - 10:00:06 | relay | ` + rid +
		` | 200 | 6.0s | 10.0.0.1 | POST /v1/chat/completions` + "\n")
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	arc := newArchive(arcDir)
	defer arc.Close()
	ing := newIngester(spool, arc, 0, 0)
	ing.once()

	rec := newQuery(newArchive(arcDir), 0).get(rid)
	if rec == nil {
		t.Fatal("record was not archived")
	}
	if want := strings.Join(words, ""); rec.StreamContent != want {
		t.Errorf("stream_content = %q, want %q", rec.StreamContent, want)
	}
	if rec.ChunkCount != len(words) {
		t.Errorf("chunk_count = %d, want %d (blank keepalives must not count)",
			rec.ChunkCount, len(words))
	}
	if !rec.IsStream {
		t.Error("is_stream should be true")
	}
	if rec.FirstChunkMS != 1000 {
		t.Errorf("first_chunk_ms = %d, want 1000", rec.FirstChunkMS)
	}

	// The envelope must be gone: no chunk id, no per-chunk object marker.
	blob, _ := json.Marshal(rec)
	for _, gone := range []string{"chatcmpl-abc", "chat.completion.chunk", "system_fingerprint"} {
		if strings.Contains(string(blob), gone) {
			t.Errorf("archived record still carries envelope field %q", gone)
		}
	}
	if int64(len(blob)) >= int64(len(sb.String())) {
		t.Errorf("folded record (%d bytes) is not smaller than the raw log (%d)",
			len(blob), len(sb.String()))
	}
}

// A call is archived only once its GIN line lands. Until then it must stay in
// the spool, and its earlier lines must not be lost when the rest arrives in a
// later pass - the failure mode that silently truncates streamed answers.
func TestIngestWaitsForCompletion(t *testing.T) {
	spool, arcDir := t.TempDir(), t.TempDir()
	rid := "PartialAaaaBbbbCcccDddd0"
	path := filepath.Join(spool, "oneapi-20260304100000.log")

	add := func(s string) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(s)
		f.Close()
	}
	chunk := func(sec int, txt string) string {
		return fmt.Sprintf(`[DEBUG] 2026/03/04 - 10:00:%02d | %s | stream scanner data: data: `+
			`{"choices":[{"index":0,"delta":{"content":"%s"}}]}`+"\n", sec, rid, txt)
	}

	arc := newArchive(arcDir)
	defer arc.Close()
	ing := newIngester(spool, arc, 0, 0)
	q := newQuery(newArchive(arcDir), 0)

	add(`[DEBUG] 2026/03/04 - 10:00:00 | ` + rid + ` | text request body: {"model":"m","stream":true,"messages":[]}` + "\n")
	add(chunk(1, "Hello "))
	ing.once()
	if q.get(rid) != nil {
		t.Fatal("call archived before its GIN line arrived")
	}
	if ing.pendingCount() != 1 {
		t.Errorf("pending = %d, want 1", ing.pendingCount())
	}

	add(chunk(2, "world"))
	add(`[GIN]   2026/03/04 - 10:00:03 | relay | ` + rid + ` | 200 | 3.0s | 10.0.0.1 | POST /v1/chat/completions` + "\n")
	ing.once()

	rec := q.get(rid)
	if rec == nil {
		t.Fatal("call was not archived after completion")
	}
	if rec.StreamContent != "Hello world" {
		t.Errorf("stream_content = %q, want %q (chunks from the first pass must survive)",
			rec.StreamContent, "Hello world")
	}
	if ing.pendingCount() != 0 {
		t.Errorf("pending = %d after archiving, want 0", ing.pendingCount())
	}

	// A further pass with nothing new must not write the record twice.
	ing.once()
	entries, _ := newArchive(arcDir).index("20260304")
	if len(entries) != 1 {
		t.Errorf("archive has %d entries, want 1 (idle pass duplicated the record)", len(entries))
	}
}

// The spool is deletable only because the archive is permanent. Pruning must
// never touch a file with unread bytes, nor the file New API is writing.
func TestSpoolPruneSafety(t *testing.T) {
	spool, arcDir := t.TempDir(), t.TempDir()
	arc := newArchive(arcDir)
	defer arc.Close()

	old := filepath.Join(spool, "oneapi-20260304090000.log")
	cur := filepath.Join(spool, "oneapi-20260304100000.log")
	rec := func(rid, sec string) string {
		return `[DEBUG] 2026/03/04 - 10:00:` + sec + ` | ` + rid + ` | text request body: {"model":"m","messages":[]}` + "\n" +
			`[GIN]   2026/03/04 - 10:00:` + sec + ` | relay | ` + rid + ` | 200 | 1.0s | 10.0.0.1 | POST /v1/chat/completions` + "\n"
	}
	if err := os.WriteFile(old, []byte(rec("OldFileAaaaBbbbCcccDddd0", "01")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cur, []byte(rec("NewFileAaaaBbbbCcccDddd0", "02")), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(old, past, past)

	ing := newIngester(spool, arc, time.Hour, 0)
	ing.once()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("consumed spool file older than the retention window should be removed")
	}
	if _, err := os.Stat(cur); err != nil {
		t.Error("the file New API is still writing must never be removed")
	}
	// Both calls must be in the archive - pruning may not lose data.
	q := newQuery(newArchive(arcDir), 0)
	for _, rid := range []string{"OldFileAaaaBbbbCcccDddd0", "NewFileAaaaBbbbCcccDddd0"} {
		if q.get(rid) == nil {
			t.Errorf("%s missing from the archive after prune", rid)
		}
	}
}

// An unread file must survive pruning even when it is old, otherwise a slow
// ingest pass would silently drop calls.
func TestSpoolPruneKeepsUnread(t *testing.T) {
	spool, arcDir := t.TempDir(), t.TempDir()
	arc := newArchive(arcDir)
	defer arc.Close()

	a := filepath.Join(spool, "oneapi-20260304090000.log")
	b := filepath.Join(spool, "oneapi-20260304100000.log")
	os.WriteFile(a, []byte("[DEBUG] 2026/03/04 - 09:00:00 | UnreadAaaaBbbbCcccDddd00 | text request body: {}\n"), 0o644)
	os.WriteFile(b, []byte("x\n"), 0o644)
	past := time.Now().Add(-5 * time.Hour)
	os.Chtimes(a, past, past)

	ing := newIngester(spool, arc, time.Hour, 0)
	ing.prune([]string{a, b}) // prune without ingesting: nothing has been read
	if _, err := os.Stat(a); os.IsNotExist(err) {
		t.Error("prune removed a spool file whose bytes were never read")
	}
}

// The spool is a fixed-size tmpfs; a full one makes New API's log writes fail.
// Age alone cannot bound it, so the size sweep must drop consumed files even
// when they are well inside the retention window.
func TestSpoolPruneRespectsMaxBytes(t *testing.T) {
	spool, arcDir := t.TempDir(), t.TempDir()
	arc := newArchive(arcDir)
	defer arc.Close()

	var files []string
	for i := 0; i < 4; i++ {
		p := filepath.Join(spool, fmt.Sprintf("oneapi-2026030410%02d00.log", i))
		if err := os.WriteFile(p, make([]byte, 1000), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(time.Duration(i) * time.Minute)
		os.Chtimes(p, mt, mt)
		files = append(files, p)
	}

	ing := newIngester(spool, arc, time.Hour, 2500) // fits two files
	for _, f := range files {
		ing.offsets[f] = 1000 // fully consumed
	}
	ing.prune(files)

	var left int64
	for _, f := range files {
		if st, err := os.Stat(f); err == nil {
			left += st.Size()
		}
	}
	if left > 2500 {
		t.Errorf("spool still %d bytes, want <= 2500", left)
	}
	if _, err := os.Stat(files[len(files)-1]); err != nil {
		t.Error("the newest file must survive the size sweep")
	}
}

// Search must be bounded by the time window the UI already asked for, and
// capped at SEARCH_DAYS when it asked for nothing.
func TestSearchWindowClamps(t *testing.T) {
	q := newQuery(newArchive(t.TempDir()), 7)
	floor := time.Now().AddDate(0, 0, -7).Unix()

	got := q.searchWindow(listFilter{})
	if got.Since < floor-2 || got.Since > floor+2 {
		t.Errorf("unbounded search since = %d, want ~%d (7-day cap)", got.Since, floor)
	}

	// A narrower window the caller chose must be preserved, not widened.
	narrow := time.Now().Add(-15 * time.Minute).Unix()
	if got := q.searchWindow(listFilter{Since: narrow}); got.Since != narrow {
		t.Errorf("15-minute filter widened to %d, want %d", got.Since, narrow)
	}

	// An older request is clamped forward to the cap.
	if got := q.searchWindow(listFilter{Since: floor - 86400*30}); got.Since != floor {
		t.Errorf("30-day search not clamped: since = %d, want %d", got.Since, floor)
	}
}

// Days outside the filter must not be opened at all - that is what makes a
// narrow filter cheap.
func TestDaysInRangeSelectsFiles(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"20260301", "20260302", "20260303"} {
		os.WriteFile(filepath.Join(dir, "arc-"+d+".idx"), []byte("{}\n"), 0o644)
	}
	arc := newArchive(dir)

	if got := len(arc.daysInRange(0, 0)); got != 3 {
		t.Errorf("unfiltered = %d days, want 3", got)
	}
	mar2 := time.Date(2026, 3, 2, 12, 0, 0, 0, time.Local).Unix()
	got := arc.daysInRange(mar2, mar2+600)
	if len(got) != 1 || got[0] != "20260302" {
		t.Errorf("10-minute window opened %v, want [20260302]", got)
	}
}

// A call that never gets a GIN line must still reach the archive eventually.
// Real causes: the client disconnects mid-stream, the gateway restarts, or the
// call straddles a log rotation. Waiting forever would both leak memory and
// lose the call - found while diffing archived ids against the spool.
func TestIngestArchivesStalledCalls(t *testing.T) {
	spool, arcDir := t.TempDir(), t.TempDir()
	rid := "StalledAaaaBbbbCcccDddd0"
	path := filepath.Join(spool, "oneapi-20260304100000.log")

	os.WriteFile(path, []byte(
		`[DEBUG] 2026/03/04 - 10:00:00 | `+rid+` | text request body: {"model":"m","stream":true,"messages":[]}`+"\n"+
			`[DEBUG] 2026/03/04 - 10:00:01 | `+rid+` | stream scanner data: data: {"choices":[{"index":0,"delta":{"content":"half"}}]}`+"\n",
	), 0o644)
	os.WriteFile(filepath.Join(spool, "oneapi-20260304110000.log"), []byte("x\n"), 0o644)

	arc := newArchive(arcDir)
	defer arc.Close()
	ing := newIngester(spool, arc, 0, 0)
	ing.stallAfter = time.Hour // long: nothing should be filed yet
	ing.once()

	q := newQuery(newArchive(arcDir), 0)
	if q.get(rid) != nil {
		t.Fatal("an in-flight call was archived before the stall deadline")
	}

	// Deadline passes with no new lines.
	ing.stallAfter = 0
	ing.once()

	rec := q.get(rid)
	if rec == nil {
		t.Fatal("a stalled call was never archived - it would leak and be lost")
	}
	if !rec.Stalled {
		t.Error("a stalled call must be marked stalled, not presented as finished")
	}
	if rec.Incomplete {
		t.Error("stalled means the response never finished, not that the request body was missing")
	}
	if rec.StreamContent != "half" {
		t.Errorf("stream_content = %q, want %q: partial output must be kept", rec.StreamContent, "half")
	}
	if ing.pendingCount() != 0 {
		t.Errorf("pending = %d after the stall deadline, want 0 (memory would grow)", ing.pendingCount())
	}
}

// reingest is how a parser fix reaches records already archived. It must copy
// the corrected version in without disturbing anything already on disk.
func TestReingestAppendsCorrections(t *testing.T) {
	live, scratch := t.TempDir(), t.TempDir()
	st := 200
	mk := func(rid, content string) *Record {
		return &Record{
			RequestID: rid, TS: "2026/03/04 10:00:00", Status: &st, Model: "m",
			Request: Raw(`{"model":"m"}`), StreamContent: content, Errors: []LogErr{},
		}
	}

	a := newArchive(live)
	a.Append(mk("BrokenAaaaBbbbCcccDdd001", "")) // archived by the old parser
	a.Append(mk("IntactAaaaBbbbCcccDdd002", "fine"))
	a.Close()
	before, err := os.ReadFile(filepath.Join(live, "arc-20260304.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}

	s := newArchive(scratch)
	s.Append(mk("BrokenAaaaBbbbCcccDdd001", "recovered"))
	s.Append(mk("IntactAaaaBbbbCcccDdd002", "fine"))
	s.Close()

	n, err := reingest(live, scratch, "20260304", "BrokenAaaaBbbbCcccDdd001")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("appended %d records, want 1 (-only should restrict the copy)", n)
	}

	// Everything that was on disk must still be there, byte for byte: the
	// existing records' offsets depend on it.
	after, err := os.ReadFile(filepath.Join(live, "arc-20260304.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) <= len(before) || string(after[:len(before)]) != string(before) {
		t.Error("reingest rewrote existing archive bytes instead of appending")
	}

	q := newQuery(newArchive(live), 0)
	if rec := q.get("BrokenAaaaBbbbCcccDdd001"); rec == nil || rec.StreamContent != "recovered" {
		t.Errorf("correction not visible: %+v", rec)
	}
	if rec := q.get("IntactAaaaBbbbCcccDdd002"); rec == nil || rec.StreamContent != "fine" {
		t.Errorf("untouched record damaged: %+v", rec)
	}
	if _, total, _, _ := q.list(listFilter{}, 1, 50); total != 2 {
		t.Errorf("list shows %d rows, want 2", total)
	}

	// Guard against the obvious foot-gun.
	if _, err := reingest(live, live, "20260304", ""); err == nil {
		t.Error("reingest into the same directory should be refused")
	}
}

// The archive is append-only, so the way to correct a record - after a parser
// fix, say - is to append a better version. The reader must then show the new
// one and list the id once, not twice.
func TestAppendedCorrectionSupersedes(t *testing.T) {
	dir := t.TempDir()
	arc := newArchive(dir)
	st := 200
	rid := "CorrectedAaaaBbbbCccc001"

	mk := func(content string) *Record {
		return &Record{
			RequestID: rid, TS: "2026/03/04 10:00:00", Status: &st, Model: "m",
			Request: Raw(`{"model":"m"}`), StreamContent: content,
			ChunkCount: 17, Errors: []LogErr{},
		}
	}
	if err := arc.Append(mk("")); err != nil { // the buggy parse
		t.Fatal(err)
	}
	if err := arc.Append(mk("recovered text")); err != nil { // the fix
		t.Fatal(err)
	}
	arc.Close()

	q := newQuery(newArchive(dir), 0)
	rec := q.get(rid)
	if rec == nil {
		t.Fatal("record missing")
	}
	if rec.StreamContent != "recovered text" {
		t.Errorf("stream_content = %q, want the appended correction", rec.StreamContent)
	}
	if _, total, _, _ := q.list(listFilter{}, 1, 50); total != 1 {
		t.Errorf("list shows %d rows for one id, want 1 (correction must not duplicate)", total)
	}
}

// Google-family channels stream Gemini's native shape, which New API logs
// verbatim instead of translating. Folding discards the raw lines, so a shape
// the parser does not know is not just displayed oddly - it is destroyed.
//
// Found in production: a gemma-4-31b-it call archived chunk_count=17 and 338
// completion tokens with an empty body.
func TestFoldsGeminiNativeChunks(t *testing.T) {
	spool, arcDir := t.TempDir(), t.TempDir()
	rid := "GeminiAaaaBbbbCcccDddd00"
	path := filepath.Join(spool, "oneapi-20260304100000.log")

	// Verbatim shape from the production log, including the thought marker.
	chunk := func(sec int, text string, thought bool) string {
		return fmt.Sprintf(`[DEBUG] 2026/03/04 - 10:00:%02d | %s | stream scanner data: data: `+
			`{"candidates": [{"content": {"parts": [{"text": %q,"thought": %v}],"role": "model"},`+
			`"index": 0}],"usageMetadata": {"promptTokenCount": 9,"candidatesTokenCount": 338,`+
			`"totalTokenCount": 347}}`+"\n", sec, rid, text, thought)
	}
	var sb strings.Builder
	sb.WriteString(`[DEBUG] 2026/03/04 - 10:00:00 | ` + rid + ` | text request body: ` +
		`{"model":"gemma-4-31b-it","stream":true,"messages":[{"role":"user","content":"hi"}]}` + "\n")
	sb.WriteString(chunk(1, "Topic: log folding.", true)) // reasoning
	sb.WriteString(chunk(2, "日志折叠是指", false))
	sb.WriteString(chunk(3, "把重复的日志合并。", false))
	sb.WriteString(`[GIN]   2026/03/04 - 10:00:04 | relay | ` + rid +
		` | 200 | 4.0s | 10.0.0.1 | POST /v1/chat/completions` + "\n")
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(spool, "oneapi-20260304110000.log"), []byte("x\n"), 0o644)

	arc := newArchive(arcDir)
	defer arc.Close()
	newIngester(spool, arc, 0, 0).once()

	rec := newQuery(newArchive(arcDir), 0).get(rid)
	if rec == nil {
		t.Fatal("record was not archived")
	}
	if want := "日志折叠是指把重复的日志合并。"; rec.StreamContent != want {
		t.Errorf("stream_content = %q, want %q", rec.StreamContent, want)
	}
	if want := "Topic: log folding."; rec.StreamReasoning != want {
		t.Errorf("stream_reasoning = %q, want %q (thought parts are not the answer)",
			rec.StreamReasoning, want)
	}
	if rec.Usage == nil || rec.Usage.CompletionTokens == nil || *rec.Usage.CompletionTokens != 338 {
		t.Errorf("usage not carried over from usageMetadata: %+v", rec.Usage)
	}
	if rec.ChunkCount != 3 {
		t.Errorf("chunk_count = %d, want 3", rec.ChunkCount)
	}
	if rec.UnknownCount != 0 {
		t.Errorf("unknown_count = %d: a known shape was treated as unparseable", rec.UnknownCount)
	}
}

// A shape neither parser understands must leave evidence. Folding is lossy, so
// silence here means a future provider format vanishes without trace.
func TestUnknownChunkShapeIsPreserved(t *testing.T) {
	spool, arcDir := t.TempDir(), t.TempDir()
	rid := "UnknownAaaaBbbbCcccDddd0"
	path := filepath.Join(spool, "oneapi-20260304100000.log")

	os.WriteFile(path, []byte(
		`[DEBUG] 2026/03/04 - 10:00:00 | `+rid+` | text request body: {"model":"m","stream":true,"messages":[]}`+"\n"+
			`[DEBUG] 2026/03/04 - 10:00:01 | `+rid+` | stream scanner data: data: {"martian_format":{"words":["hello"]}}`+"\n"+
			`[DEBUG] 2026/03/04 - 10:00:02 | `+rid+` | stream scanner data: data: not-even-json`+"\n"+
			`[GIN]   2026/03/04 - 10:00:03 | relay | `+rid+` | 200 | 3.0s | 10.0.0.1 | POST /v1/chat/completions`+"\n",
	), 0o644)
	os.WriteFile(filepath.Join(spool, "oneapi-20260304110000.log"), []byte("x\n"), 0o644)

	arc := newArchive(arcDir)
	defer arc.Close()
	newIngester(spool, arc, 0, 0).once()

	rec := newQuery(newArchive(arcDir), 0).get(rid)
	if rec == nil {
		t.Fatal("record was not archived")
	}
	if rec.UnknownCount != 2 {
		t.Errorf("unknown_count = %d, want 2", rec.UnknownCount)
	}
	if len(rec.UnknownChunks) != 2 {
		t.Fatalf("unknown_chunks kept %d samples, want 2", len(rec.UnknownChunks))
	}
	if !strings.Contains(rec.UnknownChunks[0], "martian_format") {
		t.Errorf("raw body not preserved: %q", rec.UnknownChunks[0])
	}
	if !strings.Contains(rec.UnknownChunks[1], "not-even-json") {
		t.Errorf("non-JSON body not preserved: %q", rec.UnknownChunks[1])
	}
}

// Dashboard polling must never reach the archive. New API gives every HTTP
// request an id, so on an idle gateway console polls outnumber real calls; the
// old design filtered them when rendering, but a permanent store has to reject
// them at write time.
func TestIngestSkipsHealthChecks(t *testing.T) {
	spool, arcDir := t.TempDir(), t.TempDir()
	path := filepath.Join(spool, "oneapi-20260304100000.log")

	gin := func(rid, kind, code, p string) string {
		return `[GIN]   2026/03/04 - 10:00:01 | ` + kind + ` | ` + rid + ` | ` + code +
			` | 1.0ms | 10.0.0.1 | GET ` + p + "\n"
	}
	body := &strings.Builder{}
	// a console poll: an id and a GIN line, nothing else
	body.WriteString(gin("PollAaaaBbbbCcccDddd0001", "api", "200", "/api/status"))
	// a real call
	body.WriteString(`[DEBUG] 2026/03/04 - 10:00:02 | RealAaaaBbbbCcccDddd0002 | text request body: {"model":"m","messages":[]}` + "\n")
	body.WriteString(gin("RealAaaaBbbbCcccDddd0002", "relay", "200", "/v1/chat/completions"))
	// a failed request with no body: worth keeping, that is what someone looks for
	body.WriteString(gin("FailAaaaBbbbCcccDddd0003", "relay", "502", "/v1/chat/completions"))
	// a poll that only produced billing (upstream shape varies) must be kept
	body.WriteString(`[INFO]  2026/03/04 - 10:00:04 | BillAaaaBbbbCcccDddd0004 | record consume log: userId=1, params={"model_name":"m","quota":3}` + "\n")
	body.WriteString(gin("BillAaaaBbbbCcccDddd0004", "relay", "200", "/v1/chat/completions"))

	if err := os.WriteFile(path, []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(spool, "oneapi-20260304110000.log"), []byte("x\n"), 0o644)

	arc := newArchive(arcDir)
	defer arc.Close()
	newIngester(spool, arc, 0, 0).once()

	q := newQuery(newArchive(arcDir), 0)
	if q.get("PollAaaaBbbbCcccDddd0001") != nil {
		t.Error("a /api/status poll was written to the permanent archive")
	}
	for _, rid := range []string{
		"RealAaaaBbbbCcccDddd0002",
		"FailAaaaBbbbCcccDddd0003",
		"BillAaaaBbbbCcccDddd0004",
	} {
		if q.get(rid) == nil {
			t.Errorf("%s should have been archived", rid)
		}
	}
	if _, total, _, _ := q.list(listFilter{}, 1, 50); total != 3 {
		t.Errorf("archive holds %d records, want 3", total)
	}
}

// The outage this repo actually had: a 256MB tmpfs at 100%, and New API's log
// writes failing for ~22 hours while the container reported healthy and the
// viewer served pages normally.
//
// The cause was structural, not a tuning mistake. New API opens one log file per
// process start and never rotates it, so the single file in the spool is always
// prune's `newest` - permanently exempt from both sweeps. Every prune pass was a
// no-op no matter how far over SPOOL_MAX_MB the spool went, because there were
// no older files to drop.
//
// A fully-consumed live file must therefore be truncated, not skipped.
func TestSpoolTruncatesSingleLiveFile(t *testing.T) {
	spool, arcDir := t.TempDir(), t.TempDir()
	arc := newArchive(arcDir)
	defer arc.Close()

	only := filepath.Join(spool, "oneapi-20260811204357.log")
	if err := os.WriteFile(only, make([]byte, 4000), 0o644); err != nil {
		t.Fatal(err)
	}

	ing := newIngester(spool, arc, time.Hour, 2500)
	ing.offsets[only] = 4000 // every byte already folded into the archive
	ing.prune([]string{only})

	st, err := os.Stat(only)
	if err != nil {
		// Unlinking is not an acceptable fix: New API holds the descriptor open,
		// so the blocks stay allocated while the log becomes unreachable.
		t.Fatalf("the live spool file must still exist, got %v", err)
	}
	if st.Size() != 0 {
		t.Errorf("live spool file is %d bytes, want 0 - spool over SPOOL_MAX_MB was not reclaimed", st.Size())
	}
	if ing.offsets[only] != 0 {
		t.Errorf("offset = %d, want 0 after truncation; a stale offset skips everything written next",
			ing.offsets[only])
	}
}

// Truncation reclaims space by discarding bytes, so it may only ever touch a
// file whose every byte is already in the archive. An unread tail means calls
// that were never folded, and losing those is exactly what the archive exists
// to prevent - better to let the tmpfs fill than to silently drop records.
func TestSpoolNeverTruncatesUnreadBytes(t *testing.T) {
	spool, arcDir := t.TempDir(), t.TempDir()
	arc := newArchive(arcDir)
	defer arc.Close()

	only := filepath.Join(spool, "oneapi-20260811204357.log")
	if err := os.WriteFile(only, make([]byte, 4000), 0o644); err != nil {
		t.Fatal(err)
	}

	ing := newIngester(spool, arc, time.Hour, 2500)
	ing.offsets[only] = 3999 // one byte still unread
	ing.prune([]string{only})

	st, err := os.Stat(only)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 4000 {
		t.Errorf("size = %d, want 4000: truncated a file with unread bytes", st.Size())
	}
}

// Under the high-water mark, nothing is touched. Truncation is a last resort,
// not routine behaviour: the spool doubles as the retry buffer for calls that
// have not finished, and discarding it whenever it was consumed would leave
// nothing to re-read after a restart.
func TestSpoolLeavesLiveFileUnderMaxBytes(t *testing.T) {
	spool, arcDir := t.TempDir(), t.TempDir()
	arc := newArchive(arcDir)
	defer arc.Close()

	only := filepath.Join(spool, "oneapi-20260811204357.log")
	if err := os.WriteFile(only, make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}

	ing := newIngester(spool, arc, time.Hour, 2500)
	ing.offsets[only] = 1000
	ing.prune([]string{only})

	if st, _ := os.Stat(only); st.Size() != 1000 {
		t.Errorf("size = %d, want 1000: truncated while under the high-water mark", st.Size())
	}
}

// /healthz must report write-side health, not liveness.
//
// This is the check that was green through both of this repo's real outages.
// Reads need no write permission, so a viewer that archives nothing still
// serves fast, correct pages; and `pending` is useless as a signal because the
// stall deadline drains it whether writes land or fail.
func TestHealthDetectsBrokenArchive(t *testing.T) {
	spool, arcDir := t.TempDir(), t.TempDir()
	arc := newArchive(arcDir)
	defer arc.Close()
	ing := newIngester(spool, arc, time.Hour, 0)

	t.Run("healthy after a successful append", func(t *testing.T) {
		p := filepath.Join(spool, "oneapi-20260304100000.log")
		os.WriteFile(p, []byte(
			`[DEBUG] 2026/03/04 - 10:00:01 | HealthOkAaaaBbbbCcccDddd | text request body: {"model":"m","messages":[]}`+"\n"+
				`[GIN]   2026/03/04 - 10:00:01 | relay | HealthOkAaaaBbbbCcccDddd | 200 | 1.0s | 10.0.0.1 | POST /v1/chat/completions`+"\n"), 0o644)
		ing.once()

		h := ing.health()
		if ok, _ := h["archive_ok"].(bool); !ok {
			t.Errorf("archive_ok = false, want true: %v", h)
		}
		if got, _ := h["appends"].(int64); got != 1 {
			t.Errorf("appends = %v, want 1", h["appends"])
		}
		if _, ok := h["last_append"]; !ok {
			t.Error("last_append missing; without it staleness is undetectable")
		}
	})

	t.Run("unhealthy while appends fail", func(t *testing.T) {
		// Exactly the production failure: the archive is readable but not
		// writable, so pages keep working while nothing is recorded.
		ing.mu.Lock()
		ing.appendFails, ing.lastErr = 1, "permission denied"
		ing.mu.Unlock()

		h := ing.health()
		if ok, _ := h["archive_ok"].(bool); ok {
			t.Errorf("archive_ok = true while appends are failing: %v", h)
		}
		if h["last_error"] != "permission denied" {
			t.Errorf("last_error = %v, want the append error", h["last_error"])
		}
	})

	t.Run("unhealthy while the spool is over its ceiling", func(t *testing.T) {
		ing2 := newIngester(spool, arc, time.Hour, 10) // 10-byte ceiling
		h := ing2.health()
		if ok, _ := h["archive_ok"].(bool); ok {
			t.Errorf("archive_ok = true with the spool over SPOOL_MAX_MB: %v", h)
		}
		if b, _ := h["spool_bytes"].(int64); b <= 10 {
			t.Errorf("spool_bytes = %v, want > 10", h["spool_bytes"])
		}
	})

	t.Run("idle gateway is healthy, not stale", func(t *testing.T) {
		// An idle gateway archives nothing for hours. A check that goes red
		// overnight is one that gets ignored by morning, so staleness only
		// counts against health when there is work waiting.
		idle := newIngester(t.TempDir(), arc, time.Hour, 0)
		h := idle.health()
		if ok, _ := h["archive_ok"].(bool); !ok {
			t.Errorf("archive_ok = false on an idle gateway with no pending work: %v", h)
		}
	})

	t.Run("unhealthy when calls wait and nothing is written", func(t *testing.T) {
		stuck := newIngester(t.TempDir(), arc, time.Hour, 0)
		stuck.pending["WaitingCallAaaaBbbbCccc0"] = blank("WaitingCallAaaaBbbbCccc0")
		// Never had a successful append: lastAppend is the zero time.
		h := stuck.health()
		if ok, _ := h["archive_ok"].(bool); ok {
			t.Errorf("archive_ok = true with calls pending and no append ever: %v", h)
		}
	})
}

// The endpoint must return 503 when ingest is broken, or nothing external ever
// notices: a container healthcheck and an uptime monitor both key off status.
func TestHealthzStatusCode(t *testing.T) {
	spool, arcDir := t.TempDir(), t.TempDir()
	cfg := loadConfig()
	cfg.LogDir, cfg.ArchiveDir, cfg.AuthMode, cfg.Base = spool, arcDir, "none", ""
	srv := newServer(cfg)
	ing := srv.ing

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))
	if rr.Code != 200 {
		t.Errorf("healthy: code = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}

	ing.mu.Lock()
	ing.appendFails, ing.lastErr = 3, "permission denied"
	ing.mu.Unlock()

	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))
	if rr.Code != 503 {
		t.Errorf("broken: code = %d, want 503 (%s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"ok":false`) {
		t.Errorf("broken body should carry ok=false, got %s", rr.Body.String())
	}
}

// A billed call with no GIN line must archive as a normal, completed record.
//
// gin's access logger writes to gin.DefaultWriter, which is not the file New API
// opens for its own logger. On the live deployment that meant 347,863 DEBUG
// lines in the log file and not one GIN line, while `docker logs` showed them
// arriving on stdout the whole time - and the archived history shows it changed
// mid-flight: Aug 10 had 2,052 records carrying a GIN-only field, Aug 11 onward
// had none.
//
// Relying on the GIN line alone therefore made every call wait out stallAfter
// and land in the archive marked Stalled - a 10-minute delay on every record,
// plus a "response never finished" badge on calls that finished perfectly.
func TestIngestCompletesOnBillingWithoutGIN(t *testing.T) {
	spool, arcDir := t.TempDir(), t.TempDir()
	arc := newArchive(arcDir)
	defer arc.Close()

	p := filepath.Join(spool, "oneapi-20260811204357.log")
	// No [GIN] line anywhere, exactly as the live spool looks.
	os.WriteFile(p, []byte(
		`[DEBUG] 2026/08/11 - 20:54:02 | BilledNoGinAaaaBbbbCccc0 | requestBody: {"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`+"\n"+
			`[DEBUG] 2026/08/11 - 20:54:03 | BilledNoGinAaaaBbbbCccc0 | stream scanner data: data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`+"\n"+
			`[INFO]  2026/08/11 - 20:54:04 | BilledNoGinAaaaBbbbCccc0 | record consume log: userId=1, params={"model_name":"claude-opus-5","quota":42,"channel_id":2,"token_name":"t"}`+"\n"), 0o644)

	ing := newIngester(spool, arc, time.Hour, 0)
	ing.settleAfter = 0 // the quiet period is real but not what this test is about
	ing.once()

	if n := len(ing.pending); n != 0 {
		t.Errorf("pending = %d, want 0: a billed call must not wait for a GIN line", n)
	}
	q := newQuery(newArchive(arcDir), 0)
	got := q.get("BilledNoGinAaaaBbbbCccc0")
	if got == nil {
		t.Fatal("billed call missing from the archive")
	}
	if got.Stalled {
		t.Error("stalled = true; a billed call was delivered and charged, not interrupted")
	}
	if got.StreamContent != "hello" {
		t.Errorf("stream_content = %q, want hello", got.StreamContent)
	}
}

// The quiet period matters: billing is emitted around the end of the response
// and trailing chunks can still follow it in the file, so archiving the instant
// billing lands truncates real content. The record is written once, so it has to
// be written complete.
func TestIngestWaitsForChunksAfterBilling(t *testing.T) {
	spool, arcDir := t.TempDir(), t.TempDir()
	arc := newArchive(arcDir)
	defer arc.Close()

	p := filepath.Join(spool, "oneapi-20260811204357.log")
	os.WriteFile(p, []byte(
		`[DEBUG] 2026/08/11 - 20:54:02 | SettleAaaaBbbbCcccDddd00 | requestBody: {"model":"claude-opus-5","stream":true,"messages":[]}`+"\n"+
			`[INFO]  2026/08/11 - 20:54:04 | SettleAaaaBbbbCcccDddd00 | record consume log: userId=1, params={"model_name":"claude-opus-5","quota":42}`+"\n"), 0o644)

	ing := newIngester(spool, arc, time.Hour, 0) // settleAfter at its default
	ing.once()
	if len(ing.pending) != 1 {
		t.Fatalf("pending = %d, want 1: archived before the quiet period elapsed", len(ing.pending))
	}

	// A trailing chunk lands after billing, as it does in the real log.
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`[DEBUG] 2026/08/11 - 20:54:05 | SettleAaaaBbbbCcccDddd00 | stream scanner data: data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"trailing"}}` + "\n")
	f.Close()

	ing.settleAfter = 0 // the period has now "elapsed"
	ing.once()

	got := newQuery(newArchive(arcDir), 0).get("SettleAaaaBbbbCcccDddd00")
	if got == nil {
		t.Fatal("record missing from the archive")
	}
	if got.StreamContent != "trailing" {
		t.Errorf("stream_content = %q, want trailing: a chunk after billing was lost", got.StreamContent)
	}
}

// A read-only spool file must still be reclaimable.
//
// This is the case every other truncate test missed: they create the file as the
// test user, who can truncate it whatever its mode. In production new-api creates
// the log 0644 as root and the viewer runs as 65534, so truncate returns EPERM -
// and a umask cannot help, because new-api passes the mode explicitly. Verified
// on the running container: /proc/<pid>/status showed Umask 0111 and the file was
// still 0644.
//
// Owning the directory does permit chmod on a file inside it, which is what makes
// the recovery possible.
func TestSpoolTruncatesReadOnlyLiveFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the write bit, so this cannot fail as intended")
	}
	spool, arcDir := t.TempDir(), t.TempDir()
	arc := newArchive(arcDir)
	defer arc.Close()

	only := filepath.Join(spool, "oneapi-20260811204357.log")
	if err := os.WriteFile(only, make([]byte, 4000), 0o444); err != nil {
		t.Fatal(err)
	}
	// Confirm the setup actually denies writes, so a passing test means the
	// chmod-and-retry worked rather than that the mode never mattered.
	if f, err := os.OpenFile(only, os.O_WRONLY, 0); err == nil {
		f.Close()
		t.Skip("filesystem does not enforce the write bit here")
	}

	ing := newIngester(spool, arc, time.Hour, 2500)
	ing.offsets[only] = 4000 // every byte already folded into the archive
	ing.prune([]string{only})

	st, err := os.Stat(only)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 0 {
		t.Errorf("size = %d, want 0: a read-only spool file must still be reclaimed", st.Size())
	}
	if ing.truncErr != "" {
		t.Errorf("truncErr = %q, want empty after a successful retry", ing.truncErr)
	}
}
