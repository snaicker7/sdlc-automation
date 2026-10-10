# SDLC automation orchestrator

Deterministic Go orchestrator (Postgres-backed state machine) that drives Claude agents through
PRD -> ADD -> repo breakdown -> Jira -> workspace -> per-ticket dev -> testing -> MR -> post-MR,
with human gates, an ambiguity ledger, an audit trail and traceability.

## Run it

```
go build -o sdlc.exe ./cmd/sdlc

# Unattended end-to-end demo (auto-approver stands in for humans; clearly attributed in the audit trail)
sdlc demo -agent mock              # deterministic, free, ~20s: exercises state machine, loops, gates
sdlc demo -agent claude -budget 10 # real agents build the sample "document organizer" app

# Human-in-the-loop
sdlc up -agent claude              # terminal 1: embedded Postgres + workers + dashboard on :8080
sdlc start -title "Doc organizer" -idea "..."          # terminal 2
# then answer questions / approve gates in the dashboard, or:
sdlc status <run-id>
sdlc answer <question-id> -accept        # accept the proposed default as an explicit assumption
sdlc approve <gate-id>                   # or: -reject -comment "why"
sdlc trace <run-id>
sdlc ci-check <run-id> [ticket]          # exit 1 blocks merge
```

Postgres is embedded (zonky binaries, port 5433, data in `data/pg`), so no Docker is needed. Set
`SDLC_DB` to use your own Postgres instead.

## How it works

Every phase runs the same pipeline, implemented once in `internal/engine/worker.go`:

`clarify -> generate -> critique -> validate -> gate -> publish`

| step | who | what |
|---|---|---|
| clarify | agent | raises numbered questions, each with a proposed default, into the ledger. The phase waits until none are open; a human answers or explicitly accepts the default as an assumption (a DB trigger refuses to open a gate otherwise). |
| generate | agent/tools | writes an immutable, hashed, versioned artifact (lineage recorded in `artifact_input`). |
| critique | separate agent | adversarial review; blocking findings loop back to generate (bounded). |
| validate | deterministic code | schema/coverage checks (every REQ in the ADD, every REQ covered by a story, tests pass...). |
| gate | human | approval is bound to the exact artifact hash (DB trigger). Request-changes feeds the comment back into generate. |
| publish | orchestrator | side effects (Jira issues, worktrees, merge), events, phase complete. |

Phase definitions live in `internal/engine/phases.go` (`getDef`); `phases/prd.phase.yaml` documents the contract.

State: `migrations/*.sql` (runs, phases, artifacts, ledger, gates, approvals, job queue with
`SKIP LOCKED` leases, agent invocations + cost, trace links, events + transactional outbox).
Events are relayed to `data/events.jsonl` (swap `FlushOutbox` for a Kafka producer).

Agents: `internal/agent`. `claude` drives Claude Code headless (`claude -p`, JSON output, cost
recorded per call, per-run budget enforced). Model routing by tier (strong/code/fast) via flags.
`mock` is deterministic and deliberately triggers the critique and review-fix loops.

## Real integrations (opt-in)

Copy `sdlc.env.example` to `sdlc.env` (git-ignored) and fill in credentials; run
`sdlc check-integrations` to verify them read-only. Any block you leave out stays local.

- **Confluence**: PRD and ADD are published as pages when their gate is approved.
- **Jira**: on approval of the dry-run preview, a real epic + stories are created (idempotent across
  retries), and transitioned In Progress -> In Review -> Done as the ticket moves.
- **GitHub**: repos are created (private, `<prefix><name>`) and cloned, each ticket's branch is pushed and a
  PR opened with the `sdlc-run-id` footer. The PR is left for a human to merge; the pipeline polls and
  continues once GitHub reports it merged.

Status: the code compiles and vets, but has **not yet been run against real Atlassian/GitHub accounts**.
Use a sandbox Jira project, Confluence space and GitHub owner for the first run, and run with
`-agent mock` first to test the integrations cheaply.

## Known gaps (not built yet)

- With no credentials configured, Jira/GitHub fall back to Postgres/local-git stand-ins (`jira_issue`, `merge_request`).
- Approver identity is a free-text name; no RBAC check of `required_role`, no SSO.
- No staleness propagation when an upstream artifact changes after approval (artifact lineage is recorded, the re-run logic is not).
- No gate SLA escalation job, no Slack/Jira approval buttons.
- Testing is unit tests + vet only (no testcontainers / contract / Playwright); scanners (gosec, govulncheck, gitleaks) run only if installed.
- Workspace has no docker-compose for Kafka/Cassandra/Redis (the sample app doesn't need them).
- Tickets run sequentially per run (each branches from main after the previous merge); no cross-repo contract-first ordering yet.
- Crash recovery is lease-based re-run of the step; steps are mostly but not strictly idempotent.
