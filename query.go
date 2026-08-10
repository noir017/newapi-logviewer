package main

import (
	"sort"
	"strings"
	"time"
)

// query answers list/detail/search from the archive, holding nothing between
// requests.
//
// This replaces a store that kept every parsed Record - plus a lowercased copy
// of every request and response body for search - resident for the life of the
// process. That cost 196MB RSS and grew with the log directory, to buy a search
// latency nobody needed: this viewer is opened a few times a day.
//
// The archive is instead read per request. What makes that affordable is the
// day-partitioned index: a list query parses a few hundred bytes per call from
// arc-YYYYMMDD.idx and never touches a body, and a detail view inflates exactly
// one gzip member.
type query struct {
	arc *archive
	// searchDays caps how far back a full-text search will read bodies. Listing
	// and filtering stay unbounded - those only read the index.
	searchDays int
	// ch resolves channel ids to names. Optional and nil-safe: without it the
	// list falls back to the upstream host recorded in the index.
	ch *channelResolver
}

func newQuery(arc *archive, searchDays int) *query {
	return &query{arc: arc, searchDays: searchDays}
}

func (q *query) withChannels(c *channelResolver) *query {
	q.ch = c
	return q
}

// listFilter is the subset of the UI's filters that the index alone can answer.
type listFilter struct {
	Model      string
	Status     string // "ok" | "err" | ""
	Stream     string // "1" | "0" | ""
	Tools      string // "1" | ""
	ToolName   string
	Search     string
	ErrorsOnly bool
	Since      int64
	Until      int64
}

// dayEntry pairs an index entry with the day file it came from, which is needed
// to fetch the body later.
type dayEntry struct {
	day string
	e   idxEntry
}

// candidates walks the days overlapping the time filter, newest first, and
// returns the entries matching every index-answerable predicate.
//
// A request id may appear more than once: the archive is append-only, so
// correcting a record (after a parser fix, say) means appending a new version.
// The last entry for an id wins, and the earlier ones are not listed.
func (q *query) candidates(f listFilter) []dayEntry {
	var out []dayEntry
	for _, day := range q.arc.daysInRange(f.Since, f.Until) {
		entries, err := q.arc.index(day)
		if err != nil {
			continue
		}
		latest := make(map[string]int, len(entries))
		for i, e := range entries {
			latest[e.RID] = i
		}
		for i, e := range entries {
			if latest[e.RID] != i {
				continue // superseded by a later append
			}
			if !matchIndex(e, f) {
				continue
			}
			out = append(out, dayEntry{day, e})
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].e.TS != out[b].e.TS {
			return out[a].e.TS > out[b].e.TS
		}
		return out[a].e.RID > out[b].e.RID
	})
	return out
}

func matchIndex(e idxEntry, f listFilter) bool {
	if f.Model != "" && e.Model != f.Model {
		return false
	}
	if f.Status == "ok" && (e.Status == nil || *e.Status != 200) {
		return false
	}
	if f.Status == "err" && (e.Status == nil || *e.Status == 200) {
		return false
	}
	if f.Stream == "1" && !e.IsStream {
		return false
	}
	if f.Stream == "0" && e.IsStream {
		return false
	}
	if f.Tools == "1" && !e.HasTools {
		return false
	}
	if f.ErrorsOnly && e.Errors == 0 {
		return false
	}
	if f.Since > 0 && e.Epoch > 0 && e.Epoch < f.Since {
		return false
	}
	if f.Until > 0 && e.Epoch > 0 && e.Epoch > f.Until {
		return false
	}
	return true
}

// searchWindow clamps a full-text search to the configured number of days,
// intersected with whatever range the UI already asked for.
//
// The point is that a 15-minute filter should read 15 minutes of bodies, not a
// week of them. The cap only applies when the UI supplied no lower bound.
func (q *query) searchWindow(f listFilter) listFilter {
	if q.searchDays <= 0 {
		return f
	}
	floor := time.Now().AddDate(0, 0, -q.searchDays).Unix()
	if f.Since == 0 || f.Since < floor {
		f.Since = floor
	}
	return f
}

