# Repo rules (enforced by the review bot)
- Go standard library only. No third-party modules.
- Wrap errors with fmt.Errorf("...: %w", err); never discard errors.
- Pass context.Context as the first parameter of anything that does I/O or can block.
- No goroutine without a clear shutdown path.
- Every behaviour change ships with a test; run `go vet ./...` and `go test ./...` before finishing.
- Keep it small and readable; no speculative abstractions.
- Do not run git commands; the orchestrator commits.
