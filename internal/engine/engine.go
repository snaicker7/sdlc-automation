// Package engine is the deterministic orchestrator: run/phase state machine,
// ambiguity ledger, gates, job queue, and the per-phase definitions.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdlc/internal/agent"
	"sdlc/internal/integrations"
	"sdlc/migrations"
)

const skillVersion = "0.1.0"

// ErrFatal marks a failure that must not be retried (budget exceeded, bad input).
var ErrFatal = errors.New("fatal")

type dbq interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type Engine struct {
	DB    *pgxpool.Pool
	Agent agent.Runner
	Root  string // data root: artifacts, workspaces, event sink
	Log   *slog.Logger
	Int   *integrations.Set // nil/empty = local stand-ins
}

type Run struct {
	ID, Title, Initiator, Team, Idea, Status, Size string
	BudgetUSD, SpentUSD                            float64
	MaxStories                                     int
}

type PhaseRun struct {
	ID, RunID, Phase, Scope, Status, Step, Feedback string
	Loops                                           int
	Required                                        bool
}

// Migrate applies embedded SQL migrations in order, once each.
func (e *Engine) Migrate(ctx context.Context) error {
	if _, err := e.DB.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migration (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	ents, err := migrations.FS.ReadDir(".")
	if err != nil {
		return err
	}
	var names []string
	for _, en := range ents {
		if strings.HasSuffix(en.Name(), ".sql") {
			names = append(names, en.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		var done bool
		if err := e.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migration WHERE name=$1)`, n).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sqlb, _ := migrations.FS.ReadFile(n)
		tx, err := e.DB.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sqlb)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", n, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migration(name) VALUES($1)`, n); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		e.Log.Info("applied migration", "name", n)
	}
	return nil
}

// emit writes an event and its outbox row in the caller's transaction.
func (e *Engine) emit(ctx context.Context, q dbq, runID, prID, typ, actor string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	repo, _ := payload["repo"].(string)
	b, _ := json.Marshal(payload)
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO event(run_id, phase_run_id, type, actor, team, repo, payload)
		VALUES (NULLIF($1,'')::uuid, NULLIF($2,'')::uuid, $3, $4,
		        (SELECT team FROM run WHERE id = NULLIF($1,'')::uuid), NULLIF($5,''), $6::jsonb)
		RETURNING id`, runID, prID, typ, actor, repo, string(b)).Scan(&id)
	if err != nil {
		return fmt.Errorf("emit %s: %w", typ, err)
	}
	_, err = q.Exec(ctx, `INSERT INTO outbox(event_id) VALUES($1)`, id)
	return err
}

// enqueue schedules the next step of a phase and marks it in progress.
func (e *Engine) enqueue(ctx context.Context, q dbq, prID, step string) error {
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM job WHERE phase_run_id=$1`, prID).Scan(&n); err != nil {
		return err
	}
	key := fmt.Sprintf("%s:%s:%d", prID, step, n+1)
	if _, err := q.Exec(ctx, `INSERT INTO job(phase_run_id, step, idempotency_key) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, prID, step, key); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `UPDATE phase_run SET status='in_progress', current_step=$2 WHERE id=$1`, prID, step)
	return err
}

func (e *Engine) loadRun(ctx context.Context, id string) (Run, error) {
	var r Run
	err := e.DB.QueryRow(ctx, `SELECT id::text, title, initiator, team, idea, status::text, size::text,
		budget_usd::float8, spent_usd::float8, max_stories FROM run WHERE id=$1`, id).
		Scan(&r.ID, &r.Title, &r.Initiator, &r.Team, &r.Idea, &r.Status, &r.Size, &r.BudgetUSD, &r.SpentUSD, &r.MaxStories)
	return r, err
}

func (e *Engine) loadPhase(ctx context.Context, id string) (PhaseRun, error) {
	var p PhaseRun
	err := e.DB.QueryRow(ctx, `SELECT id::text, run_id::text, phase::text, scope, status::text,
		COALESCE(current_step::text,''), feedback, loops, required FROM phase_run WHERE id=$1`, id).
		Scan(&p.ID, &p.RunID, &p.Phase, &p.Scope, &p.Status, &p.Step, &p.Feedback, &p.Loops, &p.Required)
	return p, err
}

// ---------------------------------------------------------------- artifacts

type Artifact struct {
	ID, Kind, Hash, URI string
	Version             int
}

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func safe(s string) string { return strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(s) }

// Env bundles what a phase handler needs; it also tracks artifact lineage.
type Env struct {
	E      *Engine
	Run    Run
	PR     PhaseRun
	Def    *PhaseDef
	Ticket *Issue
	used   []string // artifact IDs read during this step -> artifact_input lineage
}

func (env *Env) RunDir() string { return filepath.Join(env.E.Root, "runs", env.Run.ID) }

// Save stores a new immutable version of an artifact for this phase run.
func (env *Env) Save(ctx context.Context, kind, content string) (string, error) {
	var v int
	if err := env.E.DB.QueryRow(ctx, `SELECT COALESCE(max(version),0)+1 FROM artifact WHERE phase_run_id=$1 AND kind=$2`, env.PR.ID, kind).Scan(&v); err != nil {
		return "", err
	}
	scope := env.PR.Phase
	if env.PR.Scope != "" {
		scope += "_" + safe(env.PR.Scope)
	}
	dir := filepath.Join(env.RunDir(), "artifacts", scope)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	uri := filepath.Join(dir, fmt.Sprintf("%s.v%d", kind, v))
	if err := os.WriteFile(uri, []byte(content), 0o644); err != nil {
		return "", err
	}
	var id string
	err := env.E.DB.QueryRow(ctx, `INSERT INTO artifact(phase_run_id, kind, version, content_hash, storage_uri, schema_valid, created_by)
		VALUES($1,$2,$3,$4,$5,false,$6) RETURNING id::text`,
		env.PR.ID, kind, v, sha(content), uri, "agent:"+env.PR.Phase+"@"+skillVersion).Scan(&id)
	if err != nil {
		return "", err
	}
	for _, in := range env.used {
		env.E.DB.Exec(ctx, `INSERT INTO artifact_input VALUES($1,$2) ON CONFLICT DO NOTHING`, id, in)
	}
	env.E.emit(ctx, env.E.DB, env.Run.ID, env.PR.ID, "artifact_created", "orchestrator",
		map[string]any{"kind": kind, "version": v, "hash": sha(content)[:12]})
	return id, nil
}

func (e *Engine) readArtifact(ctx context.Context, where string, args ...any) (Artifact, string, error) {
	var a Artifact
	err := e.DB.QueryRow(ctx, `SELECT a.id::text, a.kind, a.content_hash, a.storage_uri, a.version FROM artifact a
		JOIN phase_run pr ON pr.id=a.phase_run_id WHERE `+where+` ORDER BY a.version DESC LIMIT 1`, args...).
		Scan(&a.ID, &a.Kind, &a.Hash, &a.URI, &a.Version)
	if err != nil {
		return a, "", err
	}
	b, err := os.ReadFile(a.URI)
	return a, string(b), err
}

// Own returns the latest version of an artifact of this phase run ("" if none).
func (env *Env) Own(ctx context.Context, kind string) (Artifact, string) {
	a, c, err := env.E.readArtifact(ctx, `a.phase_run_id=$1 AND a.kind=$2`, env.PR.ID, kind)
	if err != nil {
		return Artifact{}, ""
	}
	return a, c
}

// Doc returns the latest artifact from an upstream (run-level) phase and records lineage.
func (env *Env) Doc(ctx context.Context, phase, kind string) string {
	a, c, err := env.E.readArtifact(ctx, `pr.run_id=$1 AND pr.phase=$2::phase_key AND pr.scope='' AND a.kind=$3`, env.Run.ID, phase, kind)
	if err != nil {
		return ""
	}
	env.used = append(env.used, a.ID)
	return c
}

// TicketDoc returns the latest artifact from another phase of the same ticket.
func (env *Env) TicketDoc(ctx context.Context, phase, kind string) string {
	a, c, err := env.E.readArtifact(ctx, `pr.run_id=$1 AND pr.phase=$2::phase_key AND pr.scope=$3 AND a.kind=$4`, env.Run.ID, phase, env.PR.Scope, kind)
	if err != nil {
		return ""
	}
	env.used = append(env.used, a.ID)
	return c
}

// Ledger renders resolved questions/assumptions of this run's planning phases for prompts.
func (env *Env) Ledger(ctx context.Context) string {
	rows, err := env.E.DB.Query(ctx, `SELECT pr.phase::text, q.text, q.status::text, COALESCE(q.answer,'')
		FROM question q JOIN phase_run pr ON pr.id=q.phase_run_id
		WHERE pr.run_id=$1 AND q.status IN ('answered','accepted_assumption') ORDER BY pr.phase, q.seq`, env.Run.ID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var ph, t, st, a string
		rows.Scan(&ph, &t, &st, &a)
		label := "ANSWER"
		if st == "accepted_assumption" {
			label = "ACCEPTED ASSUMPTION"
		}
		fmt.Fprintf(&sb, "- [%s] Q: %s\n  %s: %s\n", ph, t, label, a)
	}
	return sb.String()
}

// Ask runs an agent, records cost/audit, and enforces the run budget.
func (env *Env) Ask(ctx context.Context, role string, tier agent.Tier, system, prompt, dir string, tools []string) (string, error) {
	var spent, budget float64
	env.E.DB.QueryRow(ctx, `SELECT spent_usd::float8, budget_usd::float8 FROM run WHERE id=$1`, env.Run.ID).Scan(&spent, &budget)
	if spent >= budget {
		env.E.emit(ctx, env.E.DB, env.Run.ID, env.PR.ID, "budget_exceeded", "orchestrator", map[string]any{"spent": spent, "budget": budget})
		return "", fmt.Errorf("%w: run budget $%.2f exhausted (spent $%.2f)", ErrFatal, budget, spent)
	}
	res, err := env.E.Agent.Run(ctx, agent.Request{Role: role, Tier: tier, System: system, Prompt: prompt, Dir: dir, Tools: tools})
	if err != nil {
		return "", err
	}
	tr := ""
	if res.Transcript != "" {
		td := filepath.Join(env.RunDir(), "transcripts")
		os.MkdirAll(td, 0o755)
		tr = filepath.Join(td, fmt.Sprintf("%s_%s_%d.json", safe(role), safe(env.PR.Phase+env.PR.Scope), len(res.Transcript)))
		os.WriteFile(tr, []byte(res.Transcript), 0o644)
	}
	env.E.DB.Exec(ctx, `INSERT INTO agent_invocation(phase_run_id, step, skill, skill_version, model, input_tokens, output_tokens, cost_usd, transcript_uri, finished_at)
		VALUES($1,$2::step_key,$3,$4,$5,$6,$7,$8,NULLIF($9,''),now())`,
		env.PR.ID, orDefault(env.PR.Step, "generate"), role, skillVersion, res.Model, res.InTokens, res.OutTokens, res.CostUSD, tr)
	env.E.DB.Exec(ctx, `UPDATE run SET spent_usd = spent_usd + $2, updated_at=now() WHERE id=$1`, env.Run.ID, res.CostUSD)
	env.E.emit(ctx, env.E.DB, env.Run.ID, env.PR.ID, "agent_invoked", "agent:"+role+"@"+skillVersion,
		map[string]any{"model": res.Model, "cost_usd": res.CostUSD, "in": res.InTokens, "out": res.OutTokens})
	return res.Text, nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
