# Document Organizer: PRD

## Problem

A single user keeps many small text and markdown notes and has no lightweight way to file them with a title and tags, or to find them again later. Existing note tools are heavy, need accounts or services, or store data in opaque formats. The user wants a tiny local tool, with no dependencies and a human-readable JSON data file, that can be driven from the shell and from scripts over local HTTP.

## Goals

- Add, list, get, search and delete text/markdown documents with a title and optional tags.
- Offer the CLI and a local HTTP JSON API in one binary, over one storage layer and one data file.
- Keep data safe: no corrupted data file, no lost writes from concurrent CLI/server use, and a recoverable failure mode if a process crashes.
- Build with the Go standard library only, and run only on the local machine.

## Requirements

**REQ-1: Storage, persistence and concurrency**
- All data lives in one JSON file, `./docs.json` by default. The path is overridable by the `--data` flag or the `DOCS_FILE` env var, and the flag wins over the env var.
- File format: `{"next_id": N, "documents": [{"id", "title", "content", "tags", "created_at"}]}`. IDs are auto-incrementing and never reused, including after deletes. `next_id` is persisted for this reason.
- The store is an in-memory working copy guarded by a mutex within a process. Every mutation (add, delete) does the following:
  1. Acquires a cross-process lock file `<data>.lock`, created with O_EXCL and containing the PID and a timestamp.
  2. Reloads the data file from disk.
  3. Applies the change.
  4. Writes a temp file in the same directory, fsyncs it, and renames it over the data file.
  5. Updates the in-memory copy and releases the lock.
- Reads (list, get, search) take no file lock. They refresh the in-memory copy from disk if the file's mtime or size changed, so the server sees CLI changes.
- Lock acquisition retries every 10–50 ms (random jitter) until a timeout, which is 30 s by default and settable with `--lock-timeout`.
- Stale-lock recovery: a lock file whose mtime is older than 10 s is treated as stale. It is removed and acquisition is retried. The lock is held only for the reload-modify-write sequence, which is a small fraction of that age.
- If acquisition times out, the operation fails (CLI exit 1, HTTP 503). The error message includes the full lock file path and says the file may be deleted if no other instance is running.
- On Windows, if the rename fails with a sharing violation, it is retried for up to 5 s total (every 25–100 ms with jitter). Readers hold the data file open only for the duration of a single `os.ReadFile`. Only after the retries are exhausted does the write fail (CLI exit 1, HTTP 500), and the original data file stays intact.
- A missing data file is treated as an empty store. A data file that is not valid JSON makes every command fail with a non-zero exit and an error naming the file, and it is never overwritten.
- Acceptance:
  (a) After `add` then restart, `list` returns the document. After deleting the highest ID and adding another, the new ID is greater than the deleted one.
  (b) With a fixture of 100 documents, 40 concurrent `add` processes (mixed CLI and HTTP POSTs) while 20 concurrent readers loop over `list` all succeed. The final file is valid JSON with exactly 140 documents and 140 distinct IDs.
  (c) Killing a process between lock creation and release leaves a lock file. After the lock's mtime is set to more than 10 s in the past, the next `add` succeeds.
  (d) With a fresh (non-stale) lock file present and `--lock-timeout 1s`, `add` exits 1 within 1–3 s and its stderr contains the lock file's path.
  (e) A corrupt data file makes `list` exit non-zero, and the file's bytes are unchanged afterward.
  (f) A temp-file write failure (for example, a read-only directory) leaves the original data file byte-identical.

**REQ-2: Add, get and delete documents**
- Fields:
  - `title` is required. It is trimmed, non-empty and at most 200 characters.
  - `content` is at most 1 MiB.
  - `tags` is optional. Tags are trimmed and lowercased, de-duplicated, non-empty, contain no commas, and are at most 50 characters each.
  - `created_at` is set by the server in UTC, RFC 3339.
- CLI:
  - `add --title T [--tag X]... [--file PATH]` reads content from the file, or from stdin if `--file` is omitted or is `-`.
  - `get ID` prints the document as JSON.
  - `delete ID` removes it.
