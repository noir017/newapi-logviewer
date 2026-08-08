# Contributing

## Setup

```bash
go test ./...        # golden tests over testdata/sample.log; no setup needed
go test -race ./...  # needs CGO_ENABLED=1
go vet ./...
gofmt -l .           # must print nothing; CI fails otherwise
```

There is no build step for the frontend. `ui.html` is embedded with `go:embed`;
edit it and rebuild the binary.

## Never commit real logs

New API's debug logs contain complete prompts, responses, tool arguments, and
whatever your users typed. `.gitignore` excludes `*.log` with a single exception
for `testdata/sample.log`, which is synthetic.

If you need a new test case, add synthetic lines to `testdata/sample.log` rather
than pasting real traffic. Check before committing:

```bash
git diff --cached --stat
```

## Parser changes

`logparse.go` keys off New API's log line formats. If you are adding support for
a new line type:

1. Add a representative line to `testdata/sample.log`.
2. Assert the derived fields in `logparse_test.go`.
3. Make sure existing subtests still pass — the fixture's call count is asserted
   in a few places and adding a call will (correctly) fail those until updated.

Malformed input must never panic. `TestGarbageInput` covers truncated JSON,
unparseable lines, and missing fields; extend it when you touch parsing.

## Field names are a wire contract

`Record`'s JSON tags are consumed directly by the browser code in `ui.html`.
Renaming a field means updating both, and there is no type checker spanning the
two — grep `ui.html` for the tag before changing it.

## Verifying a change end to end

The fixture is enough to run the real thing:

```bash
go build -o logviewer . && LOG_DIR=./testdata BASE_PATH=/logviewer ./logviewer
# http://localhost:7070/logviewer/
```

## Scope

This is a read-only viewer. It does not write, mutate, or call back into New API
except to validate a token in `bearer` mode. Features that need write access to
New API belong in New API.
