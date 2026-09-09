package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// srcArchive builds the archive of a pod that is about to be merged in: a
// couple of days of records, written through the ordinary append path so the
// source is a real v2 archive with a real blob pool.
func srcArchive(t *testing.T, dir string) []*Record {
	t.Helper()
	recs := []*Record{
		pushRecord("20260901aaaaaaaaaaaaaaaa", "2026/09/01 09:00:00", "gpt-4o", 11),
		pushRecord("20260901bbbbbbbbbbbbbbbb", "2026/09/01 09:30:00", "gpt-4o", 22),
		pushRecord("20260902cccccccccccccccc", "2026/09/02 10:00:00", "claude", 33),
	}
	a := newArchive(dir)
	defer a.Close()
	for _, r := range recs {
		if err := a.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	return recs
}

// The migration itself: another pod's archive lands here, readable and complete,
// and a second run adds nothing.
func TestImportIsIdempotent(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	recs := srcArchive(t, src)

	res, err := importArchive(dst, src, "", "oracle")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.stored != len(recs) || res.duplicate != 0 || res.skipped != 0 {
		t.Fatalf("first import: %+v, want %d stored and nothing else", res, len(recs))
	}

	// Record count matches the source, and every record reads back with its
	// request body intact - which for a v2 source means it was reassembled from
	// the source pool and re-lifted into this one.
	q := newQuery(newArchive(dst), 0)
	for _, want := range recs {
		got := q.get(want.RequestID)
		if got == nil {
			t.Fatalf("%s missing after import", want.RequestID)
		}
		if canonJSON(t, got.Request) != canonJSON(t, want.Request) {
			t.Errorf("%s request changed:\n got %s\nwant %s",
				want.RequestID, got.Request, want.Request)
		}
		if got.Pod != "oracle" {
			t.Errorf("%s pod = %q, want the -pod label oracle", want.RequestID, got.Pod)
		}
		if got.Quota == nil || *got.Quota != *want.Quota {
			t.Errorf("%s quota = %v, want %v", want.RequestID, got.Quota, *want.Quota)
		}
	}

	// A re-run - the case that matters, since a 269MB migration gets
	// interrupted - must recognise everything and write nothing.
	again, err := importArchive(dst, src, "", "oracle")
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if again.stored != 0 || again.duplicate != len(recs) {
		t.Errorf("second import: %+v, want 0 stored and %d duplicates", again, len(recs))
	}
	for day, want := range map[string]int{"20260901": 2, "20260902": 1} {
		entries, err := newArchive(dst).index(day)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != want {
			t.Errorf("%s has %d entries after two imports, want %d", day, len(entries), want)
		}
	}
}

// Importing into an archive that already holds the receiving pod's own records
// must merge, not replace: after the migration one arc-DAY carries both pods'
// traffic, which is the entire point.
func TestImportMergesIntoExistingDay(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	srcArchive(t, src)

	own := newArchive(dst)
	local := pushRecord("20260901dddddddddddddddd", "2026/09/01 08:00:00", "local", 5)
	if err := own.Append(local); err != nil {
		t.Fatal(err)
	}
	own.Close()

	if _, err := importArchive(dst, src, "20260901", ""); err != nil {
		t.Fatalf("import: %v", err)
	}
	entries, err := newArchive(dst).index("20260901")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("merged day has %d entries, want 3 (1 local + 2 imported)", len(entries))
	}
	// -pod was empty, so the local record keeps its (absent) label and the
	// imported ones keep theirs. Provenance is never invented.
	q := newQuery(newArchive(dst), 0)
	if rec := q.get(local.RequestID); rec == nil || rec.Pod != "" {
		t.Errorf("local record was relabelled: %+v", rec)
	}
	if rec := q.get("20260901aaaaaaaaaaaaaaaa"); rec == nil {
		t.Error("imported record missing from the merged day")
	}
}

// A single day can be imported on its own, which is how a 269MB migration is
// run in pieces.
func TestImportSingleDay(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	srcArchive(t, src)

	res, err := importArchive(dst, src, "20260902", "")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.stored != 1 {
		t.Errorf("imported %d records for one day, want 1", res.stored)
	}
	if days := newArchive(dst).days(); len(days) != 1 || days[0] != "20260902" {
		t.Errorf("days after a single-day import = %v, want [20260902]", days)
	}
}

// A superseded record must not be imported alongside the version that
// superseded it: the source archive is append-only too, and its reader takes
// the last entry for an id.
func TestImportTakesTheLatestVersion(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	a := newArchive(src)
	rid := "20260901eeeeeeeeeeeeeeee"
	first := pushRecord(rid, "2026/09/01 09:00:00", "wrong-model", 1)
	if err := a.Append(first); err != nil {
		t.Fatal(err)
	}
	fixed := pushRecord(rid, "2026/09/01 09:00:00", "right-model", 2)
	if err := a.Append(fixed); err != nil {
		t.Fatal(err)
	}
	a.Close()

	res, err := importArchive(dst, src, "", "")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.stored != 1 {
		t.Fatalf("imported %d records for one corrected id, want 1", res.stored)
	}
	rec := newQuery(newArchive(dst), 0).get(rid)
	if rec == nil || rec.Model != "right-model" {
		t.Errorf("imported the superseded version: %+v", rec)
	}
}

