package main

import (
	"compress/gzip"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The receiving half of push mode (see push.go for why it exists).
//
// This pod is the only writer of the merged archive: it folds its own log as
// before and appends what the other pods send it to the same arc-DAY pair. The
// records arrive already folded, so nothing here parses a log line - the
// endpoint decodes the batch and hands each record to the one archive path that
// is idempotent on request id.
//
// That idempotency is the whole contract with the sender. A push that lands and
// then loses its ACK - a proxy timeout, a killed sender - is re-sent, and a
// re-sent record must not appear twice: not as a second index entry, not as a
// second row, and above all not as a second contribution to a spend total. See
// archive.AppendNew.
//
// The endpoint sits outside the interactive auth gate on purpose. The caller is
// another pod's viewer, not a browser, and AUTH_MODE=bearer validates New API
// access tokens, which are per-user credentials this process has no way to hold.
// It authenticates with PUSH_TOKEN instead, and is disabled outright when that
// is unset: an unauthenticated write into the permanent store is worse than a
// pod that cannot push.

// receiveStats counts what the endpoint has done, for /healthz. The receiving
// pod is the only writer of the merged archive, so "are the other pod's records
// actually landing" has to be answerable without opening the archive - and
// during the migration it is the first question anyone asks.
type receiveStats struct {
	mu        sync.Mutex
	stored    int64
	duplicate int64
	rejected  int64
	last      time.Time
	pods      map[string]int64
}

func (rs *receiveStats) add(pod string, stored, duplicate int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.stored += int64(stored)
	rs.duplicate += int64(duplicate)
	rs.last = time.Now()
	if rs.pods == nil {
		rs.pods = map[string]int64{}
	}
	rs.pods[pod] += int64(stored)
}

func (rs *receiveStats) reject() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.rejected++
}

// report adds the receiver's counters to the health payload. Called only when
// the receiver is enabled, so a single-machine /healthz is byte-identical to
// what it was.
func (rs *receiveStats) report(h map[string]any) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	h["push_received"] = rs.stored
	h["push_duplicate"] = rs.duplicate
	h["push_rejected"] = rs.rejected
	if !rs.last.IsZero() {
		h["push_last_receive"] = rs.last.Format(time.RFC3339)
	}
	if len(rs.pods) > 0 {
		pods := make(map[string]int64, len(rs.pods))
		for k, v := range rs.pods {
			pods[k] = v
		}
		h["push_pods"] = pods
	}
}

// handlePush archives a batch pushed by another pod.
//
// The reply is the ACK the sender waits for, and it reports what happened to
// every record: stored + duplicate must cover the batch, or the sender treats
// the push as failed and retries it rather than consuming its spool. A failure
// part-way through therefore returns an error even though earlier records in
// the batch are already durable - the retry re-sends them and they come back as
// duplicates, which is exactly the case AppendNew exists for.
func (s *server) handlePush(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		s.pushError(w, 405, "push requires POST")
		return
	}
	if s.cfg.PushToken == "" {
		// Not 404: a misconfigured primary and a wrong URL are the two things
		// an operator has to tell apart when a sender stalls, and they should
		// not look the same from the sending end.
		s.pushError(w, 503, "push receiver disabled: PUSH_TOKEN is not set")
		return
	}
	if !tokenEqual(r.Header.Get(pushTokenHeader), s.cfg.PushToken) {
		s.pushError(w, 401, "invalid "+pushTokenHeader)
		return
	}
	pod := strings.TrimSpace(r.Header.Get(pushPodHeader))
	if pod == "" {
		s.pushError(w, 400, "missing "+pushPodHeader)
		return
	}
	if !podAllowed(pod, s.cfg.PushPods) {
		s.pushError(w, 403, "pod not allowed: "+pod)
		return
	}

	body, err := pushBody(w, r, s.cfg.PushMaxBytes)
	if err != nil {
		s.pushError(w, 400, err.Error())
		return
	}

	dec := json.NewDecoder(body)
	stored, duplicate := 0, 0
	for {
		var rec Record
		if err := dec.Decode(&rec); err == io.EOF {
			break
		} else if err != nil {
			// A truncated batch is a transport failure, not a data one: report
			// what landed and let the sender re-send the whole thing.
			s.receiveStats.add(pod, stored, duplicate)
			log.Printf("push from %s: decode after %d records: %v", pod, stored+duplicate, err)
			s.pushError(w, 400, "malformed batch: "+err.Error())
			return
		}
		if rec.RequestID == "" {
			s.receiveStats.add(pod, stored, duplicate)
			s.pushError(w, 400, "record with no request_id")
			return
		}
		// Stamp provenance if the sender did not. The header is the authority
		// here - it is what the token was checked against - but a sender that
		// labels its own records keeps that label, so a record relayed on
		// behalf of a third pod still says where it came from.
		if rec.Pod == "" {
			rec.Pod = pod
		}
		ok, err := s.ing.arc.AppendNew(&rec)
		if err != nil {
			s.receiveStats.add(pod, stored, duplicate)
			log.Printf("push from %s: append %s: %v", pod, rec.RequestID, err)
			s.pushError(w, 500, "archive append failed: "+err.Error())
			return
		}
		if ok {
			stored++
		} else {
			duplicate++
		}
	}

	s.receiveStats.add(pod, stored, duplicate)
	s.writeJSON(w, 200, pushAck{
		Success: true, Stored: stored, Duplicate: duplicate, Pod: pod,
	})
}

func (s *server) pushError(w http.ResponseWriter, code int, msg string) {
	s.receiveStats.reject()
	s.writeJSON(w, code, pushAck{Success: false, Message: msg})
}

// pushBody returns the record stream, bounded twice: once on the bytes read off
// the wire, and once on what they inflate to. The second bound is the one that
// matters - a few kilobytes of crafted gzip inflate to gigabytes, and this
// endpoint decodes into memory.
func pushBody(w http.ResponseWriter, r *http.Request, maxBytes int64) (io.Reader, error) {
	var body io.Reader = r.Body
	if maxBytes > 0 {
		body = http.MaxBytesReader(w, r.Body, maxBytes)
	}
	if !strings.Contains(strings.ToLower(r.Header.Get("Content-Encoding")), "gzip") {
		// Accepted so the endpoint can be exercised with plain curl, but the
		// sender always compresses: these bodies are the same JSON that
		// compresses ~10:1 in the archive.
		return body, nil
	}
	zr, err := gzip.NewReader(body)
	if err != nil {
		return nil, err
	}
	if maxBytes > 0 {
		return io.LimitReader(zr, maxBytes*pushInflateRatio), nil
	}
	return zr, nil
}

// tokenEqual compares in constant time, over hashes rather than the strings
// themselves so that two tokens of different lengths do not resolve in
// different times either.
func tokenEqual(got, want string) bool {
	g := sha256.Sum256([]byte(strings.TrimSpace(got)))
	e := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], e[:]) == 1
}

// podAllowed checks a sender's pod name against PUSH_PODS. An empty list
// accepts any named pod: the token is the credential, and the allowlist is a
// second, optional check for deployments that want the sender pinned.
func podAllowed(pod string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == pod {
			return true
		}
	}
	return false
}
