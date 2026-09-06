package main

import (
	"sort"
	"strconv"
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
	Token string
	// Stream is "1" | "0" | "" - index-answerable (IsStream), so it satisfies
	// the same rule as Model and Token. It pairs with the stream-share
	// dimension: a reader who sees 90% streaming can slice to the other 10%.
	Stream string
}

// toList projects onto the shared list filter so the walk, the dedupe, and the
// predicates are literally the same code the list view uses. Only the
// index-answerable fields are set - see the type comment.
func (f statsFilter) toList() listFilter {
	return listFilter{Model: f.Model, Token: f.Token, Since: f.Since, Until: f.Until,
		Stream: f.Stream}
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

	// Input/output split of Tokens, so the usage trend can stack the two rather
	// than plot one opaque total. Carried per bucket for the same reason Tokens
	// is: both come straight off PT/CT in the index, so splitting them costs a
	// second add, not a body read.
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`

	// Cache-read and reasoning volume in the bucket, so the token trend can
	// show how much of the input was served from cache rather than billed as
	// fresh. Zero on indexes that predate the fields - the totals' CachedCount
	// tells the UI whether that zero is real or a coverage gap.
	CachedTokens    int64 `json:"cached_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`

	// Wall-clock latency (from the GIN line's duration string) and
	// time-to-first-response (from the billing line's frt), carried as sums
	// over the entries that reported each. Averages are taken over the counts,
	// never over Requests - same coverage rule as TokenCount, and on this
	// deployment LatencyCount is zero because GIN logs elsewhere.
	LatSumMS int64 `json:"lat_sum_ms"`
	LatCount int   `json:"lat_count"`
	FRTSumMS int64 `json:"frt_sum_ms"`
	FRTCount int   `json:"frt_count"`
}