// An unreadable source record must not abort the day: the rest of the archive
// is still worth importing, and a run that stops on the first bad member would
// have to be nursed by hand through a migration.
func TestImportSkipsUnreadableRecords(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	srcArchive(t, src)

	// Corrupt the data file for one day, leaving its index pointing at bytes
	// that no longer inflate.
	dp := filepath.Join(src, "arc-20260901.jsonl.gz")
	b, err := os.ReadFile(dp)
	if err != nil {
		t.Fatal(err)
	}
	for i := range b {
		b[i] = 'x'
	}
	if err := os.WriteFile(dp, b, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := importArchive(dst, src, "", "")
	if err != nil {
		t.Fatalf("import aborted on an unreadable record: %v", err)
	}
	if res.skipped != 2 {
		t.Errorf("skipped %d unreadable records, want 2", res.skipped)
	}
	if res.stored != 1 {
		t.Errorf("imported %d readable records, want the 1 from the intact day", res.stored)
	}
}

// Importing a directory into itself would read and append the same file pair at
// once. Refuse it rather than discover what that does.
func TestImportRefusesSameDirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := importArchive(dir, dir, "", ""); err == nil {
		t.Error("importing a directory into itself was allowed")
	}
	if _, err := importArchive(dir, dir+string(filepath.Separator)+".", "", ""); err == nil {
		t.Error("importing a directory into itself via a different spelling was allowed")
	}
	// And an empty source is an error rather than a silent success, so a typo
	// in -src cannot be mistaken for a completed migration.
	if _, err := importArchive(dir, t.TempDir(), "", ""); err == nil {
		t.Error("importing an empty archive reported success")
	}
}

// Import goes through the same idempotent append path the receiver uses, so
// assert that path's own contract directly: a repeated id is skipped, a genuine
// one is stored, and the check survives the day being reopened.
func TestAppendNewIdempotency(t *testing.T) {
	dir := t.TempDir()
	a := newArchive(dir)
	rec := pushRecord("20260901aaaaaaaaaaaaaaaa", "2026/09/01 09:00:00", "m", 1)

	stored, err := a.AppendNew(rec)
	if err != nil || !stored {
		t.Fatalf("first AppendNew: stored=%v err=%v", stored, err)
	}
	stored, err = a.AppendNew(rec)
	if err != nil || stored {
		t.Fatalf("second AppendNew: stored=%v err=%v, want it skipped", stored, err)
	}
	// A record on another day is a different day file, and must not be caught
	// by the first day's dedup set.
	other := pushRecord("20260902bbbbbbbbbbbbbbbb", "2026/09/02 09:00:00", "m", 1)
	if stored, err := a.AppendNew(other); err != nil || !stored {
		t.Fatalf("AppendNew across a day boundary: stored=%v err=%v", stored, err)
	}
	// Back to the first day, which has to be reopened and its dedup set rebuilt
	// from the index on disk - the restart case.
	if stored, err := a.AppendNew(rec); err != nil || stored {
		t.Fatalf("AppendNew after reopening the day: stored=%v err=%v, want it skipped", stored, err)
	}
	a.Close()

	// A fresh archive object, i.e. a restarted process, must reach the same
	// verdict.
	b := newArchive(dir)
	defer b.Close()
	if stored, err := b.AppendNew(rec); err != nil || stored {
		t.Fatalf("AppendNew in a new process: stored=%v err=%v, want it skipped", stored, err)
	}
	entries, err := newArchive(dir).index("20260901")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("day has %d entries after four AppendNew calls, want 1", len(entries))
	}
}

// Append keeps its old behaviour: the local ingester relies on being able to
// re-append a corrected record, and that is how a parser fix reaches history.
func TestAppendStillAppendsDuplicates(t *testing.T) {
	dir := t.TempDir()
	a := newArchive(dir)
	defer a.Close()
	rec := pushRecord("20260901aaaaaaaaaaaaaaaa", "2026/09/01 09:00:00", "m", 1)
	for i := 0; i < 2; i++ {
		if err := a.Append(rec); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := newArchive(dir).index("20260901")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("Append wrote %d entries for two calls, want 2 - correcting a "+
			"record must still be possible", len(entries))
	}
	// And the reader still collapses them, so the duplicate is not double
	// counted in the list or in stats.
	res := newQuery(newArchive(dir), 0).stats(statsFilter{
		Since: epochOf("2026/09/01 00:00:00"), Until: epochOf("2026/09/01 23:59:59"),
	}, time.Local)
	if res.Requests != 1 {
		t.Errorf("stats counted %d requests over two versions of one record, want 1", res.Requests)
	}
}
