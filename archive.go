package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The archive is the permanent store. New API's raw *.log is treated as a
// transient spool (it lives on tmpfs); everything that must survive is folded
// into a record here and written to disk.
//
// Why fold at all: measured on production, 76% of the log bytes are streaming
// chunk lines, and each one repeats the same id/model/created/object envelope
// around a couple of characters of actual text:
//
//	... | stream scanner data: data: {"id":"chatcmpl-313b...","choices":[{"index":0,
//	"delta":{"content":"第一百","role":"assistant"},...}],"created":1786257871,
//	"model":"deepseek-ai/...","service_tier":null,"system_fingerprint":null,...}
//
// A further 18% of bytes are blank keepalive lines carrying nothing at all.
// Concatenating the deltas and keeping one copy of the envelope is lossless for
// everything the viewer shows, and takes 161MB of real log down to 19MB of
// JSONL - 5.3MB once gzipped.
//
// Layout, one directory, one file pair per UTC day:
//
//	arc-20260809.jsonl.gz   concatenated gzip members, one per record
//	arc-20260809.idx        one JSON line per record: list metadata + offset/length
//
// Two properties matter and drive the format:
//
//   - Appendable. gzip members concatenate: a reader may stop after any member,
//     and `gzip -dc` on the whole file still yields every record. So a record is
//     compressed on its own and appended, with no rewrite of what came before.
//   - Randomly accessible. The .idx carries the byte offset of each member, so
//     opening one record seeks and inflates that record alone rather than the
//     day. Per-record framing costs ~16% versus compressing the day as one
//     stream (6.10MB vs 5.26MB measured) - cheap for O(1) detail reads.
//
// The .idx is what the list view reads. It is small (a few hundred bytes per
// call), so a day of it can be parsed on demand without keeping bodies resident.
type archive struct {
	dir string

	mu   sync.Mutex
	day  string // yyyymmdd of the currently open pair
	data *os.File
	idx  *os.File
	off  int64 // bytes written to data, i.e. offset of the next member
}

// idxEntry is the list-row projection, persisted alongside the offset needed to
// fetch the full record. Field names are short because this file is written once
// per call and read in full for every list query.
type idxEntry struct {
	RID    string `json:"i"`
	TS     string `json:"t"`
	Epoch  int64  `json:"e"`
	Off    int64  `json:"o"`
	Len    int64  `json:"n"`
	Model  string `json:"m,omitempty"`
	Status *int   `json:"s,omitempty"`

	// Whether the call succeeded. Distinct from Status, which is only ever set
	// from the GIN line and is absent entirely on deployments where gin logs to
	// another sink - which is every record on this one. The list column reads
	// this; without it in the index the column is blank however good the record
	// behind it is.
	Outcome string `json:"oc,omitempty"`

	Latency  string `json:"l,omitempty"`
	IsStream bool   `json:"st,omitempty"`
	HasTools bool   `json:"ht,omitempty"`
	Quota    *int64 `json:"q,omitempty"`
	Preview  string `json:"p,omitempty"`
	Errors   int    `json:"er,omitempty"`
	MsgCount int    `json:"mc,omitempty"`
	Turns    int    `json:"tn,omitempty"`
	ToolCnt  int    `json:"tc,omitempty"`

	// Which channel served the call. The id is what the log carries; the host
	// is derived from fullRequestURL and is the fallback label when no name can
	// be resolved. Only the host is kept - the full URL is in the record.
	Chan *int   `json:"c,omitempty"`
	Up   string `json:"u,omitempty"`

	// Which API token (令牌) the caller presented, so spend can be attributed
	// without opening a body.
	//
	// A plain string rather than a pointer, unlike PT/CT and Quota. Those need
	// to distinguish "reported zero" from "never reported"; a token name has no
	// zero value to confuse it with, and the empty case is not ambiguous either:
	// the name and the quota arrive on the same billing line, so a call missing
	// one is missing both. Measured across 13,955 archived records - 785 with no
	// token name, and every single one of them with a nil Quota, no exceptions.
	// So an unnamed call contributes exactly zero to a spend breakdown and needs
	// no separate "unbilled but billed" concept.
	//
	// What the empty string cannot distinguish is an index written before this
	// field existed. That is what statsResult.TokenNameCount is for.
	TN string `json:"tk,omitempty"`

	// Token counts, carried so the stats view can total them without opening a
	// single body: the day's records are ~340MB of gzip against ~700KB of index.
	//
	// Pointers, not ints, for the same reason Outcome is not a synthesised
	// status: a call whose usage never reached the log must not be
	// indistinguishable from one that genuinely used zero tokens. Averaging over
	// the former silently understates; the nil lets the stats page report
	// coverage instead of guessing.
	PT *int `json:"pt,omitempty"`
	CT *int `json:"ct,omitempty"`
}

// makeIdxEntry is the single definition of the list-row projection. reindex
// calls it too, so a rebuilt index is byte-identical to one Append would have
// written.
func makeIdxEntry(r *Record, off, n int64) idxEntry {
	e := idxEntry{
		RID: r.RequestID, TS: r.TS, Epoch: r.Epoch, Off: off, Len: n,
		Model: r.Model, Status: r.Status, Latency: r.Latency,
		Outcome:  r.Outcome,
		IsStream: r.IsStream, HasTools: r.HasTools, Quota: r.Quota,
		Preview: r.Preview, Errors: len(r.Errors),
		MsgCount: r.MsgCount, Turns: r.Turns, ToolCnt: r.ToolCount,
		Chan: r.ChannelID, Up: hostOf(r.UpstreamURL),
		TN: r.TokenName,
	}
	// Both levels are optional: Usage is absent on calls that never reported it
	// (4% of a recent day), and either count can be missing within it. Copy the
	// values rather than aliasing the record's pointers, so a later mutation of
	// the record cannot reach into an already-written index entry.
	if r.Usage != nil {
		if p := r.Usage.PromptTokens; p != nil {
			v := *p
			e.PT = &v
		}
		if c := r.Usage.CompletionTokens; c != nil {
			v := *c
			e.CT = &v
		}
	}
	return e
}