// modelStat is one row of the model breakdown.
type modelStat struct {
	Model    string `json:"model"`
	Requests int    `json:"requests"`
	Quota    int64  `json:"quota"`
	Tokens   int64  `json:"tokens"`

	// Split, so the breakdown can rank by token volume and still show what the
	// volume is made of. Same index-only cost as the bucket fields above.
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`

	// Err and the latency accumulators let the breakdown rank by failure rate
	// and by speed, not just volume - a model that is cheap but slow, or one
	// that fails a fifth of its calls, reads very differently from its request
	// rank. The rate the UI shows is err/(ok+err) per row, never err/requests:
	// 63% of this archive predates the outcome field, and dividing by every
	// request would read that silence as failure.
	OK       int   `json:"ok"`
	Err      int   `json:"err"`
	LatSumMS int64 `json:"lat_sum_ms"`
	LatCount int   `json:"lat_count"`
	FRTSumMS int64 `json:"frt_sum_ms"`
	FRTCount int   `json:"frt_count"`

	// Cached input volume, so the model ranking can answer "which model's
	// prompts are mostly cache reads". Not averaged - the totals carry the
	// coverage count.
	CachedTokens int64 `json:"cached_tokens"`
}

// chanStat is one row of the upstream-channel breakdown.
//
// Named by the resolved channel name when the resolver has one, else the
// upstream host recorded in the index, else the bare id - the same ladder the
// list view's chip uses. The label is resolved at query time (not stored) so a
// renamed channel re-labels its history.
type chanStat struct {
	Name     string `json:"name"`
	Requests int    `json:"requests"`
	OK       int    `json:"ok"`
	Err      int    `json:"err"`
	Quota    int64  `json:"quota"`
	Tokens   int64  `json:"tokens"`
}

// tokenStat is one row of the per-token spend breakdown.
//
// Requests is carried alongside Quota because the two disagree, often sharply,
// and the disagreement is the point: on one production day the `laptop` token
// spent 967,645 units across 13 calls while `hermes` spent 55,053 across 150.
// A breakdown showing only the volume would rank those two backwards.
type tokenStat struct {
	Token    string `json:"token"`
	Requests int    `json:"requests"`
	Quota    int64  `json:"quota"`
	Tokens   int64  `json:"tokens"`

	// Split, so the per-token view can be ranked by token volume as well as by
	// spend - the two invert exactly as often here as they do for models.
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
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

	// Cache-read and reasoning totals, each with its own coverage count -
	// the pair rule from TokenCount again. CacheHitRate is computed by the UI
	// as CachedTokens / PromptTokens over the records that reported both, so
	// PromptTokens stays the denominator it already publishes.
	CachedTokens    int64 `json:"cached_tokens"`
	CachedCount     int   `json:"cached_count"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	ReasoningCount  int   `json:"reasoning_count"`

	// Latency totals. LatSumMS/LatMaxMS are over LatCount calls whose GIN
	// line carried a duration - zero on deployments (like this one) where gin
	// logs elsewhere. FRT is the fallback that exists here: first-response
	// time from the billing line, present on most billed calls.
	LatSumMS int64 `json:"lat_sum_ms"`
	LatMaxMS int64 `json:"lat_max_ms"`
	LatCount int   `json:"lat_count"`
	FRTSumMS int64 `json:"frt_sum_ms"`
	FRTMaxMS int64 `json:"frt_max_ms"`
	FRTCount int   `json:"frt_count"`

	// Shape-of-traffic counters. Streamed/Tools count calls, ToolDefs sums
	// the tool catalogue sizes so the UI can show both "how often tools are
	// offered" and "how deep the offering is".
	Streamed int `json:"streamed"`
	Tools    int `json:"tools"`
	ToolDefs int `json:"tool_defs"`

	// Hours is requests by local hour of day (24 slots), DowHours by
	// weekday-x-hour (Monday first), both over the selected range. The
	// weekday rows let the heatmap keep weekends visible rather than folding
	// them into one average.
	Hours    []int   `json:"hours"`
	DowHours [][]int `json:"dow_hours"`

	// Channels is the upstream-channel breakdown, ranked by requests.
	Channels []chanStat `json:"channels"`

	// UnknownTime counts entries dropped for having no usable timestamp. They
	// cannot be placed in a bucket, and matchIndex lets a zero epoch through
	// every time filter, so counting them into the totals would put the same
	// call in today's numbers and last March's at once. Surfaced rather than
	// silently discarded.
	UnknownTime int `json:"unknown_time"`

	Series []bucket    `json:"series"`
	Models []modelStat `json:"models"`

	// TokenStats is the per-API-token spend breakdown, ranked by quota.
	//
	// Named for the field rather than the concept because statsResult.Tokens is
	// already the token *count* total - two unrelated meanings of the word, one
	// from the billing line and one from usage. The json name says which.
	TokenStats []tokenStat `json:"tokens_by_token"`

	// TokenNameCount is how many counted calls carried a token name at all.
	//
	// This exists for the same reason TokenCount does, and guards the same
	// mistake in a nastier form. An index written before the TN field existed
	// decodes it as "" - indistinguishable, per entry, from a call that genuinely
	// had no billing line. Zero here against a non-zero Requests means the index
	// predates the field and the whole breakdown is one big unattributed row, so
	// the UI can say "run -reindex" instead of rendering an honest-looking chart
	// that attributes 100% of a month's spend to nobody.
	TokenNameCount int `json:"token_name_count"`

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

// tokenUnnamed labels calls that carried no token name.
//
// Kept as a row rather than dropped, for the same reason modelUnknown is: the
// breakdown has to account for every call or it cannot be reconciled against
// the request total, and a silently-dropped row is exactly the failure that
// reconciliation exists to catch. Its spend is always zero - the name and the
// quota come from the same billing line, verified across the production archive
// - so it never distorts the ranking it appears in.
const tokenUnnamed = "(未命名)"

// topTokenLimit is deliberately larger than topModelLimit. The model fold exists
// to keep a categorical palette readable; this list is bars in one colour, so
// the only ceiling is vertical space. The production archive holds 7 distinct
// token names across every day of history, so in practice nothing folds at all -
// the limit is a guard against a deployment that mints tokens per client.
const topTokenLimit = 12

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
		rest.PromptTokens += s.PromptTokens
		rest.CompletionTokens += s.CompletionTokens
		rest.Err += s.Err
		rest.OK += s.OK
		rest.LatSumMS += s.LatSumMS
		rest.LatCount += s.LatCount
		rest.FRTSumMS += s.FRTSumMS
		rest.FRTCount += s.FRTCount
		rest.CachedTokens += s.CachedTokens
	}
	return append(out[:limit:limit], rest)
}

