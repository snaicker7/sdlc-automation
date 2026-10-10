package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sdlc/internal/agent"
)

type StartOpts struct {
	Title, Idea, Initiator, Team, Size string
	BudgetUSD                          float64
	MaxStories                         int
}

var planningPhases = []string{"prd", "add", "repo_breakdown", "jira_creation", "workspace"}

// StartRun creates a run and its run-level phases (linear dependency chain).
// The size classifier decides which phases are required: trivial changes skip PRD+ADD.
func (e *Engine) StartRun(ctx context.Context, o StartOpts) (string, error) {
	if o.Size == "" {
		o.Size = "standard"
	}
	if o.Team == "" {
		o.Team = "default"
	}
	if o.MaxStories == 0 {
		o.MaxStories = 2
	}
	if o.BudgetUSD == 0 {
		o.BudgetUSD = 25
	}
	tx, err := e.DB.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO run(title, initiator, team, size, idea, budget_usd, max_stories)
		VALUES($1,$2,$3,$4::change_size,$5,$6,$7) RETURNING id::text`,
		o.Title, o.Initiator, o.Team, o.Size, o.Idea, o.BudgetUSD, o.MaxStories).Scan(&id); err != nil {
		return "", err
	}
	prev := ""
	for _, ph := range planningPhases {
		required := !(o.Size == "trivial" && (ph == "prd" || ph == "add"))
		status := "pending"
		if !required {
			status = "skipped"
		}
		var pid string
		if err := tx.QueryRow(ctx, `INSERT INTO phase_run(run_id, phase, status, required) VALUES($1,$2::phase_key,$3::phase_status,$4) RETURNING id::text`,
			id, ph, status, required).Scan(&pid); err != nil {
			return "", err
		}
		if prev != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO phase_dependency VALUES($1,$2)`, pid, prev); err != nil {
				return "", err
			}
		}
		prev = pid
	}
	if err := e.emit(ctx, tx, id, "", "run_started", o.Initiator, map[string]any{"title": o.Title, "size": o.Size}); err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