- HTTP:
  - `POST /documents` takes a JSON body with `title`, `content` and `tags`, and returns 201 with the created document. Request bodies are limited to 2 MiB.
  - `GET /documents/{id}` returns the document.
  - `DELETE /documents/{id}` returns 204.
- Errors:
  - Validation failures give CLI exit 2 and HTTP 400.
  - An unknown or non-numeric ID gives CLI exit 1 (unknown ID) or 2 (non-numeric ID), and HTTP 404 (unknown ID) or 400 (non-numeric ID).
  - There is no edit operation.
- Acceptance:
  (a) Add via CLI with `--file`, and via stdin, each return an ID, and `get` returns identical title, content and normalized tags.
  (b) `--tag " Go "` is stored as `go`. A tag containing a comma, a missing or whitespace-only title, a 201-character title, and content over 1 MiB each fail with exit 2 (CLI) and 400 (HTTP), and nothing is persisted.
  (c) `delete` of an existing ID succeeds. A subsequent `get` fails (exit 1 CLI, 404 HTTP). Deleting an unknown ID fails the same way. `GET /documents/abc` returns 400.

**REQ-3: List and search**
- Behavior of `list`:
  - CLI `list` and HTTP `GET /documents` without filters return all documents, newest first (by `created_at`, ties broken by higher ID).
  - Output is a JSON array including content. CLI `list` additionally accepts `--summary`, which omits content.
- Behavior of `search` and filters:
  - CLI `search [--tag X]... [--q TEXT]` and HTTP `GET /documents?tag=X&tag=Y&q=TEXT` use identical semantics.
  - Tags match case-insensitively and exactly. Multiple tags combine with AND.
  - `q` is a case-insensitive substring match over title and body, and it is trimmed first.
  - Tag and text filters combine with AND. Results are newest first, with no ranking.
  - Zero matches return `[]` (CLI exit 0, HTTP 200).
- Invalid filters:
  - A filter that is supplied but empty or whitespace-only (`--tag ""`, `--q "  "`, `?tag=`, `?q=`) is invalid, as is a tag filter containing a comma.
  - Invalid filters give CLI exit 2 with a message on stderr, and HTTP 400. Both interfaces behave the same way.
  - `search` with no filters at all is also invalid (CLI exit 2). Use `list` instead.
- Acceptance:
  (a) Given documents tagged `[go,cli]`, `[go]`, and `[cli]`, `--tag go --tag cli` returns only the first. `--tag GO` returns the first two.
  (b) `q=HeLLo` matches a document whose body contains "hello" and one whose title contains "HELLO". Combining `q` and `tag` returns only documents satisfying both.
  (c) Results are in descending `created_at`, with ties in descending ID.
  (d) `--tag ""`, `?tag=`, `?q=%20`, and `?tag=a,b` each give exit 2 (CLI) and 400 (HTTP). A tag filter with no matching documents returns `[]` with exit 0 / 200.

**REQ-4: HTTP server and local-only exposure**
- `serve [--addr HOST:PORT]` starts the server. The default is `127.0.0.1:8080`. There is no authentication and no web UI.
- Allowed `--addr` hosts:
  - A literal loopback IP (anything in 127.0.0.0/8, or `::1` written as `[::1]:PORT`), or the literal name `localhost`. `localhost` binds to 127.0.0.1 only.
  - Anything else makes `serve` exit 2 at startup, before listening. This includes an empty host (`:8080`), `0.0.0.0`, `::`, non-loopback IPs, and any other hostname (no DNS resolution is performed).
- Host header check on every request, as a DNS-rebinding defense:
  - Take the Host value and parse it, stripping any `:port` and the brackets around IPv6.
  - The allowed hosts are `localhost`, `127.0.0.1` and `::1`, compared case-insensitively. Any port is accepted.
  - A missing or empty Host (including HTTP/1.0 requests without one), or any other value, gets 403.
- Other behavior:
  - Responses are `application/json`, and errors use the body `{"error": "..."}`.
  - Unsupported methods on a known path return 405.
  - The server shuts down gracefully on SIGINT, finishing in-flight requests.
