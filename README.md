# newapi-logviewer

A read-only web viewer for [New API](https://github.com/QuantumNous/new-api) LLM call logs.

New API's built-in log page shows what a request *cost*. It does not show what was
*in* it. With `DEBUG=true`, New API writes the full outbound request body, the
upstream response body, and every streamed chunk to its file log — but as
interleaved lines that are painful to read by hand.

This service parses those files and serves a searchable UI: prompts, tool
definitions, tool calls, reasoning traces, token usage and billing, grouped per
call.

- **7 MB image, ~2 MB RSS idle.** Static Go binary on `scratch` — no OS, no shell, no interpreter.
- **Read-only.** The log directory is mounted `:ro`; the viewer cannot alter the audit trail.
- **No database, no state, no writes.** It only reads files New API already produces.

![master-detail layout](docs/screenshot.md)

---

## Requirements

New API must be running with `DEBUG=true` and writing to a log directory:

```yaml
# in your new-api service
environment:
  - DEBUG=true         # logs request/response bodies; also lifts the 2048-char
                       # truncation on upstream error bodies
  - LOG_DIR=/app/logs  # or wherever; mount it somewhere you can share
volumes:
  - ./logs:/app/logs
```

Without `DEBUG=true` the log contains only routine lines and this viewer will
show nothing useful.

> **`DEBUG=true` writes every prompt and response to disk in plaintext**, with no
> rotation. Read [Security](#security) before enabling it.

## Quick start

```bash
git clone https://github.com/<you>/newapi-logviewer
cd newapi-logviewer
cp .env.example .env
$EDITOR .env            # set NEWAPI_LOG_DIR and TZ
docker compose up -d
```

Then open <http://localhost:7071/logviewer/>.

To run it alongside an existing New API stack, see
[examples/newapi-stack.yml](examples/newapi-stack.yml).

### Without Docker

```bash
go build -o logviewer .
LOG_DIR=/path/to/new-api/logs BASE_PATH=/logviewer ./logviewer
```

## Configuration

All configuration is environment variables. Only `LOG_DIR` matters for a basic run.

| Variable | Default | Meaning |
|---|---|---|
| `LOG_DIR` | `/logs` | Directory containing New API's `*.log` files |
| `PORT` | `7070` | Listen port |
| `BASE_PATH` | `/logviewer` | URL prefix. Use `/` to serve at the root |
| `TZ` | container default | **Must match New API's timezone** — see below |
| `AUTH_MODE` | `none` | `none` or `bearer` |
| `NEWAPI_URL` | `http://new-api:3000` | Where to validate tokens (`bearer` mode only) |
| `REQUIRE_ADMIN` | `true` | In `bearer` mode, also require `role >= 100` |
| `LIMIT_MB` | `40` | Only parse the last N MB of each log file |
| `CACHE_TTL` | `3` | Seconds between disk-change checks |
| `AUTH_TTL` | `120` | Seconds to cache a token verdict |

### Timezone

New API's log lines carry no UTC offset:

```
[DEBUG] 2026/01/15 - 09:01:10 | <request-id> | text request body: {...}
```

They are local wall-clock time in whatever zone New API runs in. This service
interprets them using its own `TZ`, so **if the two disagree, every timestamp is
off by the difference and the time-range filter silently returns wrong rows.**
Set `TZ` to the same value New API uses.

The timezone database is compiled into the binary, so `TZ` works even though the
image has no `/usr/share/zoneinfo`.

## Authentication

**`AUTH_MODE=none`** (default) — no authentication. Anyone who can reach the URL
reads every prompt, response and tool argument. Only acceptable on a trusted
network.

**`AUTH_MODE=bearer`** — every request must carry a New API **dashboard access
token**:

```
Authorization: Bearer <access_token>
```

The token is validated against New API's `/api/user/self`, so revoking it in New
API revokes access here. With `REQUIRE_ADMIN=true` (default) the account must
also be an admin (`role >= 100`).

Two caveats worth knowing before you pick this mode:

- **A relay API key (`sk-...`) will not work.** New API's `/api/user/self`
  rejects relay keys — they authenticate a different code path. You need a
  dashboard access token.
- **A browser cannot supply this automatically.** New API's SPA keeps its access
  token in memory (bare zustand, no persist middleware), so there is no cookie or
  `localStorage` entry for a reverse proxy or this service to pick up. `bearer`
  mode is practical for scripted access; for browsing, put the viewer behind your
  own auth (see below) and leave `AUTH_MODE=none`.

`/healthz` is always reachable without a token.

### Recommended: put it behind your reverse proxy's auth

Since browser-side token auth is impractical, the realistic setup is
`AUTH_MODE=none` plus authentication at the proxy. Example with nginx basic auth:

```nginx
location /logviewer/ {
    auth_basic "log viewer";
    auth_basic_user_file /etc/nginx/.htpasswd;

    proxy_pass http://logviewer:7070/logviewer/;
    proxy_set_header Host $http_host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_http_version 1.1;
}
```

See [examples/nginx.conf](examples/nginx.conf) for a full server block, including
the SSE settings New API itself needs on the main location.

## The UI

Master-detail: pick a call on the left, read it on the right. The splitter drags
and remembers its width. Below 860px it stacks vertically.

**Filtering** — time (quick presets or a custom `datetime-local` range), free-text
search across prompts/responses/request-id, model, tool name, status, streaming,
has-tools, has-errors.

**List rows** show timestamp, model, status, cost, latency, and tool chips.
A chip is highlighted when that tool was *actually called*, outlined when it was
merely *defined* — the distinction matters when debugging why a model ignored a
tool. Clicking a chip filters by it.

**Detail pane** — overview (latency, first-token latency, channel, token name,
cost, ratios, upstream URL), token usage including reasoning and cached tokens,
the full message list with roles, tool definitions with parameter schemas,
model output, tool calls with formatted arguments, reasoning traces, request
parameters, and the raw response JSON.

Streamed responses are reassembled: chunks are merged back into content,
reasoning, and tool calls, so a streaming call reads the same as a
non-streaming one.

### What it cannot show

**HTTP headers.** New API does not log them — not request headers, not response
headers. There is no header-logging code in the upstream source, so no amount of
parsing recovers them. The closest available data is upstream metadata that some
gateways emit as SSE comment lines, which shows up in the raw response section.

If you need headers, they have to be added to New API itself.

## API

The UI is a client of a small JSON API you can script against.

```
GET /api/calls?page=1&page_size=30&search=&model=&tool_name=&status=&stream=
               &tools=&errors=&since=&until=&refresh=
GET /api/call?id=<request_id>
GET /healthz
```

`since` / `until` are epoch seconds. `refresh=1` forces a re-parse.

`/api/calls` returns `{total, page, page_size, models, tools, items}` — `models`
and `tools` are the full sets across all records, for populating filter dropdowns.

```bash
# every call that invoked a given tool, as JSON
curl -s 'http://localhost:7071/logviewer/api/calls?tool_name=get_weather&page_size=100' \
  | jq '.items[] | {ts, model, called_tools, quota}'
```

## Security

This service shows **complete prompts and responses**. Treat access to it as
equivalent to access to your LLM traffic.

- `DEBUG=true` writes all of that to disk in plaintext, and New API does **not**
  rotate those files. Add logrotate, or the directory grows until the disk fills.
  See [docs/logrotate.md](docs/logrotate.md).
- The log mount is `:ro` and the process runs as `nobody` (65534). It has no
  shell — the `scratch` image contains one file.
- Never commit real logs. `.gitignore` excludes `*.log` for this reason.
- The viewer never writes anything, anywhere.

## How it works

New API emits one line per event, all tagged with a request id:

```
[DEBUG] 2026/01/15 - 09:01:10 | <rid> | text request body: {...}
[DEBUG] 2026/01/15 - 09:01:12 | <rid> | upstream response body: {...}
[DEBUG] 2026/01/15 - 09:02:31 | <rid> | stream scanner data: data: {...}
[INFO]  2026/01/15 - 09:01:12 | <rid> | record consume log: userId=1, params={...}
[GIN]   2026/01/15 - 09:01:12 | relay | <rid> | 200 | 2.01s | 10.0.0.9 | POST /v1/...
```

Events from concurrent calls interleave, so the parser groups by request id and
derives per-call summaries. Some deliberate choices:

- **Request and response bodies are never unmarshalled into maps.** They are kept
  as raw bytes and spliced into the API response; only a small "partial view"
  struct is decoded to pull out the handful of fields the list needs. This is
  most of the memory difference versus a naive implementation.
- **The search haystack is built once at parse time**, so a keystroke in the
  search box does not re-serialize every record.
- **The cache invalidates on mtime+size**, not on a timer. An idle tab with
  auto-refresh on costs a few `stat` calls instead of re-parsing the log.
- **Truncated or rotated-out bodies do not drop a record.** It is flagged
  `incomplete` and rendered with whatever fields survive.

## Development

```bash
go test ./...                       # golden tests over testdata/sample.log
go test -race ./...                 # concurrency (needs CGO_ENABLED=1)
go vet ./...
```

`testdata/sample.log` is synthetic and covers the shapes that matter: a plain
call, a streaming call with tools and reasoning, a multimodal request, an
upstream error, a truncated body, and non-call noise.

The UI is a single `ui.html` embedded into the binary with `go:embed` — no build
step, no bundler, no static-file mount to keep in sync. Edit it and rebuild.

## Compatibility

Developed against New API `v1.0.0-rc.24`. The parser keys off log line formats
rather than internal APIs, so it should tolerate minor version drift; a format
change in New API's logging would need a parser update.

## License

MIT
