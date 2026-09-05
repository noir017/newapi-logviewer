package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// jsonEqual compares two JSON blobs for semantic equality. splitRequest
// re-marshals the top-level object (reordering keys) and the messages/tools
// arrays, so a v2 round trip is not byte-identical; only the meaning must
// survive.
func jsonEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("unmarshal got: %v\n%s", err, got)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("unmarshal want: %v\n%s", err, want)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("request not preserved:\n got:  %s\n want: %s", got, want)
	}
}

func msg(role, content string) map[string]any {
	return map[string]any{"role": role, "content": content}
}

func buildReq(system string, messages, tools []map[string]any) Raw {
	m := map[string]any{"model": "claude-opus-4-8", "max_tokens": 1024, "stream": true}
	if system != "" {
		m["system"] = system
	}
	if messages != nil {
		m["messages"] = messages
	}
	if tools != nil {
		m["tools"] = tools
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return Raw(b)
}

func rec(id, day string, req Raw) *Record {
	// dayOf parses "2006/01/02 15:04:05"; build that shape from a yyyymmdd day.
	ts := day[0:4] + "/" + day[4:6] + "/" + day[6:8] + " 10:00:00"
	return &Record{RequestID: id, TS: ts, Request: req, Model: "claude-opus-4-8"}
}

// long is a value comfortably above minLift, the kind of thing worth lifting.
func long(prefix string) string { return prefix + ": " + strings.Repeat("x", 300) }

func TestArchiveV2RoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := newArchive(dir)
	defer a.Close()

	req := buildReq(
		long("you are a careful assistant"),
		[]map[string]any{msg("user", long("first question")), msg("assistant", long("an answer"))},
		[]map[string]any{{"name": "read_file", "description": long("reads a file")}},
	)
	r := rec("r1", "20260902", req)
	orig := append(Raw(nil), req...) // Append must not mutate the caller's record
	if err := a.Append(r); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r.Request, orig) {
		t.Fatalf("Append mutated caller's request:\n got:  %s\n want: %s", r.Request, orig)
	}

	entries, err := a.index("20260902")
	if err != nil || len(entries) != 1 {
		t.Fatalf("index: %v len=%d", err, len(entries))
	}
	got, err := a.fetch("20260902", entries[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.V < archiveVersion {
		t.Fatalf("stored record is not v2: V=%d", got.V)
	}
	jsonEqual(t, got.Request, orig)
}

func TestArchiveV2Dedup(t *testing.T) {
	dir := t.TempDir()
	a := newArchive(dir)
	defer a.Close()

	shared := msg("user", long("shared history line"))
	// Two turns of one session: the second resends the first message verbatim.
	if err := a.Append(rec("r1", "20260902", buildReq("", []map[string]any{shared}, nil))); err != nil {
		t.Fatal(err)
	}
	if err := a.Append(rec("r2", "20260902", buildReq("", []map[string]any{shared, msg("assistant", long("reply"))}, nil))); err != nil {
		t.Fatal(err)
	}

	// Three messages were stored across the two records, but only two are
	// distinct, so the pool must hold two blobs - the shared one once.
	_, bip := a.blobPaths("20260902")
	b, err := os.ReadFile(bip)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Count(strings.TrimSpace(string(b)), "\n") + 1
	if got != 2 {
		t.Fatalf("expected 2 distinct blobs in pool, got %d\n%s", got, b)
	}

	// And the second record still reads back with both messages intact.
	entries, _ := a.index("20260902")
	var e2 idxEntry
	for _, e := range entries {
		if e.RID == "r2" {
			e2 = e
		}
	}
	r2, err := a.fetch("20260902", e2)
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, r2.Request, buildReq("", []map[string]any{shared, msg("assistant", long("reply"))}, nil))
}

// TestArchiveFetchIndependentOfBlobIdx proves the read path never touches
// blob.idx: pointers are physical offsets, so a lost or truncated index costs
// nothing but restart-time dedup.
func TestArchiveFetchIndependentOfBlobIdx(t *testing.T) {
	dir := t.TempDir()
	a := newArchive(dir)
	req := buildReq(long("sys"), []map[string]any{msg("user", long("q"))}, nil)
	if err := a.Append(rec("r1", "20260902", req)); err != nil {
		t.Fatal(err)
	}
	a.Close() // release handles so the day reopens clean

	_, bip := a.blobPaths("20260902")
	if err := os.Remove(bip); err != nil {
		t.Fatal(err)
	}

	b := newArchive(dir)
	defer b.Close()
	entries, _ := b.index("20260902")
	got, err := b.fetch("20260902", entries[0])
	if err != nil {
		t.Fatalf("fetch after blob.idx removed: %v", err)
	}
	jsonEqual(t, got.Request, req)
}

