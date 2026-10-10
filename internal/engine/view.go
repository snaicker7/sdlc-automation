package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

type PhaseView struct {
	ID, Phase, Scope, Status, Step, LastError string
	Loops                                     int
	Required                                  bool
}

type QuestionView struct {
	ID, Phase, Text, Default, Status, Answer, ResolvedBy string
	Seq, Priority                                        int
}

type GateView struct {
	ID, Phase, Scope, Type, Role, Status, Kind, Content, Critique string
	Version                                                       int
	Blocking, Notes                                               []string // parsed critic output
	Loops                                                         int
}

type ArtifactView struct {
	Phase, Scope, Kind, Content, CreatedBy string
	Version                                int
	Hash                                   string
}

type EventView struct {
	At            time.Time
	Type, Actor   string
	Phase, Detail string
}

type MRView struct {
	ID                               int
	Ticket, Branch, Status, MergeSHA string
}

type LinkView struct{ Kind, Ref, URL string }

type RunView struct {
	Links     []LinkView
	Run       Run
	Phases    []PhaseView
	Questions []QuestionView
	Gates     []GateView // waiting gates, with the artifact under review
	Events    []EventView
	Issues    []Issue
	MRs       []MRView
	Artifacts []ArtifactView
	Agents    int
}

func (e *Engine) ListRuns(ctx context.Context) ([]Run, error) {
	rows, err := e.DB.Query(ctx, `SELECT id::text FROM run ORDER BY created_at DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var out []Run
	for _, id := range ids {
		if r, err := e.loadRun(ctx, id); err == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

func (e *Engine) View(ctx context.Context, runID string) (*RunView, error) {
	r, err := e.loadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	v := &RunView{Run: r}

	rows, err := e.DB.Query(ctx, `SELECT pr.id::text, pr.phase::text, pr.scope, pr.status::text, COALESCE(pr.current_step::text,''), pr.loops, pr.required, COALESCE(pr.last_error,'')
		FROM phase_run pr LEFT JOIN jira_issue ji ON ji.key=pr.scope WHERE pr.run_id=$1 ORDER BY COALESCE(ji.seq,0), pr.phase`, runID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p PhaseView
		rows.Scan(&p.ID, &p.Phase, &p.Scope, &p.Status, &p.Step, &p.Loops, &p.Required, &p.LastError)
		v.Phases = append(v.Phases, p)
	}
	rows.Close()

	rows, err = e.DB.Query(ctx, `SELECT q.id::text, pr.phase::text, q.seq, q.priority, q.text, COALESCE(q.proposed_default,''), q.status::text, COALESCE(q.answer,''), COALESCE(q.resolved_by,'')
		FROM question q JOIN phase_run pr ON pr.id=q.phase_run_id WHERE pr.run_id=$1 ORDER BY pr.phase, q.seq`, runID)
	if err == nil {
		for rows.Next() {
			var q QuestionView
			rows.Scan(&q.ID, &q.Phase, &q.Seq, &q.Priority, &q.Text, &q.Default, &q.Status, &q.Answer, &q.ResolvedBy)
			v.Questions = append(v.Questions, q)
		}
		rows.Close()
	}

	grows, err := e.DB.Query(ctx, `SELECT g.id::text, pr.id::text, pr.phase::text, pr.scope, g.gate_type, g.required_role, g.status::text, a.kind, a.version, a.storage_uri
		FROM gate g JOIN phase_run pr ON pr.id=g.phase_run_id JOIN artifact a ON a.id=g.artifact_id
		WHERE pr.run_id=$1 AND g.status='waiting' ORDER BY g.opened_at`, runID)
	if err == nil {
		type gr struct {
			g       GateView
			pr, uri string
		}
		var list []gr
		for grows.Next() {
			var x gr
			grows.Scan(&x.g.ID, &x.pr, &x.g.Phase, &x.g.Scope, &x.g.Type, &x.g.Role, &x.g.Status, &x.g.Kind, &x.g.Version, &x.uri)
			list = append(list, x)
		}
		grows.Close()
		for _, x := range list {
			x.g.Content = readFile(x.uri)
			if _, c, err := e.readArtifact(ctx, `a.phase_run_id=$1 AND a.kind='critique.json'`, x.pr); err == nil {
				x.g.Critique = c
				var cj struct{ Blocking, Notes []string }
				if json.Unmarshal([]byte(c), &cj) == nil {
					x.g.Blocking, x.g.Notes = cj.Blocking, cj.Notes
				}
			}
			e.DB.QueryRow(ctx, `SELECT loops FROM phase_run WHERE id=$1`, x.pr).Scan(&x.g.Loops)
			v.Gates = append(v.Gates, x.g)
		}
	}

	erows, err := e.DB.Query(ctx, `SELECT e.occurred_at, e.type, e.actor, COALESCE(pr.phase::text||CASE WHEN pr.scope<>'' THEN ':'||pr.scope ELSE '' END,''), e.payload::text
		FROM event e LEFT JOIN phase_run pr ON pr.id=e.phase_run_id WHERE e.run_id=$1 ORDER BY e.id DESC LIMIT 60`, runID)
	if err == nil {
		for erows.Next() {
			var ev EventView
			erows.Scan(&ev.At, &ev.Type, &ev.Actor, &ev.Phase, &ev.Detail)
			v.Events = append(v.Events, ev)
		}
		erows.Close()
	}

	irows, err := e.DB.Query(ctx, `SELECT key FROM jira_issue WHERE run_id=$1 ORDER BY kind, seq`, runID)
	if err == nil {
		var keys []string
		for irows.Next() {
			var k string
			irows.Scan(&k)
			keys = append(keys, k)
		}
		irows.Close()
		for _, k := range keys {
			if i, err := e.loadIssue(ctx, k); err == nil {
				v.Issues = append(v.Issues, i)
			}
		}
	}

	mrows, err := e.DB.Query(ctx, `SELECT id, ticket_key, branch, status, COALESCE(merge_sha,'') FROM merge_request WHERE run_id=$1 ORDER BY id`, runID)
	if err == nil {
		for mrows.Next() {
			var m MRView
			mrows.Scan(&m.ID, &m.Ticket, &m.Branch, &m.Status, &m.MergeSHA)
			v.MRs = append(v.MRs, m)
		}
		mrows.Close()
	}
	arows, err := e.DB.Query(ctx, `SELECT DISTINCT ON (a.phase_run_id, a.kind) pr.phase::text, pr.scope, a.kind, a.version, a.storage_uri, a.created_by, a.content_hash
		FROM artifact a JOIN phase_run pr ON pr.id=a.phase_run_id LEFT JOIN jira_issue ji ON ji.key=pr.scope
		WHERE pr.run_id=$1 ORDER BY a.phase_run_id, a.kind, a.version DESC`, runID)
	if err == nil {
		type ar struct {
			a   ArtifactView
			uri string
		}
		var list []ar
		for arows.Next() {
			var x ar
			arows.Scan(&x.a.Phase, &x.a.Scope, &x.a.Kind, &x.a.Version, &x.uri, &x.a.CreatedBy, &x.a.Hash)
			list = append(list, x)
		}
		arows.Close()
		for _, x := range list {
			x.a.Content = readFile(x.uri)
			if len(x.a.Hash) > 12 {
				x.a.Hash = x.a.Hash[:12]
			}
			v.Artifacts = append(v.Artifacts, x.a)
		}
	}
	if lrows, err := e.DB.Query(ctx, `SELECT kind, ref, url FROM external_link WHERE run_id=$1 ORDER BY kind, ref`, runID); err == nil {
		for lrows.Next() {
			var l LinkView
			lrows.Scan(&l.Kind, &l.Ref, &l.URL)
			v.Links = append(v.Links, l)
		}
		lrows.Close()
	}
	order := map[string]int{"prd": 0, "add": 1, "repo_breakdown": 2, "jira_creation": 3, "workspace": 4, "ticket_dev": 5, "testing": 6, "mr_creation": 7, "post_mr": 8}
	secondary := func(k string) int { // critic output and raw review data after the main document
		if k == "critique.json" || k == "review.json" {
			return 1
		}
		return 0
	}
	sort.SliceStable(v.Artifacts, func(i, j int) bool {
		a, b := v.Artifacts[i], v.Artifacts[j]
		if (a.Scope == "") != (b.Scope == "") { // planning phases first
			return a.Scope == ""
		}
		if a.Scope != b.Scope {
			return a.Scope < b.Scope
		}
		if order[a.Phase] != order[b.Phase] {
			return order[a.Phase] < order[b.Phase]
		}
		return secondary(a.Kind) < secondary(b.Kind)
	})
	e.DB.QueryRow(ctx, `SELECT count(*) FROM agent_invocation ai JOIN phase_run pr ON pr.id=ai.phase_run_id WHERE pr.run_id=$1`, runID).Scan(&v.Agents)
	return v, nil
}

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// Trace renders the traceability matrix: requirement -> ADD -> story -> commit -> MR -> merge.
func (e *Engine) Trace(ctx context.Context, runID string) (string, error) {
	rows, err := e.DB.Query(ctx, `
		SELECT r.from_ref,
		       COALESCE(string_agg(DISTINCT a.to_ref, ','),''),
		       s.to_ref,
		       COALESCE((SELECT string_agg(DISTINCT t.to_ref, ',') FROM trace_link t WHERE t.run_id=r.run_id AND t.from_ref=s.to_ref AND t.to_type='commit'),''),
		       COALESCE((SELECT string_agg(DISTINCT t.to_ref, ',') FROM trace_link t WHERE t.run_id=r.run_id AND t.from_ref=s.to_ref AND t.to_type='mr'),''),
		       COALESCE((SELECT string_agg(DISTINCT t.to_ref, ',') FROM trace_link t WHERE t.run_id=r.run_id AND t.from_ref=s.to_ref AND t.to_type='merge'),'')
		FROM trace_link r
		LEFT JOIN trace_link a ON a.run_id=r.run_id AND a.from_ref=r.from_ref AND a.to_type='add_section'
		JOIN trace_link s ON s.run_id=r.run_id AND s.from_ref=r.from_ref AND s.to_type='jira_story'
		WHERE r.run_id=$1 AND r.from_type='prd_req' AND r.to_type='jira_story' AND r.to_ref=s.to_ref
		GROUP BY r.run_id, r.from_ref, s.to_ref ORDER BY r.from_ref, s.to_ref`, runID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-7s %-14s %-9s %-9s %-8s %s\n", "REQ", "ADD", "STORY", "COMMIT", "MR", "MERGE")
	for rows.Next() {
		var req, add, story, commit, mr, merge string
		rows.Scan(&req, &add, &story, &commit, &mr, &merge)
		fmt.Fprintf(&sb, "%-7s %-14s %-9s %-9s %-8s %s\n", req, add, story, commit, mr, merge)
	}
	return sb.String(), nil
}
