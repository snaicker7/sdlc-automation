package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func (env *Env) RepoDir(repo string) string { return filepath.Join(env.RunDir(), "ws", repo) }
func (env *Env) WorktreeDir(repo, key string) string {
	return filepath.Join(env.RunDir(), "ws", repo+"-wt-"+key)
}

func sh(ctx context.Context, dir string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	out := b.String()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w\n%s", name, redactArgs(args), err, trunc(out, 2000))
	}
	return out, nil
}

// redactArgs renders a command line for error messages with credentials removed. The git
// transport auth header is just the token base64-encoded, so it must never reach logs, the
// database, artifacts or the dashboard.
func redactArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.Contains(strings.ToLower(a), "authorization:") {
			a = "<redacted>"
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	base := []string{"-c", "user.name=sdlc-bot", "-c", "user.email=sdlc-bot@local", "-c", "core.autocrlf=false", "-c", "commit.gpgsign=false"}
	return sh(ctx, dir, "git", append(base, args...)...)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...[truncated]"
}

// scaffoldFiles is the repo baseline every workspace starts from. CLAUDE.md carries the
// repo rules that the dev and review agents are told to follow.
func scaffoldFiles(name string) map[string]string {
	return map[string]string{
		"go.mod":     fmt.Sprintf("module %s\n\ngo 1.22\n", name),
		"main.go":    "package main\n\nfunc main() {}\n",
		".gitignore": "*.exe\n*.test\n/data/\n",
		"README.md":  "# " + name + "\n\nScaffolded by the SDLC orchestrator.\n",
		"CLAUDE.md": `# Repo rules (enforced by the review bot)
- Go standard library only. No third-party modules.
- Wrap errors with fmt.Errorf("...: %w", err); never discard errors.
- Pass context.Context as the first parameter of anything that does I/O or can block.
- No goroutine without a clear shutdown path.
- Every behaviour change ships with a test; run ` + "`go vet ./...`" + ` and ` + "`go test ./...`" + ` before finishing.
- Keep it small and readable; no speculative abstractions.
- Do not run git commands; the orchestrator commits.
`,
	}
}

// ensureRepo creates and smoke-checks a repo workspace (idempotent).
func (env *Env) ensureRepo(ctx context.Context, name string) (string, error) {
	dir := env.RepoDir(name)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if env.E.ghOn() {
			// Real GitHub: create the repo if missing, then clone it.
			if _, err := env.E.Int.GH.EnsureRepo(ctx, env.E.Int.GH.RepoName(name)); err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
				return "", err
			}
			if out, err := env.gitNet(ctx, filepath.Dir(dir), "clone", env.repoURL(name), filepath.Base(dir)); err != nil {
				return out, err
			}
			git(ctx, dir, "checkout", "-B", "main")
			env.link(ctx, "repo", name, "", env.E.Int.GH.RepoWebURL(env.E.Int.GH.RepoName(name)))
		} else {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
			if out, err := git(ctx, dir, "init", "-b", "main"); err != nil {
				return out, err
			}
		}
		// Brand-new (or empty) repo: lay down the scaffold as the first commit.
		if _, err := git(ctx, dir, "rev-parse", "--verify", "HEAD"); err != nil {
			for f, c := range scaffoldFiles(name) {
				if _, serr := os.Stat(filepath.Join(dir, f)); serr == nil {
					continue
				}
				if err := os.WriteFile(filepath.Join(dir, f), []byte(c), 0o644); err != nil {
					return "", err
				}
			}
			if _, err := git(ctx, dir, "add", "-A"); err != nil {
				return "", err
			}
			if _, err := git(ctx, dir, "commit", "-m", "chore: scaffold"); err != nil {
				return "", err
			}
			if env.E.ghOn() {
				if out, err := env.gitNet(ctx, dir, "push", "-u", "origin", "main"); err != nil {
					return out, err
				}
			}
		}
	}
	var report strings.Builder
	for _, step := range [][]string{{"build", "./..."}, {"vet", "./..."}, {"test", "./...", "-count=1"}} {
		out, err := sh(ctx, dir, "go", step...)
		fmt.Fprintf(&report, "$ go %s\n%s\n", strings.Join(step, " "), out)
		if err != nil {
			return report.String(), err
		}
	}
	return report.String(), nil
}

func (env *Env) ensureWorktree(ctx context.Context, repo, key string) (string, error) {
	wt := env.WorktreeDir(repo, key)
	if _, err := os.Stat(wt); err == nil {
		return wt, nil
	}
	if out, err := git(ctx, env.RepoDir(repo), "worktree", "add", "-B", "feat/"+key, wt, "main"); err != nil {
		return out, err
	}
	return wt, nil
}

func (env *Env) commitAll(ctx context.Context, dir, msg string) (string, error) {
	if _, err := git(ctx, dir, "add", "-A"); err != nil {
		return "", err
	}
	if out, _ := git(ctx, dir, "status", "--porcelain"); strings.TrimSpace(out) == "" {
		return env.headSHA(ctx, dir), nil
	}
	if _, err := git(ctx, dir, "commit", "-m", msg); err != nil {
		return "", err
	}
	return env.headSHA(ctx, dir), nil
}

func (env *Env) headSHA(ctx context.Context, dir string) string {
	out, _ := git(ctx, dir, "rev-parse", "--short", "HEAD")
	return strings.TrimSpace(out)
}

// toolOutput runs an optional scanner; absent tools are reported, not treated as failures.
func toolOutput(ctx context.Context, dir, bin string, args ...string) string {
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Sprintf("[%s not installed - skipped]", bin)
	}
	out, err := sh(ctx, dir, bin, args...)
	if err != nil {
		return fmt.Sprintf("[%s reported issues]\n%s", bin, trunc(out+err.Error(), 2500))
	}
	return fmt.Sprintf("[%s clean]\n%s", bin, trunc(out, 800))
}
