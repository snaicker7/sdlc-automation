-- Real integrations: external links (Confluence pages, Jira issues, GitHub PRs) and PR metadata.

CREATE TABLE external_link (
  run_id  uuid NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  kind    text NOT NULL,            -- 'confluence' | 'jira' | 'pr'
  ref     text NOT NULL,            -- artifact kind / issue key / ticket key
  ext_id  text NOT NULL DEFAULT '', -- Confluence page id, PR number, ...
  url     text NOT NULL,
  PRIMARY KEY (run_id, kind, ref)
);

ALTER TABLE merge_request ADD COLUMN pr_number int;
ALTER TABLE merge_request ADD COLUMN pr_url text;

-- Makes issue creation idempotent across retries (epic has seq 0).
CREATE UNIQUE INDEX jira_issue_run_kind_seq ON jira_issue(run_id, kind, seq);
