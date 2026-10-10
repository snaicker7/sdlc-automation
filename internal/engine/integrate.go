package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"sdlc/internal/integrations"
)

// ErrWait is returned by a step that must wait on an external event (e.g. a human merging the
// GitHub PR). The job is re-queued without consuming a retry and the phase shows as "needs you".
var ErrWait = errors.New("waiting on external event")

func (e *Engine) jiraOn() bool { return e.Int.JiraEnabled() }
func (e *Engine) confOn() bool { return e.Int.ConfluenceEnabled() }
func (e *Engine) ghOn() bool   { return e.Int != nil && e.Int.GH != nil }

func (env *Env) link(ctx context.Context, kind, ref, extID, url string) {
	env.E.DB.Exec(ctx, `INSERT INTO external_link(run_id, kind, ref, ext_id, url) VALUES($1,$2,$3,$4,$5)
		ON CONFLICT (run_id, kind, ref) DO UPDATE SET ext_id=EXCLUDED.ext_id, url=EXCLUDED.url`, env.Run.ID, kind, ref, extID, url)
}

func (env *Env) linkGet(ctx context.Context, kind, ref string) (extID, url string, ok bool) {
	err := env.E.DB.QueryRow(ctx, `SELECT ext_id, url FROM external_link WHERE run_id=$1 AND kind=$2 AND ref=$3`, env.Run.ID, kind, ref).Scan(&extID, &url)
	return extID, url, err == nil
}

// publishConfluence creates (or updates, on retry) the Confluence page for an approved document.
func (env *Env) publishConfluence(ctx context.Context, kind, title string) error {
	if !env.E.confOn() {
		return nil
	}
	_, body := env.Own(ctx, kind)
	body += fmt.Sprintf("\n\n---\n*Published by the SDLC orchestrator. Run %s.*", env.Run.ID)
	id, _, _ := env.linkGet(ctx, "confluence", kind)
	title = fmt.Sprintf("%s [%s]", title, env.Run.ID[:8])
	pid, url, err := env.E.Int.Jira.UpsertPage(ctx, id, title, integrations.MarkdownToStorage(body))
	if err != nil {
		return fmt.Errorf("confluence publish %s: %w", kind, err)
	}
	env.link(ctx, "confluence", kind, pid, url)
	env.E.emit(ctx, env.E.DB, env.Run.ID, env.PR.ID, "confluence_published", "orchestrator", map[string]any{"kind": kind, "url": url})
	return nil
}

// gitNet runs a git command that talks to the remote, with GitHub auth when configured.
func (env *Env) gitNet(ctx context.Context, dir string, args ...string) (string, error) {
	if env.E.ghOn() {
		args = append(env.E.Int.GH.AuthArgs(), args...)
	}
	return git(ctx, dir, args...)
}

func (env *Env) repoURL(name string) string {
	return env.E.Int.GH.RemoteURL(env.E.Int.GH.RepoName(name))
}

// transition moves a Jira issue through the workflow; a missing transition is logged, not fatal.
func (env *Env) transition(ctx context.Context, key string, names ...string) {
	if !env.E.jiraOn() {
		return
	}
	ok, err := env.E.Int.Jira.Transition(ctx, key, names...)
	switch {
	case err != nil:
		env.E.Log.Warn("jira transition failed", "key", key, "err", firstLine(err.Error()))
	case !ok:
		env.E.Log.Warn("jira transition not available in workflow", "key", key, "wanted", strings.Join(names, "/"))
	}
}
