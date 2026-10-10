# Document Organizer: Architecture Design Document

## Context

Document Organizer (`docs`) is a single Go binary, standard library only, that files small text/markdown notes with a title and tags. It is driven from the shell (`add`, `list`, `get`, `search`, `delete`) or over a loopback-only HTTP JSON API (`serve`). Both front-ends call one storage package over one human-readable JSON file, `./docs.json` by default. The file is the source of truth. Processes keep an in-memory working copy and coordinate through a lock file. They work on Windows, macOS and Linux.

Design constraints taken from the PRD and the repo rules:
- No third-party modules. `go.mod` has no `require` lines.
- Errors are wrapped with `%w` and never discarded.
- `context.Context` is the first parameter of anything that does I/O or can block.
- Every goroutine has a shutdown path.
- No speculative abstractions.

Two PRD points are reconciled here because they affect the design:
1. **Missing Host header on HTTP/1.1.** Go's `net/http` server rejects it with 400 before any handler runs. See REQ-4.
2. **Cross-origin browser writes (CSRF).** The Host guard does not stop them, so the API adds an `Origin` check and a `Content-Type` requirement. See REQ-4.

## Components

```
docs/
  go.mod                      module docs, no require lines
  main.go                     os.Exit(cli.Run(...)), nothing else
  internal/
    store/
      store.go                Store, Open, Add, Get, Delete, List, refresh
      document.go             Document, Input, Normalize/validation, Filter, ParseFilter, Match, sort
      lock.go                 file lock: acquire, stale recovery, release
      atomic.go               temp write + fsync + rename with retry
      rename_windows.go       sharing-violation detection (build tag)
      rename_other.go         no-op detection
    cli/
      cli.go                  Run(ctx, args, stdin, stdout, stderr) int; subcommand dispatch; flags; exit-code mapping
      serve.go                addr validation, listener, graceful shutdown
    httpapi/
      handler.go              NewHandler(*store.Store) http.Handler; routes; JSON helpers
      guard.go                Host guard, Origin guard, JSON content-type check
```

| Component | Responsibility |
|---|---|
| `store` | The only code that touches the data file. Validation, filtering, sorting, locking and atomic persistence live here, so there is no logic duplicated across CLI and HTTP. |
| `cli` | Flag parsing (`flag.FlagSet` per subcommand), reading content from file/stdin, JSON output, mapping store errors to exit codes. |
| `httpapi` | Routing, guards, mapping store errors to status codes. It is a thin adapter. |

Store error sentinels and types, mapped by both front-ends:

| Error | CLI exit | HTTP |
|---|---|---|
| `*ValidationError` (includes invalid filters) | 2 | 400 |
| `ErrNotFound` | 1 | 404 |
| `ErrLockTimeout` | 1 | 503 |
| `ErrCorrupt` | 1 | 500 |
| other I/O error | 1 | 500 |

Non-numeric IDs are a `ValidationError` raised at the edge (`strconv.ParseInt`): CLI exit 2, HTTP 400.

## REQ-1 Storage, persistence and concurrency

**Path resolution.** The precedence is `--data` flag, then the `DOCS_FILE` env var, then `./docs.json`. It is resolved in `cli` (`resolvePath(flagVal, getenv)`) and passed to `store.Open(ctx, store.Options{Path, LockTimeout})`. `--lock-timeout` is a `time.Duration` flag, default 30s, registered on every subcommand through one shared helper.

**Store struct.**
```go
type Store struct {
    mu       sync.Mutex // guards the fields below within the process
    path     string
    timeout  time.Duration
    data     fileData   // in-memory working copy
    modTime  time.Time  // stat of the file when last loaded
    size     int64
    loaded   bool
}
```

**Mutation sequence** (`Add`, `Delete`), all under `s.mu`:
1. `acquireLock(ctx, path+".lock", timeout)`.
2. `loadFile` reads the file with `os.ReadFile`. A missing file gives an empty store with `next_id=1`. Invalid JSON gives `ErrCorrupt` naming the file.
3. Apply the change. For `Add`, take `id = next_id` and then `next_id++`. For `Delete`, remove the document and leave `next_id` unchanged, so IDs are never reused.
4. `writeAtomic`: `os.CreateTemp(dir, base+".tmp*")`, write, `Sync`, `Close`, `renameWithRetry`. On any failure the temp file is removed and the original file is untouched.
5. Update `data`, `modTime` and `size` from a fresh `os.Stat`, then release the lock with `defer`.

