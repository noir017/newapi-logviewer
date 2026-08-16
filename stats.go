package main

import (
	"sort"
	"time"
)

// stats aggregates the archive into the numbers the dashboard draws. It reads
// the .idx files and nothing else.
//
// That restriction is the whole design. Measured on the production archive: the
// records are 3.0GB of gzip, the indexes 6.3MB - 480x smaller. A full-history
// aggregate over every index costs 65-75ms and 12MB of allocation, so this
// needs no cache, no precomputed rollup, and no background job. Every field
// below therefore has to be answerable from idxEntry; a metric that needs a
// body does not belong on this page.
//
// statsFilter deliberately does NOT embed listFilter, and deliberately has no
// Search or ToolName field. Those two are the only list filters that cannot be
// answered from the index (query.list falls back to inflating bodies for them),
// and a stats page that inflated bodies would read 3GB to draw a bar chart. The
// missing fields make that mistake a compile error rather than a code review.
type statsFilter struct {
	Since int64
	Until int64
	Model string
}

// toList projects onto the shared list filter so the walk, the dedupe, and the
// predicates are literally the same code the list view uses. Only the
// index-answerable fields are set - see the type comment.
func (f statsFilter) toList() listFilter {
	return listFilter{Model: f.Model, Since: f.Since, Until: f.Until}
}

// bucket is one point on the time series.
//
// Requests is the total; OK/Err/Unknown partition it. Unknown is not padding:
// 63% of the production archive predates the outcome field, and collapsing that
// into either side would be a fabricated number on the most-read chart here.
type bucket struct {
	Key      string `json:"key"`   // yyyymmdd, yyyymmddhh, or the week's first day
	Label    string `json:"label"` // what the axis shows
	Requests int    `json:"requests"`
	OK       int    `json:"ok"`
	Err      int    `json:"err"`
	Unknown  int    `json:"unknown"`
	Quota    int64  `json:"quota"` // raw units; the UI divides (see fmtQ)
	Tokens   int64  `json:"tokens"`
}

// modelStat is one row of the model breakdown.
type modelStat struct {
	Model    string `json:"model"`
	Requests int    `json:"requests"`
	Quota    int64  `json:"quota"`
	Tokens   int64  `json:"tokens"`
}

// statsResult is the wire contract with the browser.
type statsResult struct {
	Requests int `json:"requests"`
	OK       int `json:"ok"`
	Err      int `json:"err"`
	Unknown  int `json:"unknown"`

	Quota int64 `json:"quota"`

	// Token totals, plus how many calls actually reported them. Averages are
	// taken over TokenCount, never over Requests: the index carries no tokens
	// for records written before the field existed, and dividing by every
	// request would silently halve the average as history dilutes it.
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	Tokens           int64 `json:"tokens"`
	TokenCount       int   `json:"token_count"`

	// QuotaCount is the same idea for spend: calls with no billing line carry
	// no quota, and the average cost is over those that do.
	QuotaCount int `json:"quota_count"`

	// UnknownTime counts entries dropped for having no usable timestamp. They
	// cannot be placed in a bucket, and matchIndex lets a zero epoch through
	// every time filter, so counting them into the totals would put the same
	// call in today's numbers and last March's at once. Surfaced rather than
	// silently discarded.
	UnknownTime int `json:"unknown_time"`

	Series []bucket    `json:"series"`
	Models []modelStat `json:"models"`

	// Granularity is which bucket size was chosen ("hour"/"day"/"week"), so the
	// UI can label the axis honestly instead of assuming days.
	Granularity string `json:"granularity"`
}

// Bucket granularity thresholds, in days of resolved span. Keyed off the
// resolved width rather than the preset name: "this year" is 15 days in January
// and 365 in December, and a rule that keyed off the word would draw 15 bars in
// one case and 365 two-pixel slivers in the other.
const (
	hourlyMaxDays = 2
	dailyMaxDays  = 120
)

