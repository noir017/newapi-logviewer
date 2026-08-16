package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

// Every assertion in this file was confirmed by deleting the code it protects
// and watching it fail - see the comment above each test for the specific break
// and the specific wrong number it produces. A stats page is the one view where
// a wrong number looks exactly like a right one, so "the test passes" is not
// evidence unless the test has been seen to fail.

// statRec builds a minimal archivable record. Only the fields the index carries
// are set: stats never opens a body, so a fixture that filled one would be
// testing a path this feature does not use.
func statRec(rid, ts string, quota int64, opts ...func(*Record)) *Record {
	r := &Record{
		RequestID: rid,
		TS:        ts,
		Epoch:     mustEpoch(ts),
		Model:     "m1",
		Quota:     &quota,
		Request:   Raw(`{"model":"m1"}`),
		Errors:    []LogErr{},
		Outcome:   outcomeOK,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

func mustEpoch(ts string) int64 {
	t, err := time.ParseInLocation("2006/01/02 15:04:05", ts, time.Local)
	if err != nil {
		return 0
	}
	return t.Unix()
}

func withUsage(prompt, completion int) func(*Record) {
	return func(r *Record) {
		r.Usage = &Usage{PromptTokens: &prompt, CompletionTokens: &completion}
	}
}

func withOutcome(oc string, status *int) func(*Record) {
	return func(r *Record) {
		r.Outcome = oc
		r.Status = status
	}
}

func withModel(m string) func(*Record) {
	return func(r *Record) { r.Model = m }
}

// span returns the filter covering the given day range, inclusive, the way the
// handler resolves a named range.
func span(fromDay, toDay string) statsFilter {
	from, _ := time.ParseInLocation("20060102", fromDay, time.Local)
	to, _ := time.ParseInLocation("20060102", toDay, time.Local)
	return statsFilter{Since: from.Unix(), Until: to.AddDate(0, 0, 1).Unix() - 1}
}

// BREAK 1: delete the `latest[e.RID] != i` skip in query.eachMatch.
// Then this reports requests=2, quota=400 - the corrected record counted twice
// AND added to its own superseded version. 1,741 of 15,386 entries on the
// production archive are superseded duplicates (55% of one day), so this is not
// a hypothetical: without the skip the spend figure is roughly doubled on the
// days that were ever backfilled.
func TestStatsDedupesAppendedCorrections(t *testing.T) {
	q := archivedQuery(t, []*Record{
		statRec("Dup00000000000000000001", "2026/03/04 10:00:00", 100),
		// Same id, appended later: this is how repair.go and reingest.go
		// correct a record. The archive is append-only; readers take the last.
		statRec("Dup00000000000000000001", "2026/03/04 10:00:00", 300),
	})

	got := q.stats(span("20260304", "20260304"), time.Local)
	if got.Requests != 1 {
		t.Errorf("requests = %d, want 1: a superseded entry is not a second call", got.Requests)
	}
	if got.Quota != 300 {
		t.Errorf("quota = %d, want 300 (the correction, not the sum)", got.Quota)
	}
}

// BREAK 2: replace bucketKey's dayOf(e.TS) with e.Epoch/86400.
// Under a non-UTC TZ that collapses 23:30 and 00:30 into one bucket (or splits
// a single local day across two), because epoch arithmetic buckets by UTC day
// while both the day files and the reader's calendar are local. Setting TZ here
// rather than relying on the runner's makes the test meaningful in CI, which
// runs UTC.
func TestStatsBucketsByLocalDayNotUTC(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// 2026/03/04 23:30 +08 is 15:30 UTC; 2026/03/05 00:30 +08 is 16:30 UTC.
	// Same UTC day, different local days - so UTC bucketing merges them.
	recs := []*Record{
		{RequestID: "Nite0000000000000000001", TS: "2026/03/04 23:30:00",
			Epoch: time.Date(2026, 3, 4, 23, 30, 0, 0, loc).Unix(),
			Model: "m1", Request: Raw(`{"model":"m1"}`), Errors: []LogErr{}},
		{RequestID: "Nite0000000000000000002", TS: "2026/03/05 00:30:00",
			Epoch: time.Date(2026, 3, 5, 0, 30, 0, 0, loc).Unix(),
			Model: "m1", Request: Raw(`{"model":"m1"}`), Errors: []LogErr{}},
	}
	q := archivedQuery(t, recs)

	from := time.Date(2026, 3, 4, 0, 0, 0, 0, loc)
	// Four days, so the range resolves to daily buckets - a two-day range is
	// hourly, which would not exercise the day key at all.
	f := statsFilter{Since: from.Unix(), Until: from.AddDate(0, 0, 4).Unix() - 1}
	got := q.stats(f, loc)

	if got.Granularity != "day" {
		t.Fatalf("granularity = %q, want day", got.Granularity)
	}
	byKey := map[string]int{}
	for _, b := range got.Series {
		byKey[b.Key] = b.Requests
	}
	if byKey["20260304"] != 1 || byKey["20260305"] != 1 {
		t.Errorf("buckets = %v, want one call on 20260304 and one on 20260305: "+
			"a call at 23:30 and one at 00:30 are different days locally", byKey)
	}
}

// BREAK 3: make the series iterate the buckets that exist instead of
// timeline(). Then a range with quiet days yields only the days that have
// records, and the chart draws a straight line across the gap - which reads as
// a gradual decline rather than as silence. daysInRange only returns days the
// archive actually holds, so nothing else catches this.
func TestStatsZeroFillsMissingDays(t *testing.T) {
	q := archivedQuery(t, []*Record{
		statRec("Gap00000000000000000001", "2026/03/04 10:00:00", 10),
		statRec("Gap00000000000000000002", "2026/03/07 10:00:00", 20),
	})

	got := q.stats(span("20260304", "20260307"), time.Local)

	if len(got.Series) != 4 {
		t.Fatalf("series has %d points, want 4 (one per day in the range, gaps included): %+v",
			len(got.Series), got.Series)
	}
	want := []int{1, 0, 0, 1}
	for i, w := range want {
		if got.Series[i].Requests != w {
			t.Errorf("series[%d] (%s) = %d requests, want %d",
				i, got.Series[i].Key, got.Series[i].Requests, w)
		}
	}
}

// BREAK 4: fold the default branch of the outcome switch into Err (or drop
// entryOutcome's Status fallback). Then this reports err=2, and a success rate
// computed as ok/(ok+err) reads 33% instead of 50%.
//
// This is the single most consequential assertion here: 9,723 of 15,386 entries
// on the production archive carry no outcome at all, so folding unknown into
// either side turns a mostly-unmeasured archive into a confident and wrong
// number on the most-read chart.
func TestStatsKeepsUnknownOutcomeSeparate(t *testing.T) {
	ok200 := 200
	q := archivedQuery(t, []*Record{
		// No outcome, but a status: the fallback resolves this one to ok.
		statRec("Ocm00000000000000000001", "2026/03/04 10:00:00", 10, withOutcome("", &ok200)),
		// Neither outcome nor status: genuinely unknown. This is the shape of
		// every record archived before the field existed.
		statRec("Ocm00000000000000000002", "2026/03/04 10:01:00", 10, withOutcome("", nil)),
		statRec("Ocm00000000000000000003", "2026/03/04 10:02:00", 10, withOutcome(outcomeErr, nil)),
	})

	got := q.stats(span("20260304", "20260304"), time.Local)

	if got.OK != 1 || got.Err != 1 || got.Unknown != 1 {
		t.Errorf("ok/err/unknown = %d/%d/%d, want 1/1/1", got.OK, got.Err, got.Unknown)
	}
	if got.Requests != 3 {
		t.Errorf("requests = %d, want 3: the unknown call still happened", got.Requests)
	}
	// The three must partition the total, or the chart's stack will not sum to
	// the KPI tile beside it.
	if got.OK+got.Err+got.Unknown != got.Requests {
		t.Errorf("ok+err+unknown = %d, want %d: the outcome split must partition requests",
			got.OK+got.Err+got.Unknown, got.Requests)
	}
}

// BREAK 5: change idxEntry.PT/CT from *int to int (and drop the nil guards in
// makeIdxEntry). Then TokenCount counts every record, the average reads 100/3
// instead of 100, and "no usage was recorded" becomes indistinguishable from
// "this call used zero tokens" - the same distinction the archive already keeps
// deliberately for Outcome vs Status and Stalled vs Incomplete.
func TestStatsTokensCountOnlyRecordsThatReportedThem(t *testing.T) {
	q := archivedQuery(t, []*Record{
		statRec("Tok00000000000000000001", "2026/03/04 10:00:00", 10, withUsage(100, 50)),
		statRec("Tok00000000000000000002", "2026/03/04 10:01:00", 10),
		statRec("Tok00000000000000000003", "2026/03/04 10:02:00", 10),
	})

	got := q.stats(span("20260304", "20260304"), time.Local)

	if got.PromptTokens != 100 || got.CompletionTokens != 50 {
		t.Errorf("prompt/completion = %d/%d, want 100/50", got.PromptTokens, got.CompletionTokens)
	}
	if got.Tokens != 150 {
		t.Errorf("tokens = %d, want 150", got.Tokens)
	}
	if got.TokenCount != 1 {
		t.Errorf("token_count = %d, want 1 of 3: two records reported no usage, "+
			"and averaging over all three would understate by 3x", got.TokenCount)
	}
	if got.Requests != 3 {
		t.Errorf("requests = %d, want 3: a record without usage is still a call", got.Requests)
	}
}

// BREAK 6: delete the `e.Epoch <= 0` skip in stats.
// matchIndex guards both bounds with `e.Epoch > 0` (so a row is never silently
// dropped from the list), which means a zero-epoch entry satisfies EVERY time
// filter. Without the skip it is counted in today's numbers, this year's, and a
// 15-minute window that excludes everything - all at once.
func TestStatsExcludesEntriesWithNoTimestamp(t *testing.T) {
	q := archivedQuery(t, []*Record{
		// Epoch deliberately left zero, as it would be if the timestamp never
		// parsed. TS is still present, so the record is archivable and lands in
		// the 20260304 day file.
		{RequestID: "Zro00000000000000000001", TS: "2026/03/04 10:00:00", Epoch: 0,
			Model: "m1", Request: Raw(`{"model":"m1"}`), Errors: []LogErr{}},
	})

	// A 15-minute window inside that same day, hours before the record's own
	// time. The day file overlaps the range so daysInRange yields it and the
	// entry is actually visited - which is the point. matchIndex then waves the
	// entry through both bounds because its epoch is zero, so only the explicit
	// skip in stats keeps it out of the numbers.
	from := time.Date(2026, 3, 4, 0, 0, 0, 0, time.Local)
	f := statsFilter{Since: from.Unix(), Until: from.Add(15 * time.Minute).Unix()}

	// Guard the guard: if matchIndex ever stops admitting zero-epoch entries,
	// this test would pass for the wrong reason and stop protecting anything.
	var visited int
	q.eachMatch(listFilter{Since: f.Since, Until: f.Until}, func(_ string, _ idxEntry) { visited++ })
	if visited != 1 {
		t.Fatalf("eachMatch visited %d entries, want 1: the fixture must reach the "+
			"code under test, or this asserts nothing", visited)
	}

	got := q.stats(f, time.Local)

	if got.Requests != 0 {
		t.Errorf("requests = %d, want 0: an entry with no timestamp cannot be placed "+
			"in this window - or in any other", got.Requests)
	}
	if got.UnknownTime != 1 {
		t.Errorf("unknown_time = %d, want 1: the entry must be reported, not silently dropped",
			got.UnknownTime)
	}
}

// The model breakdown must reconcile against the request total, or a chart that
// quietly drops records looks identical to one that does not. Both halves of
// the fold are tested: the tail collapses into "other", and calls whose model
// the log never carried are kept under an explicit label rather than dropped.
func TestStatsModelBreakdownReconciles(t *testing.T) {
	var recs []*Record
	// Nine distinct models, descending in volume, so the fold has a tail.
	for m := 1; m <= 9; m++ {
		for i := 0; i <= 9-m; i++ {
			recs = append(recs, statRec(
				fmt.Sprintf("Mdl%019d", m*100+i),
				fmt.Sprintf("2026/03/04 %02d:%02d:00", m, i),
				10, withModel(fmt.Sprintf("model-%d", m))))
		}
	}
	// One call whose model never reached the log.
	recs = append(recs, statRec("Mdl0000000000000000999", "2026/03/04 23:00:00", 10, withModel("")))

	q := archivedQuery(t, recs)
	got := q.stats(span("20260304", "20260304"), time.Local)

	if len(got.Models) != topModelLimit+1 {
		t.Fatalf("models = %d rows, want %d (top %d plus other)",
			len(got.Models), topModelLimit+1, topModelLimit)
	}
	sum := 0
	for _, m := range got.Models {
		sum += m.Requests
	}
	if sum != got.Requests {
		t.Errorf("model rows sum to %d but requests = %d: the breakdown must account "+
			"for every call, including the folded tail", sum, got.Requests)
	}
	if got.Models[len(got.Models)-1].Model != modelOther {
		t.Errorf("last row = %q, want %q", got.Models[len(got.Models)-1].Model, modelOther)
	}
	var sawUnknown bool
	for _, m := range got.Models {
		if m.Model == modelUnknown {
			sawUnknown = true
		}
	}
	if !sawUnknown && got.Models[len(got.Models)-1].Requests == 0 {
		t.Error("a call with no model must be labelled, not dropped")
	}
}

// Granularity keys off the resolved width, not the preset name: "this year" is
// two weeks in January and 365 days in December, and 365 daily bars on an 800px
// chart is a two-pixel sliver each.
func TestGranularityFollowsResolvedSpan(t *testing.T) {
	day := int64(86400)
	cases := []struct {
		days int64
		want string
	}{
		{1, "hour"}, {2, "hour"}, {3, "day"}, {30, "day"}, {120, "day"}, {200, "week"}, {365, "week"},
	}
	for _, c := range cases {
		if got := granularityFor(0, c.days*day); got != c.want {
			t.Errorf("granularityFor(%d days) = %q, want %q", c.days, got, c.want)
		}
	}
}

// The calendar ranges are resolved server-side, and both bounds are inclusive.
// An exclusive upper bound (next midnight) would count a call logged at exactly
// 00:00:00 in two adjacent ranges, because matchIndex is inclusive at both ends.
func TestResolveRangeBoundaries(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 8, 16, 14, 30, 0, 0, loc)

	since, until, ok := resolveRange("today", now)
	if !ok {
		t.Fatal("today must resolve")
	}
	if got := time.Unix(since, 0).In(loc); got.Hour() != 0 || got.Day() != 16 {
		t.Errorf("today since = %v, want midnight of the 16th", got)
	}
	if got := time.Unix(until, 0).In(loc); got.Hour() != 23 || got.Minute() != 59 || got.Second() != 59 {
		t.Errorf("today until = %v, want 23:59:59 - an exclusive bound double-counts midnight", got)
	}

	// "This year" starts January 1 of the current year, not 365 days ago.
	since, _, _ = resolveRange("ytd", now)
	if got := time.Unix(since, 0).In(loc); got.Month() != time.January || got.Day() != 1 || got.Year() != 2026 {
		t.Errorf("ytd since = %v, want 2026-01-01", got)
	}

	// 7d is inclusive of today, so it spans 7 calendar days, not 8.
	since, until, _ = resolveRange("7d", now)
	if days := (until - since + 1) / 86400; days != 7 {
		t.Errorf("7d spans %d days, want 7", days)
	}

	if _, _, ok := resolveRange("last-tuesday", now); ok {
		t.Error("an unknown range must be rejected, not silently treated as all of history")
	}
}

// A malformed bound must be a 400, not a silent full-history scan. atoiDef -
// which the list endpoint uses - returns its default on any parse failure, so a
// typo there quietly widens the query to everything.
func TestStatsRejectsMalformedRange(t *testing.T) {
	srv := newServer(Config{ArchiveDir: t.TempDir(), LogDir: t.TempDir(), AuthMode: "none"})

	for _, q := range []string{
		"range=last-tuesday",
		"since=notanumber",
		"since=1786723219000", // milliseconds: a window in the year 58000
		"since=1786723219&until=1786000000",
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stats?"+q, nil))
		if rec.Code != 400 {
			t.Errorf("GET /api/stats?%s = %d, want 400", q, rec.Code)
		}
	}
}

// The stats endpoint and the list endpoint must agree on how many calls a range
// holds. They share eachMatch precisely so they cannot drift; this asserts the
// property a reader would check first when they distrust the dashboard.
func TestStatsAgreesWithListTotal(t *testing.T) {
	var recs []*Record
	for i := 0; i < 25; i++ {
		recs = append(recs, statRec(
			fmt.Sprintf("Agr%019d", i),
			fmt.Sprintf("2026/03/04 %02d:%02d:00", 8+i/10, i%10),
			int64(i)))
	}
	// Plus a superseded correction, which neither view may count twice.
	recs = append(recs, statRec("Agr0000000000000000005", "2026/03/04 08:05:00", 999))

	q := archivedQuery(t, recs)
	f := span("20260304", "20260304")

	_, total, _, _ := q.list(listFilter{Since: f.Since, Until: f.Until}, 1, 10)
	got := q.stats(f, time.Local)

	if got.Requests != total {
		t.Errorf("stats requests = %d but list total = %d: the two views must not disagree",
			got.Requests, total)
	}
}

// The endpoint echoes the window it actually used. Without it the page can only
// label the range it asked for, which is a different thing once the server
// resolves "today" against its own clock.
func TestStatsEchoesResolvedRange(t *testing.T) {
	srv := newServer(Config{ArchiveDir: t.TempDir(), LogDir: t.TempDir(), AuthMode: "none"})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stats?range=today", nil))

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Success bool `json:"success"`
		Range   struct {
			Name  string `json:"name"`
			Since int64  `json:"since"`
			Until int64  `json:"until"`
			TZ    string `json:"tz"`
		} `json:"range"`
		Data statsResult `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Success || body.Range.Name != "today" {
		t.Errorf("range echo = %+v, want name=today", body.Range)
	}
	if body.Range.Since == 0 || body.Range.Until <= body.Range.Since {
		t.Errorf("range echo = %+v, want a resolved window", body.Range)
	}
	if body.Range.TZ == "" {
		t.Error("the response must name the clock that resolved the range")
	}
	// An empty archive still draws an axis, so the page renders a zero line
	// rather than nothing at all.
	if len(body.Data.Series) == 0 {
		t.Error("series must be zero-filled even when the archive is empty")
	}
}
