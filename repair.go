package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

// repair fixes records the parser damaged, in place, from the archive alone.
//
// This is the third backfill path and it exists because the other two cannot do
// this job:
//
//   - -reindex rebuilds the .idx from the records, so it can only propagate a
//     value the record already holds. Here the RECORD is wrong.
//   - -reingest re-parses the raw log, which is a tmpfs spool that is gone
//     minutes after the call - and these bugs date back to the archive's start.
//
// What makes an in-place fix possible is that neither bug destroyed
// information. Both were arithmetic on data that survived intact:
//
//   - Turns/MsgCount/ToolCount were multiplied by the number of read passes.
//     They are derived from the request body, which is stored verbatim, so they
//     can simply be recomputed.
//   - Streaming tool-call fragments were appended as separate calls instead of
//     being joined by index. The fragments are all there, in arrival order, so
//     re-joining them reconstructs the original call exactly.
//
// A record is rewritten by appending the corrected version: the archive is
// append-only and readers take the last entry for an id. Nothing is deleted, so
// the pre-repair version stays fetchable at its old offset and a repair that
// goes wrong costs disk, not data.
//
//	logviewer -repair [-day 20260812] [-dry-run]
//
// Run it with the server stopped, as the uid that owns the archive.
func repairDay(dir, day string, dryRun bool) (scanned, fixed int, err error) {
	a := newArchive(dir)
	defer a.Close()

	entries, err := a.index(day)
	if err != nil {
		return 0, 0, fmt.Errorf("read index: %w", err)
	}

	// Only the newest version of each id: earlier ones are superseded, and
	// re-repairing an already-repaired record would be a no-op anyway.
	latest := map[string]int{}
	for i, e := range entries {
		latest[e.RID] = i
	}

	for i, e := range entries {
		if latest[e.RID] != i {
			continue
		}
		rec, ferr := a.fetch(day, e)
		if ferr != nil {
			// A single unreadable member must not abort the run: the rest of
			// the day is still repairable, and skipping leaves it exactly as
			// readable as it was.
			log.Printf("repair: skipping %s: %v", e.RID, ferr)
			continue
		}
		scanned++
		if !repairRecord(rec) {
			continue
		}
		fixed++
		if dryRun {
			continue
		}
		if aerr := a.Append(rec); aerr != nil {
			return scanned, fixed, fmt.Errorf("append %s: %w", e.RID, aerr)
		}
	}
	return scanned, fixed, nil
}

// repairRecord corrects one record in place and reports whether anything
// changed. Split out from repairDay so it can be tested without an archive.
func repairRecord(r *Record) bool {
	changed := false

	// 1. Re-join streaming tool-call fragments.
	//
	// Detected by shape rather than by count, because a call legitimately
	// streams as many fragments: the signature is a fragment whose arguments
	// are a slice of JSON rather than a whole value. Feeding them back through
	// the same accumulator the parser now uses keeps one definition of the
	// joining rule.
	if len(r.StreamToolCalls) > 1 && fragmented(r.StreamToolCalls) {
		var ta toolAcc
		for _, frag := range r.StreamToolCalls {
			ta.add(frag)
		}
		if joined := ta.calls(); len(joined) > 0 && len(joined) < len(r.StreamToolCalls) {
			for i := range joined {
				joined[i].Function.Arguments = trimReread(joined[i].Function.Arguments)
			}
			r.StreamToolCalls = joined
			changed = true
		}
	}

	// 2. Recompute the derived counts from the request body, which was never
	// damaged. recountFrom returns false when the body is missing (a call whose
	// request line rotated out), in which case the stored counts are the only
	// ones there will ever be and are left alone.
	if recountFrom(r) {
		changed = true
	}

	// 3. CalledTools is derived from the tool calls, so it inherited the
	// duplication. Rebuild it from whatever the calls now are.
	names := make([]string, 0, len(r.StreamToolCalls))
	src := r.StreamToolCalls
	if !r.IsStream {
		// Non-streaming calls take theirs from the response body; leave those.
		src = nil
	}
	for _, tc := range src {
		if n := tc.Function.Name; n != "" && !contains(names, n) {
			names = append(names, n)
		}
	}
	if src != nil && !sameStrings(r.CalledTools, names) {
		r.CalledTools = names
		changed = true
	}
	return changed
}

// fragmented reports whether a tool-call list looks like unjoined stream
// fragments rather than whole calls.
//
// Being conservative matters more than catching every case: a false positive
// merges two real calls and concatenates their arguments into JSON that parses
// as neither, which is worse than leaving a wrong count in place. So the test is
// for evidence of an actual fragment, and a repeated index alone is not that -
// some providers restart the index per call, and two complete calls can legally
// share index 0.
func fragmented(calls []ToolCall) bool {
	for _, tc := range calls {
		// Arguments with no name can only be a continuation: the name arrives
		// on the opening delta and is never repeated.
		if tc.Function.Name == "" && tc.Function.Arguments != "" {
			return true
		}
		// A whole call's arguments are a complete JSON value. A fragment's are
		// a slice of one - `{"p"` or `:1}` - and do not parse. An empty string
		// is what the opening delta carries, so it is not evidence either way.
		if a := tc.Function.Arguments; a != "" && !json.Valid([]byte(a)) {
			return true
		}
	}
	return false
}