- Acceptance:
  (a) With the default address, a request with `Host: localhost:8080`, `127.0.0.1:8080`, or `[::1]:8080` returns 200, and the same requests with `Host: evil.example`, `Host: localhost.evil.example`, an empty Host, or no Host header return 403.
  (b) `serve --addr :8080`, `0.0.0.0:8080`, `[::]:8080`, `192.168.1.5:8080` and `example.com:8080` each exit 2 without binding. `--addr localhost:8080` and `--addr [::1]:8080` start successfully, and `localhost` listens on 127.0.0.1 only.
  (c) `PUT /documents` returns 405. An error response parses as JSON with an `error` string.
  (d) Sending SIGINT during a slow request lets that request complete before exit 0.

**REQ-5: CLI conventions and output**
- A single binary with subcommands `add`, `list`, `get`, `search`, `delete` and `serve`. Each subcommand accepts `--data` and `--lock-timeout`.
- `help`, no subcommand, or an unknown subcommand prints usage to stderr. An unknown subcommand and a missing subcommand exit 2, and `help` exits 0.
- Output and exit codes:
  - Successful output goes to stdout as JSON, and diagnostics go to stderr.
  - Exit codes: 0 success, 1 runtime failure (not found, I/O, lock, corrupt data), 2 usage or validation error.
- Acceptance:
  (a) `docs list | <JSON parser>` succeeds and stderr is empty on success.
  (b) An unknown subcommand and a missing required flag each exit 2 with usage or an error on stderr.
  (c) The binary builds with `go build` using only the standard library (`go.mod` has no `require` lines), and `go vet ./...` passes.

## Non-functional requirements

- **Portability:** works on Windows, macOS and Linux with the Go standard library only. Windows rename behavior is covered by REQ-1.
- **Durability:** a crash at any point leaves either the old or the new complete data file, never a partial one. Lock-file leftovers are recoverable as described in REQ-1.
- **Performance (reference targets, not hard gates):** on local disk with a 1,000-document, about 1 MB fixture, the median of 20 runs of the following is under 1 s: `list`, `search` (CLI process time) and `GET /documents?q=...` (server). A single `add` or `delete` is under 2 s at the median. Results are reported with hardware and antivirus context. Larger data sets are not guaranteed. Reads return all matching documents with content and there is no pagination.
- **Security:** localhost-only exposure per REQ-4. The tool never executes document content and never serves it as HTML (`Content-Type` is always JSON).
- **Testability:** all behaviors above can be tested with `go test`, using `httptest` and temp directories. Timeouts are configurable via `--lock-timeout` so failure paths run quickly.
- **Maintainability:** one storage package shared by the CLI and HTTP layers, with no feature logic duplicated.

## Assumptions

- Both ship in a single binary with subcommands (e.g. `add`, `list`, `search`, `serve`), sharing one storage layer and the same JSON file; HTTP exposes a JSON REST API (POST/GET /documents, GET /documents?tag=&q=) with no web UI.
- Case-insensitive substring match over title and body; tags are case-insensitive exact matches, multiple tags combine with AND; tag and text filters combine with AND; results sorted by newest first, no ranking.
- Include add, list, get (by ID), search, and delete; no edit in v1. Content comes from a file path or stdin; documents get auto-generated incrementing IDs, with title required and tags optional.
- Mutex-guarded in-memory store with atomic write (temp file + rename) to `./docs.json` (overridable via flag/env); HTTP server binds to 127.0.0.1:8080 by default with no authentication.

Interpretation of the assumptions, consistent with the requirements above:
- The in-memory store is a per-process working copy of the file, refreshed on change and reloaded under the cross-process lock before each mutation (REQ-1). The file is the source of truth.
- `GET /documents?tag=&q=` in the first assumption shows the filter parameter names. As specified in REQ-3, an empty value for either is rejected.
- `GET /documents/{id}` and `DELETE /documents/{id}` are the HTTP forms of get and delete.

## Out of scope

- Editing or updating documents.
- Authentication, TLS, multi-user access, or any non-loopback network exposure.
- A web UI.
- Ranked, fuzzy or tokenized search, OR-tag semantics, and tag prefix matching.
- Pagination, sorting options, and import or export formats.
- Database backends, encryption of the data file, and automatic backups.
- Rendering markdown.
- Hard performance guarantees beyond the reference targets for data sets over 1,000 documents.