**Lock file.**
- It is created with `os.OpenFile(lockPath, O_CREATE|O_EXCL|O_WRONLY, 0o644)`. The content is `pid=<pid> time=<RFC3339>`.
- On `os.IsExist`, the loop `Stat`s the lock. If `time.Since(mtime) > 10s` it is stale, so we `os.Remove` it and retry immediately.
- Otherwise it sleeps `10ms + rand(0..40ms)` with a `select` on `ctx.Done()` and a timer, and retries until the deadline.
- On timeout it returns `ErrLockTimeout`, formatted as `lock timeout: could not acquire <abs lock path>; if no other docs instance is running, delete this file`. The path is absolute so that acceptance (d) can match it on stderr.
- Release is `os.Remove`, and an error is returned wrapped (it is not discarded). The release error is joined with the operation's error via `errors.Join` so neither is lost.
- A holder slower than 10s could have its lock removed as stale. This is accepted: the critical section is a read plus one small write, so it is far shorter than 10s. Before removing a stale lock, acquisition re-`Stat`s and removes only if the mtime is unchanged, which narrows the remove race.

**Reads** (`List`, `Get`) take `s.mu` but not the file lock. `refresh(ctx)` `Stat`s the file. If mtime or size differs from the cached values, or the store was never loaded, it reloads with `os.ReadFile`. A missing file gives an empty store. This is how the server sees CLI writes. A same-size rewrite within the filesystem's mtime granularity can be missed by a read. The next change is picked up, and mutations always reload unconditionally, so writes are never affected.

**Windows rename.** `renameWithRetry(ctx, tmp, dst)` calls `os.Rename`. If `isSharingViolation(err)` is true (`syscall.Errno` 32 or 5, in `rename_windows.go`; always false on other platforms), it sleeps `25ms + rand(0..75ms)` and retries until 5s have elapsed in total. After that it returns the wrapped error (CLI exit 1, HTTP 500) and the original file stays intact. Readers use a single `os.ReadFile`, so they hold the file open only briefly.

**Corrupt file.** `ErrCorrupt` is returned by every operation, including reads. No code path writes after a failed decode, because the decode happens before the apply step.

**Acceptance mapping:**
- (a) is covered by persisted `next_id`.
- (b) is covered by lock plus reload-modify-write.
- (c) is covered by stale recovery.
- (d) is covered by the timeout error text and the `--lock-timeout` flag.
- (e) is covered by `ErrCorrupt` and the never-overwrite rule.
- (f) is covered by temp-file-first writes.

## REQ-2 Add, get and delete documents

**Validation** lives in `store.Normalize(Input) (Input, error)`, called by `Add` before the lock is taken, so invalid input never touches the disk.
- `title`: `strings.TrimSpace`, non-empty, at most 200 characters (`utf8.RuneCountInString`).
- `content`: at most 1 MiB (`len` in bytes, 1<<20).
- `tags`: each is trimmed and lowercased. Empty tags, tags containing `,`, and tags over 50 characters are rejected. Duplicates are removed while keeping first-seen order. A nil tag list is stored as `[]`.
- `created_at`: `time.Now().UTC()`, serialized as RFC 3339 (`time.RFC3339` in UTC).

**CLI.**
- `add --title T [--tag X]... [--file PATH]` uses a repeatable `--tag` flag (`flag.Value`). Content is read from `--file`, or from stdin when `--file` is omitted or is `-`. The read is wrapped in `io.LimitReader(…, 1MiB+1)` so oversized input is detected without reading it all. It prints the created document as JSON.
- `get ID` prints one document. `delete ID` prints nothing on stdout and exits 0. This choice is made explicit here because the PRD does not specify delete output.
- A missing required flag, or an unparseable flag, exits 2 with a message on stderr.

**HTTP** (`httpapi`):