// trimReread recovers the one real JSON value from an argument string that
// re-reading duplicated.
//
// A tool's arguments are exactly one JSON value, so a second one can only be
// duplication - and the damaged records contain it, because the ingester
// re-folded spool bytes it had already read. That is the same double-counting
// that multiplied Turns. It left three shapes, all seen in production:
//
//	{"a":1}{"a":1}      a complete value, then the re-read tail
//	{"a{"a":1}          a truncated first capture, then a complete restart
//	{"ab...x|b...":1}   the re-read resumed mid-value, so head and tail overlap
//
// The third is the general case of the second: somewhere in the string, the tail
// continues a prefix of the head. Recovery means finding the overlap and
// splicing, and the guard is that the result must be valid JSON - a wrong splice
// essentially never parses.
//
// Anything not recognisable as one of these is returned untouched. Leaving
// visibly broken arguments for a human to look at beats guessing at a repair.
func trimReread(args string) string {
	if args == "" || json.Valid([]byte(args)) {
		return args // already exactly one value
	}

	// Shape 1: a complete value followed by a tail.
	dec := json.NewDecoder(strings.NewReader(args))
	var v any
	if err := dec.Decode(&v); err == nil {
		if end := dec.InputOffset(); end > 0 && int(end) <= len(args) {
			if first := args[:end]; json.Valid([]byte(first)) {
				return first
			}
		}
	}

	// Shape 2: a partial capture followed by a complete restart. Requires the
	// dropped prefix to be a genuine prefix of what follows - that is what makes
	// it a restart of the same value rather than two unrelated ones.
	for i := 1; i < len(args); i++ {
		if args[i] != args[0] {
			continue // cheap filter: a restart begins with the same byte
		}
		if !strings.HasPrefix(args[i:], args[:i]) {
			continue
		}
		if json.Valid([]byte(args[i:])) {
			return args[i:]
		}
	}

	// Shape 3: the re-read resumed partway through the value, so the stored
	// string is a truncated first copy followed by a suffix that overlaps it.
	//
	// The algebra: the string is whole[:a] + whole[b:] with b < a. The overlap
	// k = a-b means the k bytes before the split equal the k bytes after it, so
	// recovery is deleting one copy of that adjacent repeat:
	//
	//	D[s-k:s] == D[s:s+k]   =>   whole = D[:s-k] + D[s:]
	//
	// Searching for that pair is bounded on both axes. Unbounded it is O(n^2) on
	// arguments that reach hundreds of KB (a file write's content), which would
	// take minutes per record. The window is generous against the observed
	// corruption - the real records split ~100 bytes in, because the restart
	// offset b is small - and a miss costs one record left visibly broken rather
	// than a repair run that does not finish.
	//
	// The json.Valid call is the expensive part, not the byte comparison: on
	// repetitive content (a file of identical lines) the repeat test matches at
	// almost every (s,k), so validating each candidate re-parses the whole
	// string. Hence a hard cap on how many candidates are tried.
	const window = 4 << 10
	const maxTries = 512
	maxS := len(args)
	if maxS > window {
		maxS = window
	}
	tries := 0
	for s := 1; s < maxS; s++ {
		maxK := s
		if r := len(args) - s; r < maxK {
			maxK = r
		}
		if maxK > window {
			maxK = window
		}
		// Longest overlap first: the largest consistent splice is the right one,
		// and a short accidental repeat would splice into nonsense - which the
		// validity check below is what rejects.
		for k := maxK; k > 0; k-- {
			if args[s-k:s] != args[s:s+k] {
				continue
			}
			if tries++; tries > maxTries {
				return args
			}
			if cand := args[:s-k] + args[s:]; json.Valid([]byte(cand)) {
				return cand
			}
		}
	}
	return args
}

// recountFrom recomputes MsgCount/Turns/ToolCount from the stored request body
// and reports whether any of them moved. Returns false if there is no body to
// count.
func recountFrom(r *Record) bool {
	if r.Request.empty() {
		return false
	}
	var req reqView
	if err := json.Unmarshal(r.Request, &req); err != nil {
		return false
	}
	turns := 0
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			turns++
		}
	}
	tools := 0
	for _, t := range req.Tools {
		if t.Function.Name != "" || t.Name != "" {
			tools++
		}
	}
	if r.Turns == turns && r.MsgCount == len(req.Messages) && r.ToolCount == tools {
		return false
	}
	r.Turns, r.MsgCount, r.ToolCount = turns, len(req.Messages), tools
	return true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// runRepair is the -repair entry point; it returns true if it handled the
// invocation and the process should exit. See runReingest for why the flag set
// is only parsed when the flag is actually present.
func runRepair(cfg Config) bool {
	asked := false
	for _, a := range os.Args[1:] {
		if a == "-repair" || a == "--repair" {
			asked = true
			break
		}
	}
	if !asked {
		return false
	}

	fs := flag.NewFlagSet("repair", flag.ExitOnError)
	fs.Bool("repair", false, "recompute damaged derived fields in place")
	day := fs.String("day", "", "day to repair, yyyymmdd; empty means every archived day")
	dry := fs.Bool("dry-run", false, "report what would change without writing")
	if err := fs.Parse(os.Args[1:]); err != nil {
		log.Fatal(err)
	}

	a := newArchive(cfg.ArchiveDir)
	days := a.days()
	a.Close()
	if *day != "" {
		days = []string{*day}
	}
	if len(days) == 0 {
		log.Fatalf("repair: no archived days in %s", cfg.ArchiveDir)
	}

	totalScanned, totalFixed := 0, 0
	for _, d := range days {
		scanned, fixed, err := repairDay(cfg.ArchiveDir, d, *dry)
		totalScanned += scanned
		totalFixed += fixed
		if err != nil {
			log.Fatalf("repair %s: %v (%d records repaired before the failure)", d, err, totalFixed)
		}
		if fixed > 0 {
			log.Printf("repair %s: %d/%d records corrected", d, fixed, scanned)
		}
	}
	verb := "corrected"
	if *dry {
		verb = "would correct"
	}
	log.Printf("repair: %s %d of %d records across %s", verb, totalFixed, totalScanned,
		strings.Join(days, ", "))
	return true
}
