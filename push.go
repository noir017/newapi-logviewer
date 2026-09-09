package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Push mode: ship folded records to another pod's viewer instead of archiving
// them locally.
//
// Why it exists: one New API cluster, two pods, one database. Each pod writes
// its own DEBUG log, so each viewer folds only the calls its own pod served -
// and a list, a search or a spend total computed from half the traffic is not a
// smaller answer, it is a wrong one. There is no shared filesystem to point
// both viewers at, and syncing two archives after the fact would mean two
// writers appending to the same day file.
//
// So one pod is made the only writer. The secondary pod's viewer folds its log
// exactly as before and pushes the records over HTTP; the primary appends them
// to the same arc-DAY pair it writes its own records into (see receive.go). The
// archive format, the per-day pooling and the read path are untouched - a
// pushed record is indistinguishable from a locally folded one once it lands.
//
// Three properties make that safe to run over a network:
//
//   - The spool stays the buffer. A record is forgotten only once the receiver
//     has acknowledged it, and while a push is failing the ingester stops
//     consuming the spool entirely. An outage therefore costs spool space, not
//     records: on restart the unconsumed log is re-read from the start and the
//     records are rebuilt from it.
//   - Retries are safe. The receiver is idempotent on request id, so a push
//     that lands and then loses its ACK is re-sent and skipped rather than
//     double counted. That is the one guarantee this design cannot do without:
//     an at-least-once transport plus a deduplicating writer is what makes
//     "never lose a record" and "never count one twice" hold at the same time.
//   - gzip on the wire. Bodies are the same JSON that compresses ~10:1 in the
//     archive, and the link between these two pods is the internet.

const (
	// pushPath is the receiver's route, under BASE_PATH.
	pushPath = "/api/push"

	pushContentType = "application/x-ndjson"
	pushPodHeader   = "X-Pod-Name"
	pushTokenHeader = "X-Push-Token"
)

const (
	// One push carries at most this many records, or this many bytes of
	// uncompressed body, whichever comes first. The byte cap is the one that
	// usually binds - a single agent record can be megabytes - and the count cap
	// only matters for a burst of small ones. Whatever does not fit stays
	// pending and goes out on the next tick.
	pushMaxRecords = 64
	pushMaxBytes   = 8 << 20

	// Backoff over an unreachable receiver, doubling from min to max. The
	// minimum is short because the common failure is a redeploy of the primary
	// pod, which is back in seconds; the maximum is what a real outage settles
	// at, and it is bounded because the sender must notice recovery on its own.
	pushBackoffMin = 2 * time.Second
	pushBackoffMax = 5 * time.Minute

	// pushInflateRatio bounds what a pushed body may inflate to, as a multiple
	// of PUSH_MAX_MB. The records themselves compress around 10:1, so this
	// leaves headroom for a legitimate batch while still refusing a body
	// engineered to inflate without limit.
	pushInflateRatio = 20
)

// pusher is the sending half. It holds no lock of its own: every method is
// called from the ingester with i.mu held, which is also what serialises the
// backoff state against the poll loop that reads it for /healthz.
type pusher struct {
	url    string
	pod    string
	token  string
	client *http.Client

	// Backoff state. fails is the consecutive failure count (0 when the last
	// attempt succeeded), nextTry the instant the next attempt is allowed.
	fails   int
	nextTry time.Time
	lastErr string
	sent    int64
}

// newPusher returns nil when PUSH_URL is unset, which is what keeps a
// single-machine install on exactly the code path it had before push mode
// existed: the ingester's push branch is never taken.
func newPusher(cfg Config) *pusher {
	if cfg.PushURL == "" {
		return nil
	}
	return &pusher{
		url:    cfg.PushURL,
		pod:    cfg.PodName,
		token:  cfg.PushToken,
		client: &http.Client{Timeout: cfg.PushTimeout},
	}
}

