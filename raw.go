package main

import (
	"bytes"
	"encoding/json"
)

// Raw is json.RawMessage that marshals to `null` when empty instead of
// producing invalid JSON. Request/response bodies are kept as the original
// bytes and spliced straight into the API response: no map[string]any round
// trip, which is where a naive port would spend most of its memory.
type Raw []byte

func (r Raw) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return r, nil
}

func (r *Raw) UnmarshalJSON(b []byte) error {
	*r = append((*r)[:0], b...)
	return nil
}

func (r Raw) empty() bool { return len(r) == 0 || string(r) == "null" }

// parseRaw validates a body before we keep it. new-api truncates nothing, but
// a rotated-out or half-written line must not poison the API response.
//
// A body that arrived pretty-printed - Gemini's REST API indents every
// non-streaming response, and new-api logs it as received - is stored compact.
// Its indentation means nothing and would otherwise be archived on every call.
// Valid JSON can only hold a raw newline as whitespace between tokens, so the
// newline check is exact, and it keeps single-line bodies byte-for-byte.
func parseRaw(s string) Raw {
	b := []byte(s)
	if !json.Valid(b) {
		return nil
	}
	if bytes.IndexByte(b, '\n') >= 0 {
		var c bytes.Buffer
		if json.Compact(&c, b) == nil {
			return Raw(c.Bytes())
		}
	}
	return Raw(b)
}
