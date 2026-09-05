package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
)

// Archive format v2: request-body deduplication.
//
// A coding agent resends its entire transcript on every turn, so the same
// message is otherwise archived once per turn - the largest record measured was
// 1.88MB, 99.6% of it the `request`, and 1.72MB of that the message history.
// gzip cannot collapse the copies: the repeats sit megabytes apart, far outside
// its 32KB window, so every turn is compressed from scratch. That is what turns
// a day of coding traffic into gigabytes.
//
// v2 lifts the three parts of a request that repeat - the conversation
// (messages[]), the tool catalogue (tools[]) and the system prompt (system) -
// out of each record and into a per-day content-addressed pool, replacing each
// with a pointer. A message shared across a hundred turns is then stored once.
//
// The pool mirrors the record file's two properties (see archive.go):
//
//   - Append-only. A blob, once written, is never moved, so the {off,len}
//     pointer embedded in a record stays valid for the life of the day file.
//     Reads need only the pointer: seek into blob.gz, inflate that member, done.
//   - Self-describing for restart. arc-DAY.blob.idx maps content hash -> offset.
//     It is read once when a day is reopened, to rebuild the in-memory dedup map
//     so an interrupted run keeps deduping instead of re-storing every blob. It
//     is never consulted on the read path; a torn or missing line costs at most
//     one re-stored duplicate, never a wrong read.
//
// Records carry V=archiveVersion. A v1 record has no pointers and is read
// exactly as before, so the 15GB of existing history stays readable untouched.
const archiveVersion = 2

// minLift is the smallest value worth lifting. A pointer plus its pool member
// costs ~60 bytes; lifting anything smaller would grow the request, not shrink
// it. Whole conversations and tool schemas are kilobytes, so this only skips
// trivia (an empty system string, a one-line message).
const minLift = 64

// blobRef is how a lifted value appears inside a stored request: a pointer into
// the day's blob.gz. The keys are `$b`/`$n` - the `$` marks it as viewer
// metadata a real request key never uses, and two letters keep the pointer
// smaller than the smallest thing worth lifting.
type blobRef struct {
	Off int64 `json:"$b"`
	Len int64 `json:"$n"`
}

// asBlobRef reports whether a stored value is one of our pointers. Detection is
// positional - only the slots v2 rewrites (system, each element of messages and
// tools) are ever inspected - but we still confirm the shape so a genuine
// request value that happens to be an object cannot be mistaken for a pointer.
func asBlobRef(b []byte) (blobRef, bool) {
	if len(b) < 2 || b[0] != '{' {
		return blobRef{}, false
	}
	var r blobRef
	if err := json.Unmarshal(b, &r); err != nil {
		return blobRef{}, false
	}
	// A real pointer always spans bytes; the empty value is never lifted. This
	// is what separates a pointer from an arbitrary object lacking these keys
	// (both decode to Len==0).
	if r.Len <= 0 {
		return blobRef{}, false
	}
	return r, true
}

func blobHash(content []byte) string {
	sum := sha256.Sum256(content)
	// 16 bytes is 2^-64 collision odds across a day's blobs - far below the odds
	// of a disk returning the wrong bytes - and halves the idx against full-width.
	return hex.EncodeToString(sum[:16])
}