| Method and path | Success | Notes |
|---|---|---|
| `POST /documents` | 201 + document | JSON body `{title, content, tags}`, `Content-Type: application/json` required (REQ-4). Body is wrapped by `http.MaxBytesReader` at 2 MiB, and oversized bodies give 413. Decoding uses `DisallowUnknownFields`. Malformed JSON gives 400. |
| `GET /documents/{id}` | 200 + document | Uses `http.ServeMux` method+wildcard patterns. |
| `DELETE /documents/{id}` | 204, no body | |

Decode errors, validation errors and non-numeric IDs give 400. An unknown ID gives 404. A body that is too large gives 413, which is the only status here beyond those in the PRD. It is a validation-style failure for a body that exceeds the PRD's 2 MiB limit.

## REQ-3 List and search

**Filter type and parsing** are in `store`, shared by both front-ends:
```go
type Filter struct {
    Tags []string // normalized: trimmed, lowercased
    Q    string   // trimmed
}
func ParseFilter(tags []string, q *string) (Filter, error) // *ValidationError on bad input
```

Rules, enforced once:
- A supplied tag that is empty or whitespace-only, or that contains a comma, is invalid. A supplied `q` that is empty or whitespace-only is invalid.
- `q` being "supplied" is distinguished from absent by a `*string`. The CLI uses `flag.Visit` to see whether `--q` was set. HTTP uses `r.URL.Query()["q"]`, which is present with the empty string for `?q=`.
- `search` with no tags and no `q` is a `ValidationError` (CLI exit 2, message "use list"). `GET /documents` with no query parameters is `list`.
- Any other query parameter on HTTP is ignored.

**Matching** (`Match(doc, Filter)`): every filter tag must equal one of the document's tags, because stored tags are already lowercase. If `Q` is set, `strings.Contains(lower(title), lower(q)) || strings.Contains(lower(content), lower(q))`. The tag and text conditions are ANDed.

**Ordering** is `sort.SliceStable` by `created_at` descending, then `id` descending. A zero-match result is a non-nil empty slice, which encodes as `[]`.

**Output.** The CLI and HTTP return the same JSON array. `list --summary` (CLI only) writes the documents with `content` omitted, using a `Summary` projection type. HTTP always includes content. `GET /documents` calls `store.List(ctx, filter)`, and the CLI `list` and `search` commands call the same function.

## REQ-4 HTTP server and local-only exposure

**Address validation** is in `cli/serve.go`, as `parseListenAddr(addr) (bindAddr string, err error)`. It runs before `net.Listen`, so failures exit 2 without binding.
- `net.SplitHostPort`. An empty host is rejected.
- The host `localhost` (case-insensitive) is rewritten to `127.0.0.1`, so it listens on IPv4 loopback only.
- Otherwise `netip.ParseAddr(host)` must succeed and `IsLoopback()` must be true. This accepts 127.0.0.0/8 and `::1`. IPv6 zones and IPv4-mapped forms are rejected. No DNS lookup is performed, so any other hostname is rejected.
- The port must be numeric.
- The listener is created with `net.Listen("tcp", bindAddr)` and the `http.Server` is served on it.

**Request guard order.** Guards wrap the mux in `httpapi/guard.go`:
1. **Host guard.** `hostOnly(r.Host)` strips the port, and brackets for IPv6, and handles a bare `[::1]` with no port. It lowercases and allows only `localhost`, `127.0.0.1`, `::1`. An empty or other value gets 403 `{"error":"forbidden host"}`.
2. **Origin guard.** This closes the browser cross-origin write gap. The Host guard only defeats DNS rebinding. A page on another site can still send a "simple" cross-origin `POST http://127.0.0.1:8080/documents` with `Content-Type: text/plain`, which has an allowed Host and needs no preflight. If an `Origin` header is present, its URL host is parsed with `url.Parse` and checked against the same loopback allow-list. `Origin: null`, an unparseable value, or a non-loopback host gets 403. Absent `Origin` (curl, scripts, CLI tooling) is allowed. This applies to every method.
3. **Routing and method.** `http.ServeMux` handles unknown methods on a known path with 405 and an `Allow` header. Because `ServeMux` writes plain text for 405, the mux is wrapped so that 405 and 404 are rewritten into the JSON error format. `OPTIONS` is not handled and gets 405. No CORS headers are ever sent, so browsers cannot get a preflight approved for cross-origin `DELETE` or JSON `POST`.
4. **Content-Type check** (only on `POST /documents`, after routing so that `PUT /documents` still returns 405). `mime.ParseMediaType(r.Header.Get("Content-Type"))` must give `application/json`, and a `charset=utf-8` parameter is tolerated. Anything else, including missing or `text/plain`, gets 415 `{"error":"Content-Type must be application/json"}`. Cross-origin pages cannot send `application/json` without a preflight, which this server never approves.
5. **Body limit and handler.** The 2 MiB limit from REQ-2, then the handler.

