-- SDLC automation orchestrator: core schema (Postgres 15+)
-- Principles:
--   * Orchestrator is deterministic code; this DB is the single source of truth.
--   * Artifacts are immutable + content-hashed; approvals bind to a hash.
--   * Work is claimed with SELECT ... FOR UPDATE SKIP LOCKED (no external queue).
--   * Events go through a transactional outbox, relayed to Kafka.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TYPE phase_key AS ENUM (
  'prd','add','repo_breakdown','jira_creation','workspace',
  'ticket_dev','testing','mr_creation','post_mr'
);

CREATE TYPE phase_status AS ENUM (
  'pending','in_progress','awaiting_human','complete','blocked','stale','skipped'
);

-- Step inside a phase; drives the agent loop.
CREATE TYPE step_key AS ENUM ('clarify','generate','critique','validate','gate','publish');

CREATE TYPE change_size AS ENUM ('trivial','standard','large');
CREATE TYPE run_status AS ENUM ('active','completed','cancelled','failed','escape_hatch');

-- ---------------------------------------------------------------- runs
CREATE TABLE run (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  title          text NOT NULL,
  initiator      text NOT NULL,              -- user id
  team           text NOT NULL,
  jira_epic_key  text,
  size           change_size NOT NULL DEFAULT 'standard',
  status         run_status NOT NULL DEFAULT 'active',
  budget_usd     numeric(10,2) NOT NULL DEFAULT 50,
  spent_usd      numeric(10,2) NOT NULL DEFAULT 0,
  escape_reason  text,                       -- required when status = escape_hatch
  escape_approver text,
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  CHECK (status <> 'escape_hatch' OR (escape_reason IS NOT NULL AND escape_approver IS NOT NULL))
);

-- ---------------------------------------------------------------- phases
-- One row per (run, phase, scope). scope = '' for run-level phases,
-- or a ticket/repo key for fan-out phases (ticket_dev, testing, mr_creation, post_mr).
CREATE TABLE phase_run (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id        uuid NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  phase         phase_key NOT NULL,
  scope         text NOT NULL DEFAULT '',
  status        phase_status NOT NULL DEFAULT 'pending',
  current_step  step_key,
  attempt       int NOT NULL DEFAULT 0,
  required      boolean NOT NULL DEFAULT true,   -- set by size classifier
  started_at    timestamptz,
  completed_at  timestamptz,
  UNIQUE (run_id, phase, scope)
);

-- Declared dependencies between phase instances (for staleness + ordering).
CREATE TABLE phase_dependency (
  phase_run_id  uuid NOT NULL REFERENCES phase_run(id) ON DELETE CASCADE,
  depends_on_id uuid NOT NULL REFERENCES phase_run(id) ON DELETE CASCADE,
  PRIMARY KEY (phase_run_id, depends_on_id),
  CHECK (phase_run_id <> depends_on_id)
);

-- ---------------------------------------------------------------- artifacts
CREATE TABLE artifact (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  phase_run_id  uuid NOT NULL REFERENCES phase_run(id) ON DELETE CASCADE,
  kind          text NOT NULL,               -- 'prd.md','add.md','add.json','jira_preview.json',...
  version       int  NOT NULL,
  content_hash  text NOT NULL,               -- sha256 hex of content
  storage_uri   text NOT NULL,               -- git path or object store URI
  schema_valid  boolean NOT NULL DEFAULT false,
  created_by    text NOT NULL,               -- 'agent:<name>@<skill_version>' or user id
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (phase_run_id, kind, version)
);

-- Which artifact versions an artifact was derived from (traceability + staleness).
CREATE TABLE artifact_input (
  artifact_id        uuid NOT NULL REFERENCES artifact(id) ON DELETE CASCADE,
  input_artifact_id  uuid NOT NULL REFERENCES artifact(id),
  PRIMARY KEY (artifact_id, input_artifact_id)
);

-- ---------------------------------------------------------------- ambiguity ledger
CREATE TYPE question_status AS ENUM ('open','answered','accepted_assumption','dropped');

CREATE TABLE question (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  phase_run_id     uuid NOT NULL REFERENCES phase_run(id) ON DELETE CASCADE,
  seq              int  NOT NULL,
  priority         smallint NOT NULL DEFAULT 2,   -- 1 blocking, 2 important, 3 nice-to-know
  text             text NOT NULL,
  proposed_default text,
  status           question_status NOT NULL DEFAULT 'open',
  answer           text,
  resolved_by      text,
  resolved_at      timestamptz,
  written_to_artifact_id uuid REFERENCES artifact(id),  -- where the assumption/answer landed
  UNIQUE (phase_run_id, seq),
  CHECK (status = 'open' OR (resolved_by IS NOT NULL AND resolved_at IS NOT NULL)),
  CHECK (status <> 'accepted_assumption' OR proposed_default IS NOT NULL)
);

-- ---------------------------------------------------------------- gates + approvals
CREATE TYPE gate_status AS ENUM ('waiting','approved','rejected','superseded');

CREATE TABLE gate (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  phase_run_id      uuid NOT NULL REFERENCES phase_run(id) ON DELETE CASCADE,
  gate_type         text NOT NULL,              -- 'pm_approval','tech_lead','plan_approval','diff_review',...
  required_role     text NOT NULL,
  artifact_id       uuid NOT NULL REFERENCES artifact(id),  -- the exact version under review
  status            gate_status NOT NULL DEFAULT 'waiting',
  opened_at         timestamptz NOT NULL DEFAULT now(),
  sla_due_at        timestamptz,
  last_escalated_at timestamptz
);