func newArchive(dir string) *archive { return &archive{dir: dir} }

func dayOf(ts string) string {
	// TS is "2006/01/02 15:04:05"; fall back to the current day if absent so a
	// record is never silently dropped for want of a timestamp.
	if len(ts) >= 10 {
		return ts[0:4] + ts[5:7] + ts[8:10]
	}
	return time.Now().Format("20060102")
}

func (a *archive) paths(day string) (string, string) {
	return filepath.Join(a.dir, "arc-"+day+".jsonl.gz"),
		filepath.Join(a.dir, "arc-"+day+".idx")
}

// open switches the open file pair to day, creating it if needed.
// Caller holds a.mu.
func (a *archive) openDay(day string) error {
	if a.day == day && a.data != nil {
		return nil
	}
	a.closeLocked()
	if err := os.MkdirAll(a.dir, 0o755); err != nil {
		return err
	}
	dp, ip := a.paths(day)
	df, err := os.OpenFile(dp, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	inf, err := os.OpenFile(ip, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		df.Close()
		return err
	}
	st, err := df.Stat()
	if err != nil {
		df.Close()
		inf.Close()
		return err
	}
	a.day, a.data, a.idx, a.off = day, df, inf, st.Size()
	return nil
}

func (a *archive) closeLocked() {
	if a.data != nil {
		a.data.Close()
		a.data = nil
	}
	if a.idx != nil {
		a.idx.Close()
		a.idx = nil
	}
	a.day = ""
}

func (a *archive) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closeLocked()
}

// Append writes one finished record. It is the only writer path, and it is
// crash-conscious: the data member is flushed before the index entry that
// points at it, so a torn write can leave an unreferenced member (harmless)
// but never an index entry pointing at bytes that were never written.
func (a *archive) Append(r *Record) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if _, err := zw.Write(body); err != nil {
		return err
	}
	// Terminate the record with a newline INSIDE the compressed member, so the
	// decompressed day is JSONL: `gunzip -c arc-DAY.jsonl.gz | jq -c .` works,
	// and so does `wc -l`. Without it the members concatenate into one long
	// line and every line-oriented tool sees a single record.
	if _, err := zw.Write([]byte("\n")); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.openDay(dayOf(r.TS)); err != nil {
		return err
	}
	n, err := a.data.Write(buf.Bytes())
	if err != nil {
		// A short write leaves the offset wrong for every later record, so
		// resync from the file rather than trusting the counter.
		if st, serr := a.data.Stat(); serr == nil {
			a.off = st.Size()
		}
		return err
	}
	if err := a.data.Sync(); err != nil {
		return err
	}

	e := makeIdxEntry(r, a.off, int64(n))
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := a.idx.Write(append(line, '\n')); err != nil {
		return err
	}
	a.off += int64(n)
	return a.idx.Sync()
}

// ---- reading ---------------------------------------------------------------

// newMultiReader inflates a whole day file as one stream, spanning every
// concatenated member. This is the `gunzip -c` view: the archive must remain
// readable with ordinary tools, not only through this binary.
func newMultiReader(r io.Reader) (io.Reader, error) {
	return gzip.NewReader(r)
}

// days lists archived days, newest first.
func (a *archive) days() []string {
	matches, _ := filepath.Glob(filepath.Join(a.dir, "arc-*.idx"))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		b := filepath.Base(m)
		d := strings.TrimSuffix(strings.TrimPrefix(b, "arc-"), ".idx")
		if len(d) == 8 {
			out = append(out, d)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// daysInRange returns archived days overlapping [since,until] (epoch seconds,
// 0 meaning unbounded), newest first.
//
// This is what keeps a query proportional to the filter rather than to history:
// a 15-minute filter opens one day's index, not the whole archive.
func (a *archive) daysInRange(since, until int64) []string {
	all := a.days()
	if since == 0 && until == 0 {
		return all
	}
	out := make([]string, 0, len(all))
	for _, d := range all {
		t, err := time.ParseInLocation("20060102", d, time.Local)
		if err != nil {
			continue
		}
		start := t.Unix()
		end := t.AddDate(0, 0, 1).Unix() - 1
		if since > 0 && end < since {
			continue
		}
		if until > 0 && start > until {
			continue
		}
		out = append(out, d)
	}
	return out
}

// index reads one day's entries. Entries are returned in file order (oldest
// first); callers sort.
func (a *archive) index(day string) ([]idxEntry, error) {
	_, ip := a.paths(day)
	f, err := os.Open(ip)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []idxEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for sc.Scan() {
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		var e idxEntry
		if json.Unmarshal(b, &e) != nil {
			// A half-written final line is expected after a crash; the record
			// it describes is simply invisible until re-ingested.
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// fetch inflates a single record identified by its index entry.
func (a *archive) fetch(day string, e idxEntry) (*Record, error) {
	dp, _ := a.paths(day)
	f, err := os.Open(dp)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Bound the read to this member. Concatenated gzip members form one valid
	// stream, so an unbounded reader would run on into the next record and
	// decode the wrong body.
	zr, err := gzip.NewReader(io.NewSectionReader(f, e.Off, e.Len))
	if err != nil {
		return nil, err
	}
	zr.Multistream(false)
	defer zr.Close()
	var rec Record
	if err := json.NewDecoder(zr).Decode(&rec); err != nil {
		return nil, err
	}
	return &rec, nil
}