The PRD lists 400 for validation failures. The 415 here is an additional status on the write endpoint, introduced for the CSRF defense. It is called out so the PRD can be amended.

**Missing Host header: reconciliation with the PRD.**
- Go's `net/http` server rejects an HTTP/1.1 request that has no `Host` header with its own `400 Bad Request` before the handler chain runs. The standard library offers no hook to change that without replacing the HTTP parser, which is out of proportion for this tool. The PRD acceptance (a) text "no Host header returns 403" therefore cannot hold literally for HTTP/1.1.
- The design adopts this behavior, and the PRD acceptance wording should be amended to match:

| Request | Result | Produced by |
|---|---|---|
| HTTP/1.1, no `Host` header | 400, never reaches the handler | `net/http` |
| HTTP/1.1, `Host:` present but empty | 403 | Host guard |
| HTTP/1.0, no `Host` header | 403 | Host guard |
| `Host: evil.example`, `localhost.evil.example` | 403 | Host guard |

- In every case the request is rejected and no data is read or written. The guard's own empty-Host branch stays in place as defense in depth for HTTP/1.0 and empty values.
- The test for this is run against a real listener, not by calling the handler directly. See Testing strategy.

**Responses.** `writeJSON(w, status, v)` always sets `Content-Type: application/json; charset=utf-8` and `X-Content-Type-Options: nosniff`. Errors use `{"error":"..."}`. Documents are never served as HTML.

**Server lifecycle.** `serve` builds `ctx, stop := signal.NotifyContext(ctx, os.Interrupt)`. The server runs as:
```go
srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
errc := make(chan error, 1)
go func() { errc <- srv.Serve(ln) }()
select {
case <-ctx.Done():  // SIGINT
    shCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
    defer cancel()
    err = srv.Shutdown(shCtx)  // waits for in-flight requests
case err = <-errc:
}
```
The serving goroutine's shutdown path is `Shutdown`, after which `Serve` returns `http.ErrServerClosed`, which is treated as a clean exit (exit 0). A `BaseContext` derived from the signal context would cancel in-flight store calls on SIGINT. So the request context handed to handlers is derived from `context.WithoutCancel(ctx)`, and in-flight requests finish normally, which satisfies acceptance (d). A non-clean `Shutdown` error gives exit 1.

## REQ-5 CLI conventions and output

- `main.go` calls `os.Exit(cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))`. `Run` is a pure function of its arguments, so tests call it directly.
- Subcommands are dispatched with a `switch` over `add|list|get|search|delete|serve|help`. Each uses its own `flag.NewFlagSet(name, flag.ContinueOnError)` with output directed to stderr. All register `--data` and `--lock-timeout` through one helper (`addCommonFlags`).
- `help` prints usage to stderr and exits 0. An empty argument list or an unknown subcommand prints usage to stderr and exits 2.
- Success output is JSON on stdout (`json.NewEncoder` with indent). Diagnostics are written only to stderr, one line per error in the form `docs: <message>`, so stderr is empty on success.
- Exit codes: 0 success, 1 runtime failure (not found, I/O, lock, corrupt data), 2 usage or validation error. The mapping is a single function, `exitCode(err error) int`, using `errors.Is`/`errors.As` on the store error types.
- `go.mod` declares only `module` and `go`, with no `require` lines. CI-equivalent checks are `go build ./...`, `go vet ./...` and `go test ./...`.

## Data model