// list returns one page of rows plus the total match count.
//
// Free-text search and tool-name filtering cannot be answered from the index -
// they need the body - so those are applied by inflating candidates in
// newest-first order. Everything else is index-only.
func (q *query) list(f listFilter, page, size int) ([]listItem, int, []string, []string) {
	needsBody := f.Search != "" || f.ToolName != ""
	if needsBody {
		f = q.searchWindow(f)
	}
	cands := q.candidates(f)

	models := map[string]bool{}
	tools := map[string]bool{}

	var matched []dayEntry
	if !needsBody {
		matched = cands
		for _, c := range cands {
			if c.e.Model != "" {
				models[c.e.Model] = true
			}
		}
	} else {
		needle := strings.ToLower(f.Search)
		for _, c := range cands {
			rec, err := q.arc.fetch(c.day, c.e)
			if err != nil {
				continue
			}
			if f.ToolName != "" && !contains(rec.ToolNames, f.ToolName) {
				continue
			}
			if needle != "" && !recordMatches(rec, needle) {
				continue
			}
			if rec.Model != "" {
				models[rec.Model] = true
			}
			for _, t := range rec.ToolNames {
				tools[t] = true
			}
			matched = append(matched, c)
		}
	}

	total := len(matched)
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}

	items := make([]listItem, 0, end-start)
	for _, c := range matched[start:end] {
		items = append(items, q.entryToItem(c.e))
	}
	return items, total, sortedKeys(models), sortedKeys(tools)
}

// recordMatches is the on-demand equivalent of the old resident searchBlob.
// Building the haystack per candidate costs allocation, but only for records a
// search actually visits, and only within the search window.
func recordMatches(r *Record, needle string) bool {
	if strings.Contains(strings.ToLower(r.Preview), needle) ||
		strings.Contains(strings.ToLower(r.RequestID), needle) ||
		strings.Contains(strings.ToLower(r.StreamContent), needle) {
		return true
	}
	return containsFold(r.Request, needle) || containsFold(r.Response, needle)
}

func containsFold(raw Raw, needle string) bool {
	if len(raw) == 0 {
		return false
	}
	return strings.Contains(strings.ToLower(string(raw)), needle)
}

func (q *query) entryToItem(e idxEntry) listItem {
	it := listItem{
		RequestID: e.RID, TS: e.TS, Epoch: e.Epoch,
		Model: e.Model, Status: e.Status, Latency: e.Latency,
		IsStream: e.IsStream, HasTools: e.HasTools, Quota: e.Quota,
		Preview: e.Preview, Errors: e.Errors,
		MsgCount: e.MsgCount, Turns: e.Turns, ToolCount: e.ToolCnt,
		ChannelID: e.Chan, Upstream: e.Up,
	}
	// Resolution happens here rather than at ingest so a renamed or newly
	// labelled channel is reflected on records already archived.
	if e.Chan != nil {
		it.Channel = q.ch.name(*e.Chan)
	}
	return it
}

// get fetches one full record by request id.
//
// The id embeds its date (New API ids start yyyymmdd), so the day holding it is
// usually the first one tried; the scan over remaining days is the fallback for
// ids that do not follow that shape.
func (q *query) get(rid string) *Record {
	days := q.arc.days()
	if len(rid) >= 8 {
		if d := rid[:8]; d >= "20200101" && d <= "20991231" {
			days = append([]string{d}, days...)
		}
	}
	seen := map[string]bool{}
	for _, day := range days {
		if seen[day] {
			continue
		}
		seen[day] = true
		entries, err := q.arc.index(day)
		if err != nil {
			continue
		}
		// Last write wins. The archive is append-only, so a record can only be
		// corrected by appending a better version of it - which is how a parser
		// fix is applied to history without rewriting (or risking) what is
		// already on disk.
		for i := len(entries) - 1; i >= 0; i-- {
			if entries[i].RID != rid {
				continue
			}
			if rec, err := q.arc.fetch(day, entries[i]); err == nil {
				return rec
			}
		}
	}
	return nil
}