func granularityFor(since, until int64) string {
	// Round up: an inclusive one-day range is 86399 seconds, and truncating
	// division would call it zero days. The bounds are inclusive because
	// matchIndex is.
	days := (until - since + 86399) / 86400
	switch {
	case days <= hourlyMaxDays:
		return "hour"
	case days <= dailyMaxDays:
		return "day"
	default:
		return "week"
	}
}

// bucketKey places one entry on the time axis.
//
// The key comes from the entry's TS string, not from arithmetic on its epoch.
// e.Epoch/86400 would bucket by UTC day, which lines up with neither the day
// files (named from the log's local wall clock by dayOf) nor the reader's
// calendar, and drifts by an hour across a DST boundary. Slicing the timestamp
// is both cheaper and correct by construction, and yyyymmdd sorts
// chronologically as a string - the same property archive.days() relies on.
//
// The file the entry came from is not usable either: a call that starts before
// midnight and is billed after it is filed under its start day, which is the
// behaviour we want and which dayOf(e.TS) reproduces exactly.
func bucketKey(e idxEntry, gran string, loc *time.Location) string {
	day := dayOf(e.TS)
	switch gran {
	case "hour":
		if len(e.TS) >= 13 {
			return day + e.TS[11:13]
		}
		return day + "00"
	case "week":
		return weekStart(day, loc)
	default:
		return day
	}
}

// weekStart snaps a yyyymmdd to the Monday of its week.
func weekStart(day string, loc *time.Location) string {
	t, err := time.ParseInLocation("20060102", day, loc)
	if err != nil {
		return day
	}
	// Go's Weekday has Sunday at 0; shift so Monday starts the week.
	off := (int(t.Weekday()) + 6) % 7
	return t.AddDate(0, 0, -off).Format("20060102")
}

// axisLabels turns a bucket key into what the chart prints.
func bucketLabel(key, gran string) string {
	switch gran {
	case "hour":
		if len(key) == 10 {
			return key[4:6] + "-" + key[6:8] + " " + key[8:10] + ":00"
		}
	case "week", "day":
		if len(key) == 8 {
			return key[4:6] + "-" + key[6:8]
		}
	}
	return key
}

// timeline lists every bucket key in [since,until], including the empty ones.
//
// Zero-filling from the requested range - not from the days that happen to have
// a file - is what stops a gap from being drawn as a slope. daysInRange only
// returns days the archive actually holds, so a week with two quiet days would
// otherwise hand the chart two adjacent points three days apart and the line
// would cut straight across, reading as a gradual decline rather than silence.
func timeline(since, until int64, gran string, loc *time.Location) []string {
	if since <= 0 || until <= 0 || until < since {
		return nil
	}
	start := time.Unix(since, 0).In(loc)
	end := time.Unix(until, 0).In(loc)

	var keys []string
	switch gran {
	case "hour":
		t := time.Date(start.Year(), start.Month(), start.Day(), start.Hour(), 0, 0, 0, loc)
		for !t.After(end) {
			keys = append(keys, t.Format("2006010215"))
			t = t.Add(time.Hour)
		}
	case "week":
		t := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
		off := (int(t.Weekday()) + 6) % 7
		t = t.AddDate(0, 0, -off)
		for !t.After(end) {
			keys = append(keys, t.Format("20060102"))
			t = t.AddDate(0, 0, 7)
		}
	default:
		// Step by calendar day rather than by adding 24h, so a DST transition
		// neither skips a day nor emits one twice.
		t := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
		for !t.After(end) {
			keys = append(keys, t.Format("20060102"))
			t = t.AddDate(0, 0, 1)
		}
	}
	return keys
}

// topModels folds the long tail into one "other" row.
//
// 31 distinct model names appear in the production archive - well past the
// eight-slot ceiling any categorical palette can keep distinguishable. The fold
// keeps the chart readable AND keeps it reconcilable: the rows still sum to the
// request total, which is the assertion that catches a breakdown quietly
// dropping records.
const topModelLimit = 7