File format, pretty-printed with two-space indent for readability:
```json
{
  "next_id": 4,
  "documents": [
    {"id": 3, "title": "Go notes", "content": "…", "tags": ["go", "cli"], "created_at": "2026-10-10T09:15:00Z"}
  ]
}
```

```go
type Document struct {
    ID        int64     `json:"id"`
    Title     string    `json:"title"`
    Content   string    `json:"content"`
    Tags      []string  `json:"tags"`
    CreatedAt time.Time `json:"created_at"`
}
type fileData struct {
    NextID    int64      `json:"next_id"`
    Documents []Document `json:"documents"`
}
```

- `next_id` starts at 1 for a missing file. It is persisted on every write and never decremented.
- If a file's `next_id` is lower than `max(id)+1`, `loadFile` treats it as corrupt (`ErrCorrupt`) rather than risk reusing an ID.
- Documents are stored in insertion order. Sorting happens at read time.
- The lock file `<data>.lock` holds `pid=<pid> time=<RFC3339>`. Temp files are `<data>.tmp*` in the same directory, and they are removed on failure.

## Failure modes

| Failure | Behavior |
|---|---|
| Data file missing | Treated as an empty store. The first write creates it. |
| Data file invalid JSON, or `next_id` inconsistent | Every command fails (CLI exit 1, HTTP 500) with an error naming the file. The file is never written. |
| Lock held by a live process | Retry with jitter until `--lock-timeout`. Then `ErrLockTimeout` (exit 1, HTTP 503) with the absolute lock path and the "may be deleted if no other instance is running" hint. |
| Lock left by a crashed process | After 10s of mtime age it is removed and acquisition retries. Until then, callers wait or time out as above. |
| Crash between temp write and rename | The old data file is intact. A stray `.tmp*` file is harmless and is not read. |
| Temp write, `Sync` or `Close` fails (read-only directory, disk full) | The temp file is removed and the original is byte-identical (exit 1, HTTP 500). |
| Rename sharing violation on Windows | Retried for up to 5s total. Then the write fails (exit 1, HTTP 500) with the original intact. |
| Lock release fails | The error is joined with the operation result, never swallowed. |
| Request body over 2 MiB | 413. Content over 1 MiB gives 400. |
| Cross-origin browser request | 403 from the Origin guard. Non-JSON `POST` gives 415. Nothing is persisted. |
| Host header missing or foreign | 400 from `net/http` (HTTP/1.1, none) or 403 from the guard. See REQ-4. |
| SIGINT | In-flight requests finish. Then the server exits 0. |
| Context cancelled while waiting for the lock | The wait returns promptly with the wrapped `context.Canceled`. |

## Testing strategy

All tests use `go test` with `t.TempDir()`. Short `--lock-timeout` values keep failure paths fast.

**`store` unit tests**
- Normalization table: title trim, empty, 200/201 characters, content at and over 1 MiB, tag trim/lowercase/dedupe/comma/over 50 characters.
- ID persistence: add, delete the highest ID, add again, and the new ID is greater. Also checks restart persistence through a reopened `Store`.
- Filter table covering REQ-3 (a)–(d): AND tags, case-insensitive tags, `q` in title and body, combined filters, ordering with ties, `[]` on zero matches, and every invalid-filter form.
- Lock behavior:
  - A fresh lock file with a 1s timeout fails within 1–3s, and the error contains the lock path.
  - A lock file whose mtime is set more than 10s back with `os.Chtimes` is recovered.
- Corrupt file: bytes are identical after failed `List`, `Add` and `Delete`.
- Write failure: a read-only directory (skipped where permissions cannot be enforced) or an injected failing writer leaves the original byte-identical.
- Rename retry: on all platforms, the retry loop is tested with an injected rename function that returns a "sharing violation" N times. The windows-only error classification is tested in a `_windows_test.go` file.

**Concurrency test** (REQ-1 (b)): a 100-document fixture, 40 concurrent writers (a mix of `store.Add` through separate `Store` instances, CLI `Run` calls, and HTTP POSTs to an `httptest.Server`) and 20 concurrent readers looping `List`. All operations succeed. The final file is valid JSON with 140 documents and 140 distinct IDs. This is run with `-race` where available.