// TestArchiveV1Compat proves a record written before v2 (no version, inline
// request, no pool) is read back unchanged.
func TestArchiveV1Compat(t *testing.T) {
	dir := t.TempDir()
	req := buildReq(long("sys"), []map[string]any{msg("user", long("q"))}, nil)
	writeV1Record(t, dir, rec("old", "20260902", req))

	a := newArchive(dir)
	defer a.Close()
	entries, err := a.index("20260902")
	if err != nil || len(entries) != 1 {
		t.Fatalf("index: %v len=%d", err, len(entries))
	}
	got, err := a.fetch("20260902", entries[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.V != 0 {
		t.Fatalf("v1 record should decode with V==0, got %d", got.V)
	}
	// v1 stores the request inline and byte-for-byte; no pool file should exist.
	jsonEqual(t, got.Request, req)
	if bp, _ := a.blobPaths("20260902"); fileExists(bp) {
		t.Fatalf("v1 read must not create a blob pool at %s", bp)
	}
}

func TestArchiveNoLiftSmallRequest(t *testing.T) {
	dir := t.TempDir()
	a := newArchive(dir)
	defer a.Close()
	// Everything below minLift: nothing is lifted, but the record still reads.
	req := buildReq("hi", []map[string]any{msg("user", "yo")}, nil)
	if err := a.Append(rec("r1", "20260902", req)); err != nil {
		t.Fatal(err)
	}
	_, bip := a.blobPaths("20260902")
	if b, _ := os.ReadFile(bip); len(bytes.TrimSpace(b)) != 0 {
		t.Fatalf("small request should lift nothing, pool idx: %s", b)
	}
	entries, _ := a.index("20260902")
	got, err := a.fetch("20260902", entries[0])
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, got.Request, req)
}

// TestReingestMigratesV1ToV2 drives the existing -reingest path over
// session-shaped v1 history - forty turns that each resend the growing
// transcript - and proves it both preserves every request and collapses the
// duplication that v1 stored in full.
func TestReingestMigratesV1ToV2(t *testing.T) {
	src := t.TempDir()
	const day = "20260902"
	const turns = 40

	// Build a session: turn t carries messages 0..t, each ~2KB, so history grows
	// linearly and the total v1 bytes grow quadratically - the real shape.
	reqFor := func(t int) Raw {
		msgs := make([]map[string]any, 0, t+1)
		for i := 0; i <= t; i++ {
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			msgs = append(msgs, msg(role, long(strings.Repeat("m", 1)+string(rune('A'+i%26))+"#"+strings.Repeat("z", 2000)+"#"+itoa(i))))
		}
		return buildReq(long("system prompt for the session"), msgs, nil)
	}
	originals := make([]Raw, turns)
	for i := 0; i < turns; i++ {
		originals[i] = reqFor(i)
		writeV1Record(t, src, rec(itoa(i), day, originals[i]))
	}

	dst := t.TempDir()
	n, err := reingest(dst, src, day, "")
	if err != nil {
		t.Fatalf("reingest: %v", err)
	}
	if n != turns {
		t.Fatalf("reingest copied %d records, want %d", n, turns)
	}

	// Every migrated record reads back byte-for-byte (semantically).
	a := newArchive(dst)
	defer a.Close()
	entries, _ := a.index(day)
	if len(entries) != turns {
		t.Fatalf("dst index has %d entries, want %d", len(entries), turns)
	}
	byID := map[string]idxEntry{}
	for _, e := range entries {
		byID[e.RID] = e
	}
	for i := 0; i < turns; i++ {
		got, err := a.fetch(day, byID[itoa(i)])
		if err != nil {
			t.Fatalf("fetch turn %d: %v", i, err)
		}
		jsonEqual(t, got.Request, originals[i])
	}

	// The payoff: v2 (records + pool) is far smaller than the v1 records alone.
	v1 := dirBytes(t, src, "arc-"+day+".jsonl.gz")
	v2 := dirBytes(t, dst, "arc-"+day+".jsonl.gz") + dirBytes(t, dst, "arc-"+day+".blob.gz")
	if v2 >= v1 {
		t.Fatalf("v2 not smaller: v1=%d v2=%d", v1, v2)
	}
	t.Logf("session dedup: v1=%dKB  v2=%dKB  (%.1fx smaller)", v1/1024, v2/1024, float64(v1)/float64(v2))
}

func TestLoadBlobMapSkipsTornLine(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/arc-20260902.blob.idx"
	good := `{"h":"abc","o":0,"n":42}` + "\n"
	torn := `{"h":"def","o":42,` // crash mid-write, no newline
	if err := os.WriteFile(path, []byte(good+torn), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadBlobMap(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 {
		t.Fatalf("torn final line should be skipped, got %d entries: %v", len(m), m)
	}
	if m["abc"] != (blobRef{Off: 0, Len: 42}) {
		t.Fatalf("good line not loaded: %v", m)
	}
}

// ---- test helpers ----------------------------------------------------------

func itoa(i int) string {
	return strconv.Itoa(i)
}

func dirBytes(t *testing.T, dir, name string) int64 {
	t.Helper()
	fi, err := os.Stat(dir + "/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// writeV1Record appends rec to the day pair in the pre-v2 format: the record is
// marshaled inline (V stays 0) and framed exactly as the old Append did, with no
// blob pool. This is how history on disk looks; the test drives fetch's v1 path.
func writeV1Record(t *testing.T, dir string, r *Record) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(r) // V omitted -> no "v" key -> v1
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	zw.Write(body)
	zw.Write([]byte("\n"))
	zw.Close()

	a := newArchive(dir)
	dp, ip := a.paths(dayOf(r.TS))
	df, err := os.OpenFile(dp, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	off, _ := df.Seek(0, io.SeekEnd)
	n, err := df.Write(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	df.Close()

	e := makeIdxEntry(r, off, int64(n))
	line, _ := json.Marshal(e)
	inf, err := os.OpenFile(ip, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	inf.Write(append(line, '\n'))
	inf.Close()
}