// topChanLimit bounds the channel breakdown. Same rationale as topTokenLimit:
// one-colour bars, so the ceiling is vertical space, not palette size.
const topChanLimit = 10

// chanOther labels both the folded tail and calls that never reached a
// channel - a request rejected by the distributor has neither id nor host, and
// folding those into the biggest channel would manufacture its traffic.
const chanOther = "other"
const chanNone = "(无渠道)"

func foldChans(m map[string]*chanStat, limit int) []chanStat {
	out := make([]chanStat, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Requests != out[b].Requests {
			return out[a].Requests > out[b].Requests
		}
		return out[a].Name < out[b].Name
	})
	if len(out) <= limit {
		return out
	}
	rest := chanStat{Name: chanOther}
	for _, s := range out[limit:] {
		rest.Requests += s.Requests
		rest.OK += s.OK
		rest.Err += s.Err
		rest.Quota += s.Quota
		rest.Tokens += s.Tokens
	}
	return append(out[:limit:limit], rest)
}

// latencyMS parses the GIN duration string ("1.234s", "58.2ms") into whole
// milliseconds. Returns false for the empty/absent value so an unmeasured call
// is a coverage gap, never a zero averaged in.
func latencyMS(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, false
	}
	return d.Milliseconds(), true
}

// foldTokens is foldModels' twin, ranked by spend rather than by volume.
//
// The sort key is the whole reason this is not one shared function taking a
// comparator: the question this breakdown answers is "which token is costing
// me money", and on the production archive the two orderings genuinely invert
// (13 calls at 967,645 units above 150 calls at 55,053). Ties fall back to
// requests and then to the name, so the row order is stable across refreshes
// rather than reshuffling with map iteration order - a chart whose rows move
// between two identical queries reads as data changing.
//
// The unnamed row sorts by the same rule as any other. It carries zero quota,
// so it lands at the bottom on its own without a special case; it is only ever
// hoisted by having more requests than a token that also spent nothing.
func foldTokens(m map[string]*tokenStat, limit int) []tokenStat {
	out := make([]tokenStat, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Quota != out[b].Quota {
			return out[a].Quota > out[b].Quota
		}
		if out[a].Requests != out[b].Requests {
			return out[a].Requests > out[b].Requests
		}
		return out[a].Token < out[b].Token
	})
	if len(out) <= limit {
		return out
	}
	rest := tokenStat{Token: modelOther}
	for _, s := range out[limit:] {
		rest.Requests += s.Requests
		rest.Quota += s.Quota
		rest.Tokens += s.Tokens
		rest.PromptTokens += s.PromptTokens
		rest.CompletionTokens += s.CompletionTokens
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

	res := statsResult{Granularity: gran, Series: []bucket{}, Models: []modelStat{},
		TokenStats: []tokenStat{}, Channels: []chanStat{},
		Hours: make([]int, 24), DowHours: make([][]int, 7)}
	for i := range res.DowHours {
		res.DowHours[i] = make([]int, 24)
	}
	buckets := map[string]*bucket{}
	models := map[string]*modelStat{}
	tokens := map[string]*tokenStat{}
	chans := map[string]*chanStat{}

	q.eachMatch(f.toList(), func(_ string, e idxEntry) {
		// No usable timestamp: unplaceable on the axis, and matchIndex lets a
		// zero epoch satisfy every bound, so counting it would put one call in
		// every range at once. Reported separately instead.
		if e.Epoch <= 0 {
			res.UnknownTime++
			return
		}

		res.Requests++
		oc := entryOutcome(e)
		switch oc {
		case outcomeOK:
			res.OK++
		case outcomeErr:
			res.Err++
		default:
			res.Unknown++
		}

		// Shape of traffic. ToolCnt is the catalogue size (tools offered), so
		// its average over Tools calls is the depth of an average tool call.
		if e.IsStream {
			res.Streamed++
		}
		if e.HasTools {
			res.Tools++
			res.ToolDefs += e.ToolCnt
		}

		// Latency: the GIN wall-clock when the deployment logs one, plus the
		// billing line's first-response time. Each keeps its own count; both
		// can be absent on one record.
		var latMS, frtMS int64
		var hasLat, hasFRT bool
		if ms, ok := latencyMS(e.Latency); ok {
			latMS, hasLat = ms, true
			res.LatSumMS += ms
			res.LatCount++
			if ms > res.LatMaxMS {
				res.LatMaxMS = ms
			}
		}
		if e.FRTms != nil {
			frtMS, hasFRT = int64(*e.FRTms), true
			res.FRTSumMS += frtMS
			res.FRTCount++
			if frtMS > res.FRTMaxMS {
				res.FRTMaxMS = frtMS
			}
		}

		var q64, tok, pt, ct, cached, reasoning int64
		if e.Quota != nil {
			// Accumulate in the raw integer unit and divide once, at the edge.
			// Converting per record would round 600k times.
			q64 = *e.Quota
			res.Quota += q64
			res.QuotaCount++
		}
		if e.PT != nil || e.CT != nil {
			if e.PT != nil {
				pt = int64(*e.PT)
				res.PromptTokens += pt
			}
			if e.CT != nil {
				ct = int64(*e.CT)
				res.CompletionTokens += ct
			}
			tok = pt + ct
			res.Tokens += tok
			res.TokenCount++
		}
		if e.Cached != nil {
			cached = int64(*e.Cached)
			res.CachedTokens += cached
			res.CachedCount++
		}
		if e.Reasoning != nil {
			reasoning = int64(*e.Reasoning)
			res.ReasoningTokens += reasoning
			res.ReasoningCount++
		}

		// Time-of-day shape. Both the hour histogram and the weekday-x-hour
		// heatmap read the wall-clock string, not Epoch arithmetic - the same
		// reason bucketKey does: the day boundary is the log's local clock.
		if len(e.TS) >= 13 {
			if h, err := strconv.Atoi(e.TS[11:13]); err == nil && h < 24 {
				res.Hours[h]++
				if t, err := time.ParseInLocation("20060102", dayOf(e.TS), loc); err == nil {
					// Monday-first, matching weekStart.
					dow := (int(t.Weekday()) + 6) % 7
					res.DowHours[dow][h]++
				}
			}
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
		b.PromptTokens += pt
		b.CompletionTokens += ct
		b.CachedTokens += cached
		b.ReasoningTokens += reasoning
		if hasLat {
			b.LatSumMS += latMS
			b.LatCount++
		}
		if hasFRT {
			b.FRTSumMS += frtMS
			b.FRTCount++
		}
		switch oc {
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
		ms.PromptTokens += pt
		ms.CompletionTokens += ct
		ms.CachedTokens += cached
		if oc == outcomeErr {
			ms.Err++
		} else if oc == outcomeOK {
			ms.OK++
		}
		if hasLat {
			ms.LatSumMS += latMS
			ms.LatCount++
		}
		if hasFRT {
			ms.FRTSumMS += frtMS
			ms.FRTCount++
		}

		// Channel label: resolved name, else the recorded upstream host, else
		// the bare id. Resolved here, per query, so a renamed channel
		// re-labels its history - the index stores only the id.
		ch := ""
		if e.Chan != nil {
			if q.ch != nil {
				ch = q.ch.name(*e.Chan)
			}
			if ch == "" {
				ch = "#" + strconv.Itoa(*e.Chan)
			}
		}
		if ch == "" {
			ch = e.Up
		}
		if ch == "" {
			ch = chanNone
		}
		cs := chans[ch]
		if cs == nil {
			cs = &chanStat{Name: ch}
			chans[ch] = cs
		}
		cs.Requests++
		cs.Quota += q64
		cs.Tokens += tok
		switch oc {
		case outcomeOK:
			cs.OK++
		case outcomeErr:
			cs.Err++
		}

		// Per-token spend. The name is counted before it is defaulted, so
		// TokenNameCount measures index coverage rather than the label the row
		// happens to render under.
		tn := e.TN
		if tn != "" {
			res.TokenNameCount++
		} else {
			tn = tokenUnnamed
		}
		ts := tokens[tn]
		if ts == nil {
			ts = &tokenStat{Token: tn}
			tokens[tn] = ts
		}
		ts.Requests++
		ts.Quota += q64
		ts.Tokens += tok
		ts.PromptTokens += pt
		ts.CompletionTokens += ct
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
	res.TokenStats = foldTokens(tokens, topTokenLimit)
	res.Channels = foldChans(chans, topChanLimit)
	return res
}