// modelOther is the label for the folded tail, and also for calls whose model
// the log never carried. Both are kept in the totals rather than dropped, so
// the breakdown reconciles against the request count.
const modelOther = "other"
const modelUnknown = "(unknown)"

func foldModels(m map[string]*modelStat, limit int) []modelStat {
	out := make([]modelStat, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Requests != out[b].Requests {
			return out[a].Requests > out[b].Requests
		}
		return out[a].Model < out[b].Model
	})
	if len(out) <= limit {
		return out
	}
	rest := modelStat{Model: modelOther}
	for _, s := range out[limit:] {
		rest.Requests += s.Requests
		rest.Quota += s.Quota
		rest.Tokens += s.Tokens
	}
	return append(out[:limit:limit], rest)
}

// stats folds the archive into one dashboard payload.
//
// The walk is shared with the list view (eachMatch), so the two can never
// disagree about which calls exist in a range - a stats page whose request
// count does not match the list's total for the same filter is worse than no
// stats page. Entries are consumed as they arrive rather than collected: at a
// year's scale that is ~620k index entries, and materialising them (as
// candidates does, for pagination) would allocate for no reason.
func (q *query) stats(f statsFilter, loc *time.Location) statsResult {
	if loc == nil {
		loc = time.Local
	}
	gran := granularityFor(f.Since, f.Until)

	res := statsResult{Granularity: gran, Series: []bucket{}, Models: []modelStat{}}
	buckets := map[string]*bucket{}
	models := map[string]*modelStat{}

	q.eachMatch(f.toList(), func(_ string, e idxEntry) {
		// No usable timestamp: unplaceable on the axis, and matchIndex lets a
		// zero epoch satisfy every bound, so counting it would put one call in
		// every range at once. Reported separately instead.
		if e.Epoch <= 0 {
			res.UnknownTime++
			return
		}

		res.Requests++
		switch entryOutcome(e) {
		case outcomeOK:
			res.OK++
		case outcomeErr:
			res.Err++
		default:
			res.Unknown++
		}

		var q64, tok int64
		if e.Quota != nil {
			// Accumulate in the raw integer unit and divide once, at the edge.
			// Converting per record would round 600k times.
			q64 = *e.Quota
			res.Quota += q64
			res.QuotaCount++
		}
		if e.PT != nil || e.CT != nil {
			if e.PT != nil {
				res.PromptTokens += int64(*e.PT)
				tok += int64(*e.PT)
			}
			if e.CT != nil {
				res.CompletionTokens += int64(*e.CT)
				tok += int64(*e.CT)
			}
			res.Tokens += tok
			res.TokenCount++
		}

		key := bucketKey(e, gran, loc)
		b := buckets[key]
		if b == nil {
			b = &bucket{Key: key, Label: bucketLabel(key, gran)}
			buckets[key] = b
		}
		b.Requests++
		b.Quota += q64
		b.Tokens += tok
		switch entryOutcome(e) {
		case outcomeOK:
			b.OK++
		case outcomeErr:
			b.Err++
		default:
			b.Unknown++
		}

		name := e.Model
		if name == "" {
			name = modelUnknown
		}
		ms := models[name]
		if ms == nil {
			ms = &modelStat{Model: name}
			models[name] = ms
		}
		ms.Requests++
		ms.Quota += q64
		ms.Tokens += tok
	})

	// Emit the axis from the requested range so quiet periods are drawn as
	// zeroes rather than skipped. When the caller gave no bounds (rare - the UI
	// always sends a range), fall back to the keys that do exist.
	keys := timeline(f.Since, f.Until, gran, loc)
	if len(keys) == 0 {
		keys = make([]string, 0, len(buckets))
		for k := range buckets {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	res.Series = make([]bucket, 0, len(keys))
	for _, k := range keys {
		if b := buckets[k]; b != nil {
			res.Series = append(res.Series, *b)
			continue
		}
		res.Series = append(res.Series, bucket{Key: k, Label: bucketLabel(k, gran)})
	}

	res.Models = foldModels(models, topModelLimit)
	return res
}
