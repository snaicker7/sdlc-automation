package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"sdlc/internal/agent"
	"sdlc/internal/integrations"
)

type GateDef struct{ Type, Role string }

// PhaseDef is the phase contract. The orchestrator drives
// clarify -> generate -> critique -> validate -> gate -> publish for every phase;
// each phase only supplies the pieces it needs.
type PhaseDef struct {
	Key      string
	Clarify  bool
	Primary  string // artifact kind reviewed by the critic and presented at the gate
	Critique bool
	MaxLoops int
	Gate     *GateDef
	Generate func(context.Context, *Env) error
	Validate func(context.Context, *Env) error
	Publish  func(context.Context, *Env) error
}

type Issue struct {
	Key, Kind, Summary, Description, Repo, Status string
	Acceptance, Reqs                              []string
	Estimate, Seq                                 int
}

const sysBase = "You are one role in an automated SDLC pipeline. Do exactly what is asked. No preamble, no commentary outside the requested format."

var reqRe = regexp.MustCompile(`REQ-\d+`)

func reqIDs(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range reqRe.FindAllString(s, -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

func stripFence(s string) string {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "```") {
		if i := strings.Index(t, "\n"); i >= 0 {
			t = t[i+1:]
		}
		t = strings.TrimSuffix(strings.TrimSpace(t), "```")
	}
	return strings.TrimSpace(t)
}

func withFeedback(prompt, feedback string) string {
	if feedback == "" {
		return prompt
	}
	return prompt + "\n\nREVISION FEEDBACK (address every point):\n" + feedback
}

func getDef(key string) *PhaseDef {
	switch key {
	case "prd":
		return &PhaseDef{Key: key, Clarify: true, Primary: "prd.md", Critique: true, MaxLoops: 2,
			Gate:     &GateDef{"pm_approval", "product_manager"},
			Generate: docGen("prd-writer", agent.TierStrong, "prd.md", prdPrompt),
			Validate: validatePRD, Publish: publishDoc("prd.md")}
	case "add":
		return &PhaseDef{Key: key, Clarify: true, Primary: "add.md", Critique: true, MaxLoops: 2,
			Gate:     &GateDef{"tech_lead_approval", "tech_lead"},
			Generate: docGen("add-writer", agent.TierStrong, "add.md", addPrompt),
			Validate: validateADD, Publish: publishADD}
	case "repo_breakdown":
		return &PhaseDef{Key: key, Primary: "breakdown.json", Critique: true, MaxLoops: 2,
			Gate:     &GateDef{"tech_lead_approval", "tech_lead"},
			Generate: docGen("breakdown-writer", agent.TierFast, "breakdown.json", breakdownPrompt),
			Validate: validateBreakdown}
	case "jira_creation":
		return &PhaseDef{Key: key, Primary: "jira_preview.json", Critique: true, MaxLoops: 2,
			Gate:     &GateDef{"jira_preview_approval", "tech_lead"},
			Generate: docGen("jira-writer", agent.TierFast, "jira_preview.json", jiraPrompt),
			Validate: validateJira, Publish: publishJira}
	case "workspace":
		return &PhaseDef{Key: key, Primary: "smoke.txt", MaxLoops: 1,
			Generate: genWorkspace, Validate: validateResult("smoke.txt")}
	case "ticket_dev":
		return &PhaseDef{Key: key, Primary: "plan.md", Critique: true, MaxLoops: 2,
			Gate:     &GateDef{"plan_approval", "developer"},
			Generate: genPlan, Validate: validateNonEmpty("plan.md"), Publish: publishImplement}
	case "testing":
		return &PhaseDef{Key: key, Primary: "test_report.txt", MaxLoops: 3,
			Gate:     &GateDef{"dev_signoff", "developer"},
			Generate: genTests, Validate: validateResult("test_report.txt")}
	case "mr_creation":
		return &PhaseDef{Key: key, Primary: "mr.md", MaxLoops: 2,
			Gate:     &GateDef{"dev_submit", "developer"},
			Generate: genMR, Validate: validateMR, Publish: publishMR}
	case "post_mr":
		return &PhaseDef{Key: key, Primary: "post_mr_report.md", MaxLoops: 2,
			Gate:     &GateDef{"reviewer_approval", "reviewer"},
			Generate: genPostMR, Validate: validateReview, Publish: publishMerge}
	}
	return nil
}

// ---------------------------------------------------------------- generic helpers

func docGen(role string, tier agent.Tier, kind string, build func(context.Context, *Env) string) func(context.Context, *Env) error {
	return func(ctx context.Context, env *Env) error {
		out, err := env.Ask(ctx, role, tier, sysBase, withFeedback(build(ctx, env), env.PR.Feedback), "", nil)
		if err != nil {
			return err
		}
		out = stripFence(out)
		if _, err := env.Save(ctx, kind, out); err != nil {
			return err
		}
		return env.setFeedback(ctx, "")
	}
}

func (env *Env) setFeedback(ctx context.Context, fb string) error {
	env.PR.Feedback = fb
	_, err := env.E.DB.Exec(ctx, `UPDATE phase_run SET feedback=$2 WHERE id=$1`, env.PR.ID, fb)
	return err
}

func (env *Env) Trace(ctx context.Context, ft, fr, tt, tr string) {
	env.E.DB.Exec(ctx, `INSERT INTO trace_link VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, env.Run.ID, ft, fr, tt, tr)
}

func publishDoc(kind string) func(context.Context, *Env) error {
	return func(ctx context.Context, env *Env) error {
		_, c := env.Own(ctx, kind)
		dir := filepath.Join(env.RunDir(), "confluence")
		os.MkdirAll(dir, 0o755)
		if err := os.WriteFile(filepath.Join(dir, kind), []byte(c), 0o644); err != nil {
			return err
		}
		prefix := map[string]string{"prd.md": "PRD", "add.md": "ADD"}[kind]
		if prefix == "" {
			return nil
		}
		return env.publishConfluence(ctx, kind, prefix+": "+env.Run.Title)
	}
}

func validateNonEmpty(kind string) func(context.Context, *Env) error {
	return func(ctx context.Context, env *Env) error {
		if _, c := env.Own(ctx, kind); len(strings.TrimSpace(c)) < 40 {
			return fmt.Errorf("%s is empty or too short", kind)
		}
		return nil
	}
}

// validateResult checks the "RESULT: PASS" first line of tool-generated reports.
func validateResult(kind string) func(context.Context, *Env) error {
	return func(ctx context.Context, env *Env) error {
		_, c := env.Own(ctx, kind)
		if !strings.HasPrefix(c, "RESULT: PASS") {
			return fmt.Errorf("%s did not pass:\n%s", kind, trunc(c, 3000))
		}
		return nil
	}
}

func (env *Env) loadTicket(ctx context.Context) error {
	if env.PR.Scope == "" {
		return nil
	}
	i, err := env.E.loadIssue(ctx, env.PR.Scope)
	env.Ticket = &i
	return err
}

func (e *Engine) loadIssue(ctx context.Context, key string) (Issue, error) {
	var i Issue
	var acc []byte
	err := e.DB.QueryRow(ctx, `SELECT key, kind, summary, description, COALESCE(repo,''), status, acceptance, reqs, COALESCE(estimate,0), seq
		FROM jira_issue WHERE key=$1`, key).Scan(&i.Key, &i.Kind, &i.Summary, &i.Description, &i.Repo, &i.Status, &acc, &i.Reqs, &i.Estimate, &i.Seq)
	if err == nil {
		json.Unmarshal(acc, &i.Acceptance)
	}
	return i, err
}

func (i Issue) text() string {
	return fmt.Sprintf("TICKET: %s\nSummary: %s\nRepo: %s\nRequirements: %s\nDescription: %s\nAcceptance criteria:\n- %s",
		i.Key, i.Summary, i.Repo, strings.Join(i.Reqs, ", "), i.Description, strings.Join(i.Acceptance, "\n- "))
}

// ---------------------------------------------------------------- PRD / ADD / breakdown / Jira

func prdPrompt(ctx context.Context, env *Env) string {
	return `Write a PRD in Markdown for the idea below.

IDEA: ` + env.Run.Idea + `

RESOLVED QUESTIONS AND ASSUMPTIONS:
` + env.Ledger(ctx) + `
Rules:
- Sections: Problem, Goals, Requirements, Non-functional requirements, Assumptions, Out of scope.
- Number requirements REQ-1, REQ-2, ... (at most 5, right-sized for the idea). Each requirement must contain an explicit "Acceptance:" criterion that is testable.
- The Assumptions section must reproduce every ACCEPTED ASSUMPTION above verbatim.
- Output only the Markdown document.`
}

func addPrompt(ctx context.Context, env *Env) string {
	return `Write an Architecture Design Document (ADD) in Markdown that satisfies the approved PRD.

PRD:
` + env.Doc(ctx, "prd", "prd.md") + `

RESOLVED QUESTIONS AND ASSUMPTIONS:
` + env.Ledger(ctx) + `
REPO RULES THE IMPLEMENTATION MUST FOLLOW:
` + scaffoldFiles("x")["CLAUDE.md"] + `
Rules:
- Sections: Context, Components, one "## REQ-n <title>" design section for EVERY requirement in the PRD, Data model, Failure modes, Testing strategy, Rollout.
- Be concrete (packages, types, endpoints/CLI commands, storage format) but right-sized; no speculative infrastructure.
- Output only the Markdown document.`
}

func breakdownPrompt(ctx context.Context, env *Env) string {
	return `Split the ADD into repositories. Prefer the fewest repos that make sense (one repo for a single small service).

ADD:
` + env.Doc(ctx, "add", "add.md") + `
Return ONLY JSON: {"repos":[{"name":"kebab-case-name","summary":"...","contracts_first":false,"depends_on":[]}]}`
}

func jiraPrompt(ctx context.Context, env *Env) string {
	return fmt.Sprintf(`Create an epic and user stories for the work below.

PRD:
%s
ADD:
%s
REPOS:
%s
Rules:
- At most %d stories, ordered so that earlier stories never depend on later ones; each story independently mergeable with tests.
- Every REQ-n in the PRD must be covered by at least one story (list them in "reqs").
- "repo" must be one of the repo names above. Each story needs >=1 testable acceptance criterion and an estimate in points.
Return ONLY JSON:
{"epic":{"summary":"..."},"stories":[{"summary":"...","repo":"...","estimate":3,"reqs":["REQ-1"],"acceptance":["..."],"description":"..."}]}`,
		env.Doc(ctx, "prd", "prd.md"), env.Doc(ctx, "add", "add.md"), env.Doc(ctx, "repo_breakdown", "breakdown.json"), env.Run.MaxStories)
}

func validatePRD(ctx context.Context, env *Env) error {
	_, c := env.Own(ctx, "prd.md")
	ids := reqIDs(c)
	if len(ids) == 0 {
		return fmt.Errorf("PRD has no REQ-n requirements")
	}
	if n := strings.Count(strings.ToLower(c), "acceptance"); n < len(ids) {
		return fmt.Errorf("PRD has %d requirements but only %d acceptance criteria", len(ids), n)
	}
	return nil
}

func validateADD(ctx context.Context, env *Env) error {
	_, c := env.Own(ctx, "add.md")
	var missing []string
	for _, id := range reqIDs(env.Doc(ctx, "prd", "prd.md")) {
		if !strings.Contains(c, id) {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("ADD does not address requirements: %s", strings.Join(missing, ", "))
	}
	return nil
}

func publishADD(ctx context.Context, env *Env) error {
	if err := publishDoc("add.md")(ctx, env); err != nil {
		return err
	}
	_, c := env.Own(ctx, "add.md")
	for _, id := range reqIDs(c) {
		env.Trace(ctx, "prd_req", id, "add_section", "add.md#"+id)
	}
	return nil
}

type breakdown struct {
	Repos []struct {
		Name           string   `json:"name"`
		Summary        string   `json:"summary"`
		ContractsFirst bool     `json:"contracts_first"`
		DependsOn      []string `json:"depends_on"`
	} `json:"repos"`
}

var repoNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (env *Env) breakdown(ctx context.Context) (breakdown, error) {
	var b breakdown
	err := agent.ExtractJSON(env.Doc(ctx, "repo_breakdown", "breakdown.json"), &b)
	return b, err
}

func validateBreakdown(ctx context.Context, env *Env) error {
	_, c := env.Own(ctx, "breakdown.json")
	var b breakdown
	if err := agent.ExtractJSON(c, &b); err != nil || len(b.Repos) == 0 {
		return fmt.Errorf("breakdown.json must be JSON with at least one repo: %v", err)
	}
	for _, r := range b.Repos {
		if !repoNameRe.MatchString(r.Name) {
			return fmt.Errorf("invalid repo name %q (kebab-case required)", r.Name)
		}
	}
	return nil
}

type jiraPreview struct {
	Epic struct {
		Summary string `json:"summary"`
	} `json:"epic"`
	Stories []struct {
		Summary     string   `json:"summary"`
		Repo        string   `json:"repo"`
		Estimate    int      `json:"estimate"`
		Reqs        []string `json:"reqs"`
		Acceptance  []string `json:"acceptance"`
		Description string   `json:"description"`
	} `json:"stories"`
}

func validateJira(ctx context.Context, env *Env) error {
	_, c := env.Own(ctx, "jira_preview.json")
	var p jiraPreview
	if err := agent.ExtractJSON(c, &p); err != nil {
		return fmt.Errorf("jira_preview.json invalid: %w", err)
	}
	if len(p.Stories) == 0 || len(p.Stories) > env.Run.MaxStories {
		return fmt.Errorf("need 1..%d stories, got %d", env.Run.MaxStories, len(p.Stories))
	}
	repos := map[string]bool{}
	if b, err := env.breakdown(ctx); err == nil {
		for _, r := range b.Repos {
			repos[r.Name] = true
		}
	}
	covered := map[string]bool{}
	for i, s := range p.Stories {
		if strings.TrimSpace(s.Summary) == "" || len(s.Acceptance) == 0 {
			return fmt.Errorf("story %d needs a summary and acceptance criteria", i+1)
		}
		if !repos[s.Repo] {
			return fmt.Errorf("story %d has unknown repo %q", i+1, s.Repo)
		}
		for _, r := range s.Reqs {
			covered[r] = true
		}
	}
	for _, id := range reqIDs(env.Doc(ctx, "prd", "prd.md")) {
		if !covered[id] {
			return fmt.Errorf("requirement %s is not covered by any story", id)
		}
	}
	return nil
}

type issueSpec struct {
	Kind, Summary, Description, Parent, Repo, PRDLink string
	Seq, Estimate                                     int
	Reqs, Acceptance                                  []string
}

// ensureIssue creates one issue (in real Jira when configured, else the local stand-in) and
// records it immediately, so a retry after a partial failure never creates duplicates.
func (env *Env) ensureIssue(ctx context.Context, s issueSpec) (string, error) {
	var key string
	if err := env.E.DB.QueryRow(ctx, `SELECT key FROM jira_issue WHERE run_id=$1 AND kind=$2 AND seq=$3`, env.Run.ID, s.Kind, s.Seq).Scan(&key); err == nil {
		return key, nil
	}
	if env.E.jiraOn() {
		blocks := []integrations.Block{integrations.P(s.Description)}
		if len(s.Acceptance) > 0 {
			blocks = append(blocks, integrations.H(3, "Acceptance criteria"), integrations.UL(s.Acceptance))
		}
		var meta []string
		if s.Estimate > 0 {
			meta = append(meta, fmt.Sprintf("Estimate: %d pts", s.Estimate))
		}
		if len(s.Reqs) > 0 {
			meta = append(meta, "Covers: "+strings.Join(s.Reqs, ", "))
		}
		if s.Repo != "" {
			meta = append(meta, "Repo: "+s.Repo)
		}
		if s.PRDLink != "" {
			meta = append(meta, "PRD: "+s.PRDLink)
		}
		meta = append(meta, "SDLC run: "+env.Run.ID)
		blocks = append(blocks, integrations.UL(meta))
		typ := "Story"
		if s.Kind == "epic" {
			typ = "Epic"
		}
		out, err := env.E.Int.Jira.CreateIssue(ctx, integrations.IssueIn{Type: typ, Summary: s.Summary, Parent: s.Parent,
			Description: blocks, Labels: []string{"sdlc-" + env.Run.ID[:8]}})
		if err != nil {
			return "", fmt.Errorf("jira create %s %q: %w", typ, s.Summary, err)
		}
		key = out.Key
		env.link(ctx, "jira", key, "", out.URL)
	} else {
		var n int
		env.E.DB.QueryRow(ctx, `SELECT nextval('jira_key_seq')`).Scan(&n)
		key = fmt.Sprintf("DEMO-%d", n)
	}
	acc, _ := json.Marshal(s.Acceptance)
	if s.Acceptance == nil {
		acc = []byte("[]")
	}
	reqs := s.Reqs
	if reqs == nil {
		reqs = []string{}
	}
	_, err := env.E.DB.Exec(ctx, `INSERT INTO jira_issue(key,run_id,kind,seq,summary,description,acceptance,estimate,repo,reqs)
		VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,NULLIF($9,''),$10)`,
		key, env.Run.ID, s.Kind, s.Seq, s.Summary, s.Description, string(acc), s.Estimate, s.Repo, reqs)
	return key, err
}

// publishJira creates the epic and stories, then fans the run out into per-ticket phases:
// ticket_dev -> testing -> mr_creation -> post_mr. Tickets are chained so each branches from
// main after the previous one merged.
func publishJira(ctx context.Context, env *Env) error {
	_, c := env.Own(ctx, "jira_preview.json")
	var p jiraPreview
	if err := agent.ExtractJSON(c, &p); err != nil {
		return err
	}
	_, prdLink, _ := env.linkGet(ctx, "confluence", "prd.md")
	epic, err := env.ensureIssue(ctx, issueSpec{Kind: "epic", Summary: p.Epic.Summary, Description: env.Run.Idea, PRDLink: prdLink})
	if err != nil {
		return err
	}
	env.E.DB.Exec(ctx, `UPDATE run SET jira_epic_key=$2 WHERE id=$1`, env.Run.ID, epic)

	var keys []string
	for i, s := range p.Stories {
		key, err := env.ensureIssue(ctx, issueSpec{Kind: "story", Seq: i + 1, Summary: s.Summary, Description: s.Description, Parent: epic,
			Repo: s.Repo, PRDLink: prdLink, Estimate: s.Estimate, Reqs: s.Reqs, Acceptance: s.Acceptance})
		if err != nil {
			return err
		}
		keys = append(keys, key)
		for _, r := range s.Reqs {
			env.Trace(ctx, "prd_req", r, "jira_story", key)
		}
	}

	tx, err := env.E.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var prev string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM phase_run WHERE run_id=$1 AND phase='workspace'`, env.Run.ID).Scan(&prev); err != nil {
		return err
	}
	for _, key := range keys {
		for _, ph := range []string{"ticket_dev", "testing", "mr_creation", "post_mr"} {
			var id string
			if err := tx.QueryRow(ctx, `INSERT INTO phase_run(run_id, phase, scope) VALUES($1,$2::phase_key,$3)
				ON CONFLICT (run_id, phase, scope) DO UPDATE SET scope=EXCLUDED.scope RETURNING id::text`, env.Run.ID, ph, key).Scan(&id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO phase_dependency VALUES($1,$2) ON CONFLICT DO NOTHING`, id, prev); err != nil {
				return err
			}
			prev = id
		}
	}
	if err := env.E.emit(ctx, tx, env.Run.ID, env.PR.ID, "jira_created", "orchestrator", map[string]any{"epic": epic, "stories": keys}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------- workspace

func (env *Env) repoNames(ctx context.Context) []string {
	b, err := env.breakdown(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range b.Repos {
		out = append(out, r.Name)
	}
	return out
}

func genWorkspace(ctx context.Context, env *Env) error {
	var sb strings.Builder
	result := "PASS"
	for _, name := range env.repoNames(ctx) {
		rep, err := env.ensureRepo(ctx, name)
		fmt.Fprintf(&sb, "== repo %s ==\n%s\n", name, rep)
		if err != nil {
			result = "FAIL"
			fmt.Fprintf(&sb, "ERROR: %v\n", err)
		}
	}
	_, err := env.Save(ctx, "smoke.txt", "RESULT: "+result+"\n"+sb.String())
	return err
}

// ---------------------------------------------------------------- per-ticket dev

func genPlan(ctx context.Context, env *Env) error {
	t := env.Ticket
	prompt := withFeedback(fmt.Sprintf(`Propose an implementation plan for this ticket. Inspect the repo (read-only) to see what already exists.

%s

ADD:
%s
Plan format (Markdown): Files to add/change; Tests first (TDD) - name the test cases; Implementation steps; Risks.
Keep it short and concrete. Output only the plan.`, t.text(), env.Doc(ctx, "add", "add.md")), env.PR.Feedback)
	out, err := env.Ask(ctx, "planner", agent.TierStrong, sysBase, prompt, env.RepoDir(t.Repo), []string{"Read", "Glob", "Grep"})
	if err != nil {
		return err
	}
	if _, err := env.Save(ctx, "plan.md", stripFence(out)); err != nil {
		return err
	}
	return env.setFeedback(ctx, "")
}

// publishImplement runs after the plan is approved: implement with TDD in a git worktree.
func publishImplement(ctx context.Context, env *Env) error {
	t := env.Ticket
	wt, err := env.ensureWorktree(ctx, t.Repo, t.Key)
	if err != nil {
		return err
	}
	env.transition(ctx, t.Key, "In Progress")
	_, plan := env.Own(ctx, "plan.md")
	prompt := fmt.Sprintf(`Implement this ticket using TDD, following the approved plan. Follow CLAUDE.md in this repo.
Write failing tests first, then the code, then run "go vet ./..." and "go test ./..." until both pass.
Do not run git commands. Standard library only.

%s

APPROVED PLAN:
%s`, t.text(), plan)
	if _, err := env.Ask(ctx, "implementer", agent.TierCode, sysBase, prompt, wt,
		[]string{"Read", "Edit", "Write", "Glob", "Grep", "Bash(go *)"}); err != nil {
		return err
	}
	sha, err := env.commitAll(ctx, wt, fmt.Sprintf("%s: %s", t.Key, t.Summary))
	if err != nil {
		return err
	}
	diff, _ := git(ctx, wt, "diff", "main...HEAD")
	if _, err := env.Save(ctx, "diff.patch", diff); err != nil {
		return err
	}
	env.Trace(ctx, "jira_story", t.Key, "commit", sha)
	return nil
}

func genTests(ctx context.Context, env *Env) error {
	t := env.Ticket
	wt := env.WorktreeDir(t.Repo, t.Key)
	if env.PR.Feedback != "" { // previous run failed: let the dev agent fix it
		prompt := fmt.Sprintf(`Vet/tests are failing. Fix the code (and tests if they are wrong) so "go vet ./..." and "go test ./..." pass. Do not run git commands.

%s

FAILURE OUTPUT:
%s`, t.text(), env.PR.Feedback)
		if _, err := env.Ask(ctx, "fixer", agent.TierCode, sysBase, prompt, wt, []string{"Read", "Edit", "Write", "Glob", "Grep", "Bash(go *)"}); err != nil {
			return err
		}
		if _, err := env.commitAll(ctx, wt, fmt.Sprintf("%s: fix failing checks", t.Key)); err != nil {
			return err
		}
		env.setFeedback(ctx, "")
	}
	var sb strings.Builder
	result := "PASS"
	for _, step := range [][]string{{"vet", "./..."}, {"test", "./...", "-count=1", "-v"}} {
		out, err := sh(ctx, wt, "go", step...)
		fmt.Fprintf(&sb, "$ go %s\n%s\n", strings.Join(step, " "), trunc(out, 4000))
		if err != nil {
			result = "FAIL"
		}
	}
	stat, _ := git(ctx, wt, "diff", "--stat", "main...HEAD")
	_, err := env.Save(ctx, "test_report.txt", "RESULT: "+result+"\n(unit tests; contract/integration/e2e suites plug in here)\n\n"+sb.String()+"\n"+stat)
	return err
}

func genMR(ctx context.Context, env *Env) error {
	t := env.Ticket
	report := env.TicketDoc(ctx, "testing", "test_report.txt")
	prompt := withFeedback(fmt.Sprintf(`Write a merge request description in Markdown: Summary, What changed, How it was tested, Risk and rollback. Be brief and factual.

%s

APPROVED PLAN:
%s
TEST REPORT:
%s`, t.text(), env.TicketDoc(ctx, "ticket_dev", "plan.md"), trunc(report, 3000)), env.PR.Feedback)
	out, err := env.Ask(ctx, "mr-writer", agent.TierFast, sysBase, prompt, "", nil)
	if err != nil {
		return err
	}
	footer := fmt.Sprintf("\n\n---\nJira: %s\nTraces: %s\nsdlc-run-id: %s\n", t.Key, strings.Join(t.Reqs, ", "), env.Run.ID)
	if _, err := env.Save(ctx, "mr.md", stripFence(out)+footer); err != nil {
		return err
	}
	return env.setFeedback(ctx, "")
}

func validateMR(ctx context.Context, env *Env) error {
	if _, c := env.Own(ctx, "mr.md"); !strings.Contains(c, "sdlc-run-id: "+env.Run.ID) {
		return fmt.Errorf("MR description is missing the sdlc-run-id footer")
	}
	return nil
}

func publishMR(ctx context.Context, env *Env) error {
	t := env.Ticket
	_, desc := env.Own(ctx, "mr.md")
	title := t.Key + ": " + t.Summary
	var id int
	if err := env.E.DB.QueryRow(ctx, `SELECT id FROM merge_request WHERE run_id=$1 AND ticket_key=$2`, env.Run.ID, t.Key).Scan(&id); err != nil {
		err := env.E.DB.QueryRow(ctx, `INSERT INTO merge_request(run_id, ticket_key, repo, branch, title, description)
			VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, env.Run.ID, t.Key, t.Repo, "feat/"+t.Key, title, desc).Scan(&id)
		if err != nil {
			return err
		}
	}
	label := fmt.Sprintf("MR !%d", id)
	if env.E.ghOn() { // real GitHub: push the branch and open a pull request
		wt := env.WorktreeDir(t.Repo, t.Key)
		if err := env.ensureRemoteMain(ctx, env.RepoDir(t.Repo)); err != nil { // a PR needs a base branch on the remote
			return err
		}
		if out, err := env.gitNet(ctx, wt, "push", "-u", "origin", "feat/"+t.Key); err != nil {
			return fmt.Errorf("push branch: %w\n%s", err, out)
		}
		pr, err := env.E.Int.GH.CreatePR(ctx, env.E.Int.GH.RepoName(t.Repo), "feat/"+t.Key, "main", title, desc)
		if err != nil {
			return fmt.Errorf("open pull request: %w", err)
		}
		env.E.DB.Exec(ctx, `UPDATE merge_request SET pr_number=$2, pr_url=$3 WHERE id=$1`, id, pr.Number, pr.URL)
		env.link(ctx, "pr", t.Key, fmt.Sprint(pr.Number), pr.URL)
		label = fmt.Sprintf("PR #%d", pr.Number)
	}
	env.Trace(ctx, "jira_story", t.Key, "mr", label)
	env.E.DB.Exec(ctx, `UPDATE jira_issue SET status='In Review' WHERE key=$1`, t.Key)
	env.transition(ctx, t.Key, "In Review", "Code Review", "In Progress")
	return env.E.emit(ctx, env.E.DB, env.Run.ID, env.PR.ID, "mr_created", "orchestrator", map[string]any{"mr": id, "ticket": t.Key, "repo": t.Repo, "label": label})
}

// ---------------------------------------------------------------- post-MR: scans + review bot

type finding struct {
	Severity   string `json:"severity"`
	File       string `json:"file"`
	Summary    string `json:"summary"`
	Suggestion string `json:"suggestion"`
}

func genPostMR(ctx context.Context, env *Env) error {
	t := env.Ticket
	wt := env.WorktreeDir(t.Repo, t.Key)
	if env.PR.Feedback != "" {
		prompt := fmt.Sprintf(`Fix these blocking code-review findings. Keep changes minimal, follow CLAUDE.md, then run "go vet ./..." and "go test ./...". Do not run git commands.

%s

FINDINGS:
%s`, t.text(), env.PR.Feedback)
		if _, err := env.Ask(ctx, "fixer", agent.TierCode, sysBase, prompt, wt, []string{"Read", "Edit", "Write", "Glob", "Grep", "Bash(go *)"}); err != nil {
			return err
		}
		if _, err := env.commitAll(ctx, wt, fmt.Sprintf("%s: address review findings", t.Key)); err != nil {
			return err
		}
		if env.E.ghOn() { // keep the open pull request in sync with the fix
			if out, err := env.gitNet(ctx, wt, "push", "origin", "feat/"+t.Key); err != nil {
				return fmt.Errorf("push review fixes: %w\n%s", err, out)
			}
		}
		env.setFeedback(ctx, "")
	}
	scans := strings.Join([]string{
		"go vet: " + toolOutput(ctx, wt, "go", "vet", "./..."),
		"gosec: " + toolOutput(ctx, wt, "gosec", "-quiet", "./..."),
		"govulncheck: " + toolOutput(ctx, wt, "govulncheck", "./..."),
		"gitleaks: " + toolOutput(ctx, wt, "gitleaks", "detect", "--no-banner", "--source", "."),
	}, "\n\n")
	diff, _ := git(ctx, wt, "diff", "main...HEAD")
	prompt := fmt.Sprintf(`You are the code review bot. Review the diff for correctness bugs, violations of the repo rules in CLAUDE.md, security problems, and missing tests. You may read files in the repo.
Triage the scanner output below too (ignore noise).
Severity: "blocking" = must fix before merge; "warning"; "info". Do not mark style nits as blocking.
Return ONLY JSON: {"findings":[{"severity":"blocking|warning|info","file":"...","summary":"...","suggestion":"..."}]}

%s

SCANNER OUTPUT:
%s
DIFF:
%s`, t.text(), scans, trunc(diff, 24000))
	out, err := env.Ask(ctx, "reviewer", agent.TierStrong, sysBase, prompt, wt, []string{"Read", "Glob", "Grep"})
	if err != nil {
		return err
	}
	var r struct {
		Findings []finding `json:"findings"`
	}
	if err := agent.ExtractJSON(out, &r); err != nil {
		return fmt.Errorf("reviewer output unparseable: %w", err)
	}
	js, _ := json.MarshalIndent(r, "", "  ")
	var md strings.Builder
	fmt.Fprintf(&md, "# Post-MR report for %s\n\n## Review findings\n", t.Key)
	if len(r.Findings) == 0 {
		md.WriteString("None.\n")
	}
	for _, f := range r.Findings {
		fmt.Fprintf(&md, "- **%s** %s %s %s\n", strings.ToUpper(f.Severity), f.File, f.Summary, f.Suggestion)
	}
	fmt.Fprintf(&md, "\n## Scanners\n```\n%s\n```\n", scans)
	if _, err := env.Save(ctx, "review.json", string(js)); err != nil {
		return err
	}
	_, err = env.Save(ctx, "post_mr_report.md", md.String())
	return err
}

func validateReview(ctx context.Context, env *Env) error {
	_, c := env.Own(ctx, "review.json")
	var r struct {
		Findings []finding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(c), &r); err != nil {
		return err
	}
	var blocking []string
	for _, f := range r.Findings {
		if strings.EqualFold(f.Severity, "blocking") {
			blocking = append(blocking, fmt.Sprintf("- %s: %s (%s)", f.File, f.Summary, f.Suggestion))
		}
	}
	if len(blocking) > 0 {
		return fmt.Errorf("%d blocking finding(s):\n%s", len(blocking), strings.Join(blocking, "\n"))
	}
	return nil
}

// publishMerge lands the approved MR on main and closes the Jira story. Locally it merges the
// branch itself. With GitHub the PR is left for a human to merge: this step polls the PR and
// waits (ErrWait) until it is merged, then fast-forwards the local main so the next ticket
// branches from it.
func publishMerge(ctx context.Context, env *Env) error {
	t := env.Ticket
	repo := env.RepoDir(t.Repo)
	var sha string
	if env.E.ghOn() {
		var n int
		if err := env.E.DB.QueryRow(ctx, `SELECT COALESCE(pr_number,0) FROM merge_request WHERE run_id=$1 AND ticket_key=$2`, env.Run.ID, t.Key).Scan(&n); err != nil || n == 0 {
			return fmt.Errorf("%w: no GitHub PR recorded for %s", ErrFatal, t.Key)
		}
		pr, err := env.E.Int.GH.GetPR(ctx, env.E.Int.GH.RepoName(t.Repo), n)
		if err != nil {
			return err
		}
		if !pr.Merged {
			if pr.State == "closed" {
				return fmt.Errorf("%w: PR #%d was closed without being merged", ErrFatal, n)
			}
			if env.PR.Status != "awaiting_human" {
				env.E.emit(ctx, env.E.DB, env.Run.ID, env.PR.ID, "waiting_for_pr_merge", "orchestrator", map[string]any{"ticket": t.Key, "pr": pr.URL})
			}
			return fmt.Errorf("%w: merge PR #%d on GitHub (%s)", ErrWait, n, pr.URL)
		}
		git(ctx, repo, "worktree", "remove", "--force", env.WorktreeDir(t.Repo, t.Key))
		if out, err := env.gitNet(ctx, repo, "fetch", "origin", "main"); err != nil {
			return fmt.Errorf("fetch: %w\n%s", err, out)
		}
		if out, err := git(ctx, repo, "merge", "--ff-only", "origin/main"); err != nil {
			return fmt.Errorf("local main could not fast-forward to origin/main: %w\n%s", err, out)
		}
		sha = env.headSHA(ctx, repo)
	} else {
		if out, err := git(ctx, repo, "merge", "--no-ff", "feat/"+t.Key, "-m", "Merge "+t.Key+": "+t.Summary); err != nil {
			return fmt.Errorf("merge failed: %w\n%s", err, out)
		}
		sha = env.headSHA(ctx, repo)
		git(ctx, repo, "worktree", "remove", "--force", env.WorktreeDir(t.Repo, t.Key))
	}
	env.E.DB.Exec(ctx, `UPDATE merge_request SET status='merged', merge_sha=$2, merged_at=now() WHERE run_id=$1 AND ticket_key=$3`, env.Run.ID, sha, t.Key)
	env.E.DB.Exec(ctx, `UPDATE jira_issue SET status='Done' WHERE key=$1`, t.Key)
	env.transition(ctx, t.Key, "Done")
	env.Trace(ctx, "jira_story", t.Key, "merge", sha)
	return env.E.emit(ctx, env.E.DB, env.Run.ID, env.PR.ID, "mr_merged", "orchestrator", map[string]any{"ticket": t.Key, "sha": sha, "repo": t.Repo})
}