// Tick activates pending phases whose dependencies are complete.
func (e *Engine) Tick(ctx context.Context) error {
	tx, err := e.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		SELECT pr.id::text, pr.run_id::text, pr.phase::text FROM phase_run pr
		JOIN run r ON r.id = pr.run_id
		WHERE pr.status='pending' AND r.status='active'
		  AND NOT EXISTS (SELECT 1 FROM phase_dependency d JOIN phase_run dp ON dp.id = d.depends_on_id
		                  WHERE d.phase_run_id = pr.id AND dp.status NOT IN ('complete','skipped'))
		ORDER BY pr.phase FOR UPDATE OF pr SKIP LOCKED`)
	if err != nil {
		return err
	}
	type act struct{ id, run, phase string }
	var acts []act
	for rows.Next() {
		var a act
		rows.Scan(&a.id, &a.run, &a.phase)
		acts = append(acts, a)
	}
	rows.Close()
	for _, a := range acts {
		if _, err := tx.Exec(ctx, `UPDATE phase_run SET status='in_progress', started_at=now() WHERE id=$1`, a.id); err != nil {
			return err
		}
		if err := e.enqueue(ctx, tx, a.id, "clarify"); err != nil {
			return err
		}
		if err := e.emit(ctx, tx, a.run, a.id, "phase_started", "orchestrator", map[string]any{"phase": a.phase}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type job struct {
	ID          int64
	PhaseRunID  string
	Step        string
	Attempts    int
	MaxAttempts int
}

func (e *Engine) claim(ctx context.Context, worker string) (*job, error) {
	var j job
	err := e.DB.QueryRow(ctx, `
		UPDATE job SET status='running', locked_by=$1, locked_until=now()+interval '30 minutes', attempts=attempts+1
		WHERE id = (SELECT id FROM job
		            WHERE (status='queued' AND run_after<=now()) OR (status='running' AND locked_until<now())
		            ORDER BY run_after, id FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING id, phase_run_id::text, step::text, attempts, max_attempts`, worker).
		Scan(&j.ID, &j.PhaseRunID, &j.Step, &j.Attempts, &j.MaxAttempts)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil, nil
		}
		return nil, err
	}
	return &j, nil
}

// Work is a worker loop: tick the scheduler, claim a job, run one step.
func (e *Engine) Work(ctx context.Context, name string) {
	for ctx.Err() == nil {
		j, err := e.claim(ctx, name)
		if err != nil {
			e.Log.Error("claim", "err", err)
		}
		if j == nil {
			if err := e.Tick(ctx); err != nil && ctx.Err() == nil {
				e.Log.Error("tick", "err", err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(400 * time.Millisecond):
			}
			continue
		}
		e.process(ctx, j)
	}
}

func (e *Engine) process(ctx context.Context, j *job) {
	pr, err := e.loadPhase(ctx, j.PhaseRunID)
	if err != nil {
		e.Log.Error("load phase", "err", err)
		return
	}
	run, err := e.loadRun(ctx, pr.RunID)
	if err != nil {
		return
	}
	env := &Env{E: e, Run: run, PR: pr, Def: getDef(pr.Phase)}
	env.PR.Step = j.Step
	e.Log.Info("step", "run", run.ID[:8], "phase", pr.Phase, "scope", pr.Scope, "step", j.Step, "attempt", j.Attempts)

	err = env.loadTicket(ctx)
	if err == nil && run.Status == "active" {
		err = e.step(ctx, env, j.Step)
	}
	if err == nil {
		e.DB.Exec(ctx, `UPDATE job SET status='succeeded', locked_until=NULL WHERE id=$1`, j.ID)
		return
	}
	if errors.Is(err, ErrWait) { // waiting on a human/external event: poll again, don't burn a retry
		e.DB.Exec(ctx, `UPDATE job SET status='queued', locked_until=NULL, attempts=GREATEST(attempts-1,0), run_after=now()+interval '20 seconds', last_error=$2 WHERE id=$1`, j.ID, err.Error())
		e.DB.Exec(ctx, `UPDATE phase_run SET status='awaiting_human', last_error=NULL WHERE id=$1`, pr.ID)
		return
	}
	e.Log.Warn("step failed", "phase", pr.Phase, "scope", pr.Scope, "step", j.Step, "err", firstLine(err.Error()))
	if errors.Is(err, ErrFatal) || j.Attempts >= j.MaxAttempts {
		e.DB.Exec(ctx, `UPDATE job SET status='dead', last_error=$2, locked_until=NULL WHERE id=$1`, j.ID, err.Error())
		e.DB.Exec(ctx, `UPDATE phase_run SET status='blocked', last_error=$2 WHERE id=$1`, pr.ID, err.Error())
		e.emit(ctx, e.DB, run.ID, pr.ID, "phase_blocked", "orchestrator", map[string]any{"step": j.Step, "error": trunc(err.Error(), 500)})
		return
	}
	e.DB.Exec(ctx, `UPDATE job SET status='queued', last_error=$2, locked_until=NULL, run_after=now()+($3 || ' seconds')::interval WHERE id=$1`,
		j.ID, err.Error(), fmt.Sprint(5*j.Attempts))
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// step executes one step of the clarify->generate->critique->validate->gate->publish pipeline.
func (e *Engine) step(ctx context.Context, env *Env, step string) error {
	def, pr := env.Def, env.PR
	switch step {
	case "clarify":
		if !def.Clarify {
			return e.enqueue(ctx, e.DB, pr.ID, "generate")
		}
		var n int
		e.DB.QueryRow(ctx, `SELECT count(*) FROM question WHERE phase_run_id=$1`, pr.ID).Scan(&n)
		if n == 0 {
			ctxDoc := ""
			if pr.Phase == "add" {
				ctxDoc = "APPROVED PRD:\n" + env.Doc(ctx, "prd", "prd.md") + "\n"
			}
			prompt := fmt.Sprintf(`You are about to work on the "%s" phase of a software project. Find genuine ambiguities that would change the outcome. Ask at most 4 batched questions, most important first. Each needs a proposed_default: what you would choose if nobody answers. If nothing is ambiguous, return [].
Return ONLY JSON: [{"text":"...","priority":1,"proposed_default":"..."}]

IDEA: %s
%sALREADY RESOLVED:
%s`, pr.Phase, env.Run.Idea, ctxDoc, env.Ledger(ctx))
			out, err := env.Ask(ctx, "clarifier-"+pr.Phase, agent.TierStrong, sysBase, prompt, "", nil)
			if err != nil {
				return err
			}
			var qs []struct {
				Text     string `json:"text"`
				Priority int    `json:"priority"`
				Default  string `json:"proposed_default"`
			}
			if err := agent.ExtractJSON(out, &qs); err != nil {
				return fmt.Errorf("clarifier output unparseable: %w", err)
			}
			if len(qs) > 4 {
				qs = qs[:4]
			}
			for i, q := range qs {
				if q.Priority < 1 || q.Priority > 3 {
					q.Priority = 2
				}
				if _, err := e.DB.Exec(ctx, `INSERT INTO question(phase_run_id, seq, priority, text, proposed_default) VALUES($1,$2,$3,$4,NULLIF($5,''))
					ON CONFLICT DO NOTHING`, pr.ID, i+1, q.Priority, q.Text, q.Default); err != nil {
					return err
				}
			}
			e.emit(ctx, e.DB, env.Run.ID, pr.ID, "questions_raised", "agent:clarifier@"+skillVersion, map[string]any{"count": len(qs)})
		}
		var open int
		e.DB.QueryRow(ctx, `SELECT count(*) FROM question WHERE phase_run_id=$1 AND status='open'`, pr.ID).Scan(&open)
		if open > 0 {
			_, err := e.DB.Exec(ctx, `UPDATE phase_run SET status='awaiting_human', current_step='clarify' WHERE id=$1`, pr.ID)
			return err
		}
		return e.enqueue(ctx, e.DB, pr.ID, "generate")

	case "generate":
		if err := def.Generate(ctx, env); err != nil {
			return err
		}
		if def.Critique {
			return e.enqueue(ctx, e.DB, pr.ID, "critique")
		}
		return e.enqueue(ctx, e.DB, pr.ID, "validate")

	case "critique":
		_, doc := env.Own(ctx, def.Primary)
		upstream := ""
		switch pr.Phase {
		case "add":
			upstream = "UPSTREAM PRD:\n" + env.Doc(ctx, "prd", "prd.md")
		case "repo_breakdown", "jira_creation":
			upstream = "UPSTREAM PRD:\n" + env.Doc(ctx, "prd", "prd.md") + "\nUPSTREAM ADD:\n" + env.Doc(ctx, "add", "add.md")
		case "ticket_dev":
			upstream = env.Ticket.text() + "\nUPSTREAM ADD:\n" + env.Doc(ctx, "add", "add.md")
		}
		prompt := fmt.Sprintf(`You are an adversarial reviewer. Review the document below before a human sees it.
Look for: requirements that are not testable, missing failure modes or non-functional requirements, contradictions with upstream documents, scope creep, ambiguity, unsafe or unrealistic design.
Report ONLY blocking problems (things that would lead to wrong, unsafe or untestable work). Do not nitpick.
Return ONLY JSON: {"blocking":["..."],"notes":["..."]}

KIND: %s
%s

DOCUMENT:
%s`, def.Primary, upstream, doc)
		out, err := env.Ask(ctx, "critic", agent.TierStrong, sysBase, prompt, "", nil)
		if err != nil {
			return err
		}
		var c struct {
			Blocking []string `json:"blocking"`
			Notes    []string `json:"notes"`
		}
		if err := agent.ExtractJSON(out, &c); err != nil {
			return fmt.Errorf("critic output unparseable: %w", err)
		}
		cj, _ := json.MarshalIndent(c, "", "  ")
		if _, err := env.Save(ctx, "critique.json", string(cj)); err != nil {
			return err
		}
		e.emit(ctx, e.DB, env.Run.ID, pr.ID, "critique_completed", "agent:critic@"+skillVersion, map[string]any{"blocking": len(c.Blocking), "loop": pr.Loops})
		if len(c.Blocking) > 0 && pr.Loops < def.MaxLoops {
			return e.loopBack(ctx, env, "Critic found blocking problems:\n- "+strings.Join(c.Blocking, "\n- "))
		}
		return e.enqueue(ctx, e.DB, pr.ID, "validate")

	case "validate":
		if def.Validate != nil {
			if err := def.Validate(ctx, env); err != nil {
				e.emit(ctx, e.DB, env.Run.ID, pr.ID, "validation_failed", "orchestrator", map[string]any{"error": trunc(err.Error(), 400), "loop": pr.Loops})
				if pr.Loops < def.MaxLoops {
					return e.loopBack(ctx, env, "Validation failed:\n"+err.Error())
				}
				return fmt.Errorf("%w: validation still failing after %d loops: %v", ErrFatal, pr.Loops, err)
			}
		}
		e.DB.Exec(ctx, `UPDATE artifact SET schema_valid=true WHERE id=(SELECT id FROM artifact WHERE phase_run_id=$1 AND kind=$2 ORDER BY version DESC LIMIT 1)`, pr.ID, def.Primary)
		if def.Gate != nil {
			return e.enqueue(ctx, e.DB, pr.ID, "gate")
		}
		return e.enqueue(ctx, e.DB, pr.ID, "publish")

	case "gate":
		a, _ := env.Own(ctx, def.Primary)
		var waiting int
		e.DB.QueryRow(ctx, `SELECT count(*) FROM gate WHERE phase_run_id=$1 AND status='waiting'`, pr.ID).Scan(&waiting)
		if waiting == 0 {
			if _, err := e.DB.Exec(ctx, `INSERT INTO gate(phase_run_id, gate_type, required_role, artifact_id, sla_due_at) VALUES($1,$2,$3,$4, now()+interval '48 hours')`,
				pr.ID, def.Gate.Type, def.Gate.Role, a.ID); err != nil {
				return err
			}
			e.emit(ctx, e.DB, env.Run.ID, pr.ID, "gate_opened", "orchestrator", map[string]any{"gate": def.Gate.Type, "role": def.Gate.Role, "artifact": def.Primary, "version": a.Version})
		}
		_, err := e.DB.Exec(ctx, `UPDATE phase_run SET status='awaiting_human', current_step='gate' WHERE id=$1`, pr.ID)
		return err

	case "publish":
		if def.Publish != nil {
			if err := def.Publish(ctx, env); err != nil {
				return err
			}
		}
		return e.completePhase(ctx, env)
	}
	return fmt.Errorf("%w: unknown step %q", ErrFatal, step)
}

func (e *Engine) loopBack(ctx context.Context, env *Env, feedback string) error {
	if _, err := e.DB.Exec(ctx, `UPDATE phase_run SET loops=loops+1, feedback=$2 WHERE id=$1`, env.PR.ID, feedback); err != nil {
		return err
	}
	return e.enqueue(ctx, e.DB, env.PR.ID, "generate")
}

func (e *Engine) completePhase(ctx context.Context, env *Env) error {
	tx, err := e.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE phase_run SET status='complete', completed_at=now(), current_step=NULL, last_error=NULL WHERE id=$1`, env.PR.ID); err != nil {
		return err
	}
	if err := e.emit(ctx, tx, env.Run.ID, env.PR.ID, "phase_completed", "orchestrator", map[string]any{"phase": env.PR.Phase, "scope": env.PR.Scope}); err != nil {
		return err
	}
	var remaining int
	tx.QueryRow(ctx, `SELECT count(*) FROM phase_run WHERE run_id=$1 AND status NOT IN ('complete','skipped')`, env.Run.ID).Scan(&remaining)
	if remaining == 0 {
		tx.Exec(ctx, `UPDATE run SET status='completed', updated_at=now() WHERE id=$1`, env.Run.ID)
		if err := e.emit(ctx, tx, env.Run.ID, "", "run_completed", "orchestrator", nil); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------- human actions

// ResolveQuestion answers a ledger question, or (accept=true) accepts its proposed default as
// an explicit assumption. When the last open question closes, the phase moves on.
func (e *Engine) ResolveQuestion(ctx context.Context, qid, user, answer string, accept bool) error {
	tx, err := e.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var status, def, runID, prID string
	err = tx.QueryRow(ctx, `SELECT q.status::text, COALESCE(q.proposed_default,''), pr.run_id::text, pr.id::text
		FROM question q JOIN phase_run pr ON pr.id=q.phase_run_id WHERE q.id=$1 FOR UPDATE OF q`, qid).Scan(&status, &def, &runID, &prID)
	if err != nil {
		return fmt.Errorf("question %s: %w", qid, err)
	}
	if status != "open" {
		return fmt.Errorf("question already %s", status)
	}
	newStatus, typ := "answered", "question_resolved"
	if accept {
		if def == "" {
			return errors.New("question has no proposed default to accept")
		}
		answer, newStatus, typ = def, "accepted_assumption", "assumption_accepted"
	}
	if strings.TrimSpace(answer) == "" {
		return errors.New("empty answer")
	}
	if _, err := tx.Exec(ctx, `UPDATE question SET status=$2::question_status, answer=$3, resolved_by=$4, resolved_at=now() WHERE id=$1`, qid, newStatus, answer, user); err != nil {
		return err
	}
	if err := e.emit(ctx, tx, runID, prID, typ, user, map[string]any{"question": qid}); err != nil {
		return err
	}
	var open int
	tx.QueryRow(ctx, `SELECT count(*) FROM question WHERE phase_run_id=$1 AND status='open'`, prID).Scan(&open)
	if open == 0 {
		var st, step string
		tx.QueryRow(ctx, `SELECT status::text, COALESCE(current_step::text,'') FROM phase_run WHERE id=$1 FOR UPDATE`, prID).Scan(&st, &step)
		if st == "awaiting_human" && step == "clarify" {
			if err := e.enqueue(ctx, tx, prID, "generate"); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// Decide records a gate decision bound to the exact artifact hash that was reviewed.
func (e *Engine) Decide(ctx context.Context, gateID, user, decision, comment string) error {
	tx, err := e.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var status, hash, prID, runID, gtype string
	err = tx.QueryRow(ctx, `SELECT g.status::text, a.content_hash, g.phase_run_id::text, pr.run_id::text, g.gate_type
		FROM gate g JOIN artifact a ON a.id=g.artifact_id JOIN phase_run pr ON pr.id=g.phase_run_id
		WHERE g.id=$1 FOR UPDATE OF g`, gateID).Scan(&status, &hash, &prID, &runID, &gtype)
	if err != nil {
		return fmt.Errorf("gate %s: %w", gateID, err)
	}
	if status != "waiting" {
		return fmt.Errorf("gate already %s", status)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO approval(gate_id, approver, decision, artifact_hash, comment) VALUES($1,$2,$3,$4,NULLIF($5,''))`,
		gateID, user, decision, hash, comment); err != nil {
		return err
	}
	if decision == "approve" {
		tx.Exec(ctx, `UPDATE gate SET status='approved' WHERE id=$1`, gateID)
		if err := e.emit(ctx, tx, runID, prID, "gate_approved", user, map[string]any{"gate": gtype, "hash": hash[:12]}); err != nil {
			return err
		}
		if err := e.enqueue(ctx, tx, prID, "publish"); err != nil {
			return err
		}
	} else {
		if strings.TrimSpace(comment) == "" {
			return errors.New("a comment is required when rejecting")
		}
		tx.Exec(ctx, `UPDATE gate SET status='superseded' WHERE id=$1`, gateID)
		tx.Exec(ctx, `UPDATE phase_run SET loops=0, feedback=$2 WHERE id=$1`, prID, "Reviewer "+user+" requested changes: "+comment)
		if err := e.emit(ctx, tx, runID, prID, "gate_rejected", user, map[string]any{"gate": gtype, "comment": comment}); err != nil {
			return err
		}
		if err := e.enqueue(ctx, tx, prID, "generate"); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Retry re-queues a blocked phase from its failed step (e.g. after raising the budget).
func (e *Engine) Retry(ctx context.Context, prID string) error {
	var step string
	if err := e.DB.QueryRow(ctx, `SELECT COALESCE(j.step::text,'generate') FROM phase_run pr
		LEFT JOIN LATERAL (SELECT step FROM job WHERE phase_run_id=pr.id ORDER BY id DESC LIMIT 1) j ON true
		WHERE pr.id=$1 AND pr.status='blocked'`, prID).Scan(&step); err != nil {
		return fmt.Errorf("phase %s is not blocked", prID)
	}
	e.DB.Exec(ctx, `UPDATE phase_run SET loops=0, last_error=NULL WHERE id=$1`, prID)
	return e.enqueue(ctx, e.DB, prID, step)
}

// AutoApprove resolves the human steps automatically. Demo/testing only; every action is
// attributed to "demo-auto-approver" in the audit trail so it can never pass as a person.
func (e *Engine) AutoApprove(ctx context.Context) {
	const who = "demo-auto-approver"
	for ctx.Err() == nil {
		rows, err := e.DB.Query(ctx, `SELECT q.id::text FROM question q JOIN phase_run pr ON pr.id=q.phase_run_id
			JOIN run r ON r.id=pr.run_id WHERE q.status='open' AND r.status='active'`)
		if err == nil {
			var ids []string
			for rows.Next() {
				var id string
				rows.Scan(&id)
				ids = append(ids, id)
			}
			rows.Close()
			for _, id := range ids {
				if err := e.ResolveQuestion(ctx, id, who, "", true); err != nil {
					e.ResolveQuestion(ctx, id, who, "Use your best judgment.", false)
				}
			}
		}
		rows, err = e.DB.Query(ctx, `SELECT g.id::text FROM gate g JOIN phase_run pr ON pr.id=g.phase_run_id
			JOIN run r ON r.id=pr.run_id WHERE g.status='waiting' AND r.status='active'`)
		if err == nil {
			var ids []string
			for rows.Next() {
				var id string
				rows.Scan(&id)
				ids = append(ids, id)
			}
			rows.Close()
			for _, id := range ids {
				e.Decide(ctx, id, who, "approve", "auto-approved (demo mode)")
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(700 * time.Millisecond):
		}
	}
}

// Relay publishes outbox events. Sink here is a JSONL file; swap for a Kafka producer.
func (e *Engine) Relay(ctx context.Context) {
	for ctx.Err() == nil {
		e.FlushOutbox(ctx)
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// FlushOutbox publishes pending outbox rows once.
func (e *Engine) FlushOutbox(ctx context.Context) {
	path := filepath.Join(e.Root, "events.jsonl")
	{
		func() {
			tx, err := e.DB.Begin(ctx)
			if err != nil {
				return
			}
			defer tx.Rollback(ctx)
			rows, err := tx.Query(ctx, `SELECT o.id, o.topic, e.id, e.type, COALESCE(e.run_id::text,''), COALESCE(e.phase_run_id::text,''),
				e.actor, COALESCE(e.team,''), COALESCE(e.repo,''), e.payload::text, e.occurred_at
				FROM outbox o JOIN event e ON e.id=o.event_id WHERE o.published_at IS NULL ORDER BY o.id LIMIT 200 FOR UPDATE OF o SKIP LOCKED`)
			if err != nil {
				return
			}
			var lines []string
			var ids []int64
			for rows.Next() {
				var oid, eid int64
				var topic, typ, run, pr, actor, team, repo, payload string
				var at time.Time
				rows.Scan(&oid, &topic, &eid, &typ, &run, &pr, &actor, &team, &repo, &payload, &at)
				b, _ := json.Marshal(map[string]any{"event_id": eid, "type": typ, "run_id": run, "phase_run_id": pr, "actor": actor,
					"team": team, "repo": repo, "occurred_at": at, "payload": json.RawMessage(payload), "topic": topic})
				lines = append(lines, string(b))
				ids = append(ids, oid)
			}
			rows.Close()
			if len(lines) == 0 {
				return
			}
			f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return
			}
			f.WriteString(strings.Join(lines, "\n") + "\n")
			f.Close()
			tx.Exec(ctx, `UPDATE outbox SET published_at=now() WHERE id = ANY($1)`, ids)
			tx.Commit(ctx)
		}()
	}
}

// CICheck is what the required CI check calls for an MR: the MR must carry a run-id whose
// earlier phases are complete and whose gates for this ticket are approved. (Branch
// protection + this check is what makes the automation the only path to merge.)
func (e *Engine) CICheck(ctx context.Context, runID, ticket string) (bool, string) {
	var status string
	if err := e.DB.QueryRow(ctx, `SELECT status::text FROM run WHERE id=$1`, runID).Scan(&status); err != nil {
		return false, "unknown sdlc-run-id"
	}
	if status == "escape_hatch" {
		return true, "escape hatch (logged, lead-approved)"
	}
	if status == "cancelled" || status == "failed" {
		return false, "run is " + status
	}
	rows, err := e.DB.Query(ctx, `SELECT phase::text, scope, status::text FROM phase_run
		WHERE run_id=$1 AND required AND status NOT IN ('complete','skipped')
		  AND (scope='' OR ($2<>'' AND scope=$2 AND phase IN ('ticket_dev','testing','mr_creation')))`, runID, ticket)
	if err != nil {
		return false, err.Error()
	}
	defer rows.Close()
	var pending []string
	for rows.Next() {
		var ph, sc, st string
		rows.Scan(&ph, &sc, &st)
		pending = append(pending, ph+"("+st+")")
	}
	if len(pending) > 0 {
		return false, "phases not complete: " + strings.Join(pending, ", ")
	}
	var open int
	e.DB.QueryRow(ctx, `SELECT count(*) FROM gate g JOIN phase_run pr ON pr.id=g.phase_run_id
		WHERE pr.run_id=$1 AND g.status='waiting' AND (pr.scope='' OR pr.scope=$2) AND pr.phase<>'post_mr'`, runID, ticket).Scan(&open)
	if open > 0 {
		return false, fmt.Sprintf("%d gate(s) still waiting", open)
	}
	return true, "run-id valid; all required phases and approvals recorded"
}