// pushEndpoint normalizes PUSH_URL into the receiver's full endpoint URL, so
// that both the viewer's mount root ("https://host/logviewer") and the endpoint
// itself ("https://host/logviewer/api/push") configure the same thing. Both
// spellings are equally natural to write and getting it wrong is a silent 404
// loop that only shows up as a stalled sender.
func pushEndpoint(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" || strings.HasSuffix(raw, pushPath) {
		return raw
	}
	return raw + pushPath
}

// ready reports whether an attempt is allowed now. A failing receiver is
// retried on a doubling delay rather than on every ingest tick.
func (p *pusher) ready(now time.Time) bool { return !now.Before(p.nextTry) }

// failing reports whether the last attempt failed - i.e. whether the ingester
// should stop consuming its spool.
func (p *pusher) failing() bool { return p.fails > 0 }

// succeeded and failed record the outcome of one attempt.
func (p *pusher) succeeded(n int) {
	p.sent += int64(n)
	p.fails, p.lastErr, p.nextTry = 0, "", time.Time{}
}

func (p *pusher) failed(now time.Time, err error) {
	p.fails++
	p.lastErr = err.Error()
	// Shift capped well below the width of a Duration: 2s << 8 is already past
	// the maximum, so anything further would only risk overflowing.
	d := pushBackoffMin << min(p.fails-1, 8)
	if d > pushBackoffMax {
		d = pushBackoffMax
	}
	p.nextTry = now.Add(d)
}

// send delivers one batch and returns nil only when the receiver has accepted
// every record in it. Any other outcome is an error, and an error means the
// caller keeps the records: the transport is at-least-once by design, and the
// receiver's request-id dedup is what makes that safe.
func (p *pusher) send(recs []*Record) error {
	body, err := encodePush(recs)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", p.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", pushContentType)
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set(pushPodHeader, p.pod)
	if p.token != "" {
		req.Header.Set(pushTokenHeader, p.token)
	}
	res, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	// The ACK is a few dozen bytes; anything longer is not our receiver and the
	// prefix is enough to say so in the log.
	ack, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("receiver returned %s: %s", res.Status,
			strings.TrimSpace(string(ack)))
	}

	// A 2xx alone is not an ACK. Between these two pods sit a reverse proxy and
	// the public internet, and both can answer 200 with something that is not
	// this endpoint - an auth portal, a cached page, a proxy's own error body.
	// Consuming the spool on one of those would lose the batch silently, so the
	// receiver's own accounting has to add up to what was sent.
	var got pushAck
	if json.Unmarshal(ack, &got) != nil || !got.Success {
		return fmt.Errorf("receiver did not acknowledge (%s): %s", res.Status,
			strings.TrimSpace(string(ack)))
	}
	if n := got.Stored + got.Duplicate; n != len(recs) {
		return fmt.Errorf("receiver accounted for %d of %d records", n, len(recs))
	}
	return nil
}

// pushAck is the receiver's reply: how many records it archived, and how many
// it recognised as already archived. A duplicate is a success - it is what a
// retried batch looks like from the receiving end.
type pushAck struct {
	Success   bool   `json:"success"`
	Stored    int    `json:"stored"`
	Duplicate int    `json:"duplicate"`
	Pod       string `json:"pod,omitempty"`
	Message   string `json:"message,omitempty"`
}

// encodePush frames a batch as gzipped NDJSON: one record's archive JSON per
// line, the same bytes the archive would have stored. NDJSON rather than a JSON
// array so the receiver can decode and append record by record, holding one at
// a time instead of the whole batch.
func encodePush(recs []*Record) ([]byte, error) {
	var buf bytes.Buffer
	// Default compression, not BestSpeed as the archive uses: there the tradeoff
	// is against an ingest tick's CPU on the same machine, here it is against
	// bytes on a metered WAN link.
	zw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(zw)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// recordSize is the batching estimate, not an exact encoded length: the point
// is to cap a request at a few megabytes of bodies, and computing that exactly
// would mean marshalling every pending record twice.
func recordSize(r *Record) int64 {
	return int64(len(r.Request) + len(r.Response) + len(r.Billing) +
		len(r.StreamContent) + len(r.StreamReasoning) + 512)
}