**`httpapi` tests** (`httptest`, handler called directly)
- CRUD: 201/200/204, 400 for validation and non-numeric ID, 404 for unknown ID, 405 for `PUT /documents`, and the JSON error shape.
- Filter parameters: `?tag=`, `?q=%20` and `?tag=a,b` each give 400.
- Host guard matrix: `localhost:8080`, `127.0.0.1:8080`, `[::1]:8080` and mixed case give 200. `evil.example`, `localhost.evil.example` and an empty Host give 403.
- **Origin guard:** `Origin: http://evil.example` and `Origin: null` give 403 for `POST` and `DELETE`, with nothing persisted. `Origin: http://localhost:8080` and `http://127.0.0.1:3000` pass. An absent `Origin` passes.
- **Content-Type guard:** `POST /documents` with `text/plain`, `application/x-www-form-urlencoded`, `multipart/form-data` and no `Content-Type` each give 415 with nothing persisted. `application/json` and `application/json; charset=utf-8` give 201. `PUT /documents` with `text/plain` still returns 405, which pins the guard order.
- No `Access-Control-*` header appears on any response, and `OPTIONS /documents` returns 405.
- Oversized body gives 413.

**Real-listener tests** (`serve_test.go`, `net.Listen("tcp","127.0.0.1:0")` plus a raw `net.Conn`; these are the only tests that can show what `net/http` does before the handler runs):
- `GET /documents HTTP/1.1` with no Host header asserts the actual status line is 400, and that no data was read or written.
- `GET /documents HTTP/1.1` with `Host:` empty asserts 403.
- `GET /documents HTTP/1.0` with no Host header asserts 403.
- `Host: evil.example` over the wire asserts 403.
- A raw cross-origin-style `POST` with `Content-Type: text/plain` and `Origin: http://evil.example` asserts a rejection and an unchanged data file.
- Graceful shutdown: a slow handler (an injected delay hook in the test handler wrapper) is in flight, the test triggers the cancellation that `signal.NotifyContext` would deliver, and it asserts the request completes with 200 before `serve` returns 0.

**`cli` tests** (call `Run` with buffers):
- Exit codes: `help` 0, no subcommand 2, unknown subcommand 2, missing `--title` 2, bad filter 2, unknown ID 1, non-numeric ID 2, corrupt file 1.
- `add` via `--file` and via stdin, then `get` returns identical title, content and normalized tags.
- `list --summary` omits `content`. On success, stdout parses as JSON and stderr is empty.
- `--data` beats `DOCS_FILE`, which beats the default (using `t.Setenv`).
- `parseListenAddr` table: accepts `127.0.0.1:8080`, `127.5.5.5:1`, `localhost:8080` (bound as 127.0.0.1) and `[::1]:8080`. Rejects `:8080`, `0.0.0.0:8080`, `[::]:8080`, `192.168.1.5:8080` and `example.com:8080` with exit 2 and no listener created.

**Repo checks:** `go vet ./...` and `go test ./...`. A `go.mod` check confirms there are no `require` lines. Performance reference runs (1,000 documents, about 1 MB) are a separate benchmark that reports medians together with hardware context. They are not a gate.

## Rollout

This is a new tool with no existing data or users to migrate.

1. Implement in this order, each step with its tests: `store` (document/filter, then lock and atomic write), then `cli` for the non-serve commands, then `httpapi` with guards, then `serve` with the real-listener tests.
2. Run `go vet ./...` and `go test ./...` on the target platforms (Windows, macOS, Linux). The Windows rename-retry path needs a Windows run.
3. Ship as a single binary built with `go build -o docs .`. No installer, service, or configuration beyond the optional `--data` flag and `DOCS_FILE` env var.
4. Before release, amend PRD REQ-4 acceptance (a) to record the HTTP/1.1 missing-Host behavior (400 from `net/http`, still rejected, nothing processed). Also add the new `Origin` and `Content-Type` rules and their 403 and 415 statuses, plus the 413 for oversized bodies.
5. Rollback is deleting the binary. The data file is plain JSON, can be read or edited by hand, and has no migration. A stale `<data>.lock` can be deleted if no other instance is running.