-- Opening a gate requires zero open questions (enforced in trigger below).
CREATE TABLE approval (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  gate_id       uuid NOT NULL REFERENCES gate(id) ON DELETE CASCADE,
  approver      text NOT NULL,
  decision      text NOT NULL CHECK (decision IN ('approve','reject','request_changes')),
  artifact_hash text NOT NULL,                  -- hash approved; must equal artifact.content_hash
  comment       text,
  decided_at    timestamptz NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION enforce_gate_open() RETURNS trigger AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM question q
             WHERE q.phase_run_id = NEW.phase_run_id AND q.status = 'open') THEN
    RAISE EXCEPTION 'cannot open gate: unresolved questions in ledger';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;

CREATE TRIGGER gate_requires_clean_ledger
  BEFORE INSERT ON gate FOR EACH ROW EXECUTE FUNCTION enforce_gate_open();

CREATE OR REPLACE FUNCTION enforce_approval_hash() RETURNS trigger AS $$
DECLARE h text;
BEGIN
  SELECT a.content_hash INTO h FROM gate g JOIN artifact a ON a.id = g.artifact_id
   WHERE g.id = NEW.gate_id;
  IF NEW.decision = 'approve' AND NEW.artifact_hash IS DISTINCT FROM h THEN
    RAISE EXCEPTION 'approval hash does not match gated artifact (stale review)';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;

CREATE TRIGGER approval_hash_check
  BEFORE INSERT ON approval FOR EACH ROW EXECUTE FUNCTION enforce_approval_hash();

-- ---------------------------------------------------------------- work queue
CREATE TYPE job_status AS ENUM ('queued','running','succeeded','failed','dead');

CREATE TABLE job (
  id            bigserial PRIMARY KEY,
  phase_run_id  uuid NOT NULL REFERENCES phase_run(id) ON DELETE CASCADE,
  step          step_key NOT NULL,
  status        job_status NOT NULL DEFAULT 'queued',
  run_after     timestamptz NOT NULL DEFAULT now(),
  attempts      int NOT NULL DEFAULT 0,
  max_attempts  int NOT NULL DEFAULT 3,
  locked_by     text,
  locked_until  timestamptz,                    -- lease; expired leases are reclaimed
  last_error    text,
  idempotency_key text NOT NULL UNIQUE,          -- e.g. '<phase_run_id>:<step>:<attempt>'
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX job_claim_idx ON job (run_after) WHERE status = 'queued';

-- Claim query (worker):
--   UPDATE job SET status='running', locked_by=$1, locked_until=now()+interval '10 min', attempts=attempts+1
--   WHERE id = (SELECT id FROM job
--               WHERE (status='queued' AND run_after<=now())
--                  OR (status='running' AND locked_until<now())
--               ORDER BY run_after FOR UPDATE SKIP LOCKED LIMIT 1)
--   RETURNING *;

-- ---------------------------------------------------------------- agent invocations (cost + audit)
CREATE TABLE agent_invocation (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  phase_run_id  uuid NOT NULL REFERENCES phase_run(id) ON DELETE CASCADE,
  step          step_key NOT NULL,
  skill         text NOT NULL,
  skill_version text NOT NULL,
  model         text NOT NULL,
  input_tokens  int NOT NULL DEFAULT 0,
  output_tokens int NOT NULL DEFAULT 0,
  cost_usd      numeric(10,4) NOT NULL DEFAULT 0,
  transcript_uri text,
  started_at    timestamptz NOT NULL DEFAULT now(),
  finished_at   timestamptz
);

-- ---------------------------------------------------------------- traceability
CREATE TABLE trace_link (
  run_id     uuid NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  from_type  text NOT NULL,   -- 'prd_req','add_section','jira_story','commit','test','mr'
  from_ref   text NOT NULL,
  to_type    text NOT NULL,
  to_ref     text NOT NULL,
  PRIMARY KEY (run_id, from_type, from_ref, to_type, to_ref)
);

-- ---------------------------------------------------------------- events + outbox
CREATE TABLE event (
  id          bigserial PRIMARY KEY,
  run_id      uuid REFERENCES run(id),
  phase_run_id uuid REFERENCES phase_run(id),
  type        text NOT NULL,                    -- see events/event.schema.json
  actor       text NOT NULL,
  team        text,
  repo        text,
  payload     jsonb NOT NULL DEFAULT '{}',
  occurred_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX event_run_idx ON event (run_id, occurred_at);
CREATE INDEX event_type_idx ON event (type, occurred_at);

-- Written in the same transaction as the state change; relay publishes to Kafka.
CREATE TABLE outbox (
  id           bigserial PRIMARY KEY,
  event_id     bigint NOT NULL REFERENCES event(id),
  topic        text NOT NULL DEFAULT 'sdlc.events.v1',
  published_at timestamptz
);
CREATE INDEX outbox_pending_idx ON outbox (id) WHERE published_at IS NULL;

-- ---------------------------------------------------------------- CI enforcement view
-- The required CI check calls this: is run-id valid and fully approved for this MR?
CREATE VIEW run_merge_eligibility AS
SELECT r.id AS run_id,
       r.status,
       bool_and(p.status IN ('complete','skipped') OR NOT p.required) AS all_required_phases_done,
       NOT EXISTS (
         SELECT 1 FROM gate g JOIN phase_run pr ON pr.id = g.phase_run_id
         WHERE pr.run_id = r.id AND g.status <> 'approved' AND g.status <> 'superseded'
       ) AS all_gates_approved
FROM run r JOIN phase_run p ON p.run_id = r.id
GROUP BY r.id, r.status;