// splitRequest rewrites a request for storage, replacing each liftable value
// with a pointer returned by put. put persists the content (deduplicating) and
// hands back the pointer to write in its place. A request that is not a JSON
// object, or carries none of the three keys, comes back unchanged.
func splitRequest(req Raw, put func(content []byte) (blobRef, error)) (Raw, error) {
	if req.empty() {
		return req, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(req, &top); err != nil {
		return req, nil // not an object: nothing to lift, store verbatim
	}

	lift := func(v json.RawMessage) (json.RawMessage, bool, error) {
		if len(v) < minLift {
			return v, false, nil
		}
		ref, err := put(v)
		if err != nil {
			return nil, false, err
		}
		packed, err := json.Marshal(ref)
		if err != nil {
			return nil, false, err
		}
		return packed, true, nil
	}

	changed := false
	if v, ok := top["system"]; ok {
		nv, did, err := lift(v)
		if err != nil {
			return nil, err
		}
		if did {
			top["system"] = nv
			changed = true
		}
	}
	for _, key := range []string{"messages", "tools"} {
		raw, ok := top[key]
		if !ok {
			continue
		}
		var arr []json.RawMessage
		if json.Unmarshal(raw, &arr) != nil {
			continue // not an array: leave the key as-is
		}
		any := false
		for i := range arr {
			nv, did, err := lift(arr[i])
			if err != nil {
				return nil, err
			}
			if did {
				arr[i] = nv
				any = true
			}
		}
		if any {
			packed, err := json.Marshal(arr)
			if err != nil {
				return nil, err
			}
			top[key] = packed
			changed = true
		}
	}
	if !changed {
		return req, nil
	}
	return json.Marshal(top)
}

// joinRequest is the inverse: it reinlines every pointer by calling get, so the
// caller sees the original request bytes. Only the three lifted slots are
// inspected; anything else is returned untouched. A request with no pointers
// (v1, or a v2 call that carried none) passes straight through.
func joinRequest(req Raw, get func(ref blobRef) ([]byte, error)) (Raw, error) {
	if req.empty() {
		return req, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(req, &top); err != nil {
		return req, nil
	}

	inline := func(v json.RawMessage) (json.RawMessage, bool, error) {
		ref, ok := asBlobRef(v)
		if !ok {
			return v, false, nil
		}
		content, err := get(ref)
		if err != nil {
			return nil, false, err
		}
		return content, true, nil
	}

	changed := false
	if v, ok := top["system"]; ok {
		nv, did, err := inline(v)
		if err != nil {
			return nil, err
		}
		if did {
			top["system"] = nv
			changed = true
		}
	}
	for _, key := range []string{"messages", "tools"} {
		raw, ok := top[key]
		if !ok {
			continue
		}
		var arr []json.RawMessage
		if json.Unmarshal(raw, &arr) != nil {
			continue
		}
		any := false
		for i := range arr {
			nv, did, err := inline(arr[i])
			if err != nil {
				return nil, err
			}
			if did {
				arr[i] = nv
				any = true
			}
		}
		if any {
			packed, err := json.Marshal(arr)
			if err != nil {
				return nil, err
			}
			top[key] = packed
			changed = true
		}
	}
	if !changed {
		return req, nil
	}
	return json.Marshal(top)
}

// gzipBlob compresses one blob into a standalone gzip member, matching the
// record file's per-member framing so the pool is a valid concatenated stream.
func gzipBlob(content []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if _, err := zw.Write(content); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// readBlobAt inflates the single pool member a pointer names. The bound matters
// for the same reason fetch bounds a record read: concatenated members form one
// valid stream, so an unbounded reader would run on into the next blob.
func readBlobAt(ra io.ReaderAt, ref blobRef) ([]byte, error) {
	zr, err := gzip.NewReader(io.NewSectionReader(ra, ref.Off, ref.Len))
	if err != nil {
		return nil, err
	}
	zr.Multistream(false)
	defer zr.Close()
	return io.ReadAll(zr)
}

// blobIdxLine is one entry of arc-DAY.blob.idx: the content hash and the pointer
// it resolved to. Read only when a day is reopened, to rebuild the write-side
// dedup map.
type blobIdxLine struct {
	H   string `json:"h"`
	Off int64  `json:"o"`
	Len int64  `json:"n"`
}

// loadBlobMap rebuilds the hash -> pointer dedup map from a day's blob index. A
// missing file is a fresh day, not an error. A half-written final line after a
// crash is skipped: the blob it named is simply eligible to be stored again,
// which wastes space but is never wrong.
func loadBlobMap(path string) (map[string]blobRef, error) {
	m := map[string]blobRef{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for sc.Scan() {
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		var l blobIdxLine
		if json.Unmarshal(b, &l) != nil || l.Len <= 0 || l.H == "" {
			continue
		}
		m[l.H] = blobRef{Off: l.Off, Len: l.Len}
	}
	return m, sc.Err()
}
