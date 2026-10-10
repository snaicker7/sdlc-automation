-- Runtime additions: run inputs, loop bookkeeping, local Jira/GitLab stand-ins.

ALTER TABLE run ADD COLUMN idea text NOT NULL DEFAULT '';
ALTER TABLE run ADD COLUMN max_stories int NOT NULL DEFAULT 2;

ALTER TABLE phase_run ADD COLUMN loops int NOT NULL DEFAULT 0;      -- critique/validate/reject loops used
ALTER TABLE phase_run ADD COLUMN feedback text NOT NULL DEFAULT ''; -- fed into the next generate
ALTER TABLE phase_run ADD COLUMN last_error text;

CREATE SEQUENCE jira_key_seq START 1;

-- Stand-in for Jira (swap for the real MCP/API integration behind the same publish hook).
CREATE TABLE jira_issue (
  key         text PRIMARY KEY,
  run_id      uuid NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  kind        text NOT NULL CHECK (kind IN ('epic','story')),
  seq         int  NOT NULL DEFAULT 0,
  summary     text NOT NULL,
  description text NOT NULL DEFAULT '',
  acceptance  jsonb NOT NULL DEFAULT '[]',
  estimate    int,
  repo        text,
  reqs        text[] NOT NULL DEFAULT '{}',
  status      text NOT NULL DEFAULT 'To Do',
  created_at  timestamptz NOT NULL DEFAULT now()
);

-- Stand-in for GitLab/GitHub merge requests.
CREATE TABLE merge_request (
  id          serial PRIMARY KEY,
  run_id      uuid NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  ticket_key  text NOT NULL REFERENCES jira_issue(key),
  repo        text NOT NULL,
  branch      text NOT NULL,
  title       text NOT NULL,
  description text NOT NULL,
  status      text NOT NULL DEFAULT 'open' CHECK (status IN ('open','merged','closed')),
  merge_sha   text,
  created_at  timestamptz NOT NULL DEFAULT now(),
  merged_at   timestamptz
);
