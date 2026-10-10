package integrations

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// GitHub creates repos, opens pull requests and reports their state. Git transport (clone/push)
// authenticates through AuthArgs so the token never lands in a remote URL or .git/config.
type GitHub struct {
	Token, Owner, Prefix string
	HC                   *http.Client
	APIBase, GitBase     string // overridable for GitHub Enterprise or tests
}

func (g *GitHub) api() string {
	if g.APIBase != "" {
		return strings.TrimRight(g.APIBase, "/")
	}
	return "https://api.github.com"
}

func (g *GitHub) gitBase() string {
	if g.GitBase != "" {
		return strings.TrimRight(g.GitBase, "/")
	}
	return "https://github.com"
}

func (g *GitHub) hdr() map[string]string {
	return map[string]string{"Authorization": "Bearer " + g.Token, "Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}
}

func (g *GitHub) call(ctx context.Context, method, path string, in, out any) error {
	return doJSON(ctx, g.HC, method, g.api()+path, g.hdr(), in, out)
}

func (g *GitHub) RepoName(name string) string { return g.Prefix + name }
func (g *GitHub) RemoteURL(repo string) string {
	return fmt.Sprintf("%s/%s/%s.git", g.gitBase(), g.Owner, repo)
}
func (g *GitHub) RepoWebURL(repo string) string {
	return fmt.Sprintf("%s/%s/%s", g.gitBase(), g.Owner, repo)
}

// AuthArgs are git global options that authenticate HTTPS transport to github.com.
func (g *GitHub) AuthArgs() []string {
	b := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + g.Token))
	return []string{"-c", "http.https://github.com/.extraheader=AUTHORIZATION: basic " + b}
}

// EnsureRepo makes sure owner/repo exists (private). Returns true if it was just created.
func (g *GitHub) EnsureRepo(ctx context.Context, repo string) (bool, error) {
	err := g.call(ctx, "GET", fmt.Sprintf("/repos/%s/%s", g.Owner, repo), nil, nil)
	if err == nil {
		return false, nil
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 404 {
		return false, err
	}
	var me struct{ Login string }
	if err := g.call(ctx, "GET", "/user", nil, &me); err != nil {
		return false, err
	}
	path := "/orgs/" + g.Owner + "/repos"
	if strings.EqualFold(me.Login, g.Owner) {
		path = "/user/repos"
	}
	body := map[string]any{"name": repo, "private": true, "description": "Created by the SDLC orchestrator"}
	if err := g.call(ctx, "POST", path, body, nil); err != nil {
		return false, fmt.Errorf("create repo %s/%s: %w", g.Owner, repo, err)
	}
	return true, nil
}

type PR struct {
	Number   int
	URL      string
	State    string // open | closed
	Merged   bool
	MergeSHA string
}

func (g *GitHub) CreatePR(ctx context.Context, repo, head, base, title, body string) (PR, error) {
	var out struct {
		Number  int
		HTMLURL string `json:"html_url"`
	}
	err := g.call(ctx, "POST", fmt.Sprintf("/repos/%s/%s/pulls", g.Owner, repo),
		map[string]any{"title": title, "head": head, "base": base, "body": body}, &out)
	if err != nil {
		// A PR for this branch may already exist (retry): find and reuse it.
		var he *HTTPError
		if errors.As(err, &he) && he.Status == 422 {
			var list []struct {
				Number  int
				HTMLURL string `json:"html_url"`
			}
			if e2 := g.call(ctx, "GET", fmt.Sprintf("/repos/%s/%s/pulls?head=%s:%s&state=open", g.Owner, repo, g.Owner, head), nil, &list); e2 == nil && len(list) > 0 {
				return PR{Number: list[0].Number, URL: list[0].HTMLURL, State: "open"}, nil
			}
		}
		return PR{}, err
	}
	return PR{Number: out.Number, URL: out.HTMLURL, State: "open"}, nil
}

func (g *GitHub) GetPR(ctx context.Context, repo string, n int) (PR, error) {
	var out struct {
		Number         int
		HTMLURL        string `json:"html_url"`
		State          string
		Merged         bool
		MergeCommitSHA string `json:"merge_commit_sha"`
	}
	if err := g.call(ctx, "GET", fmt.Sprintf("/repos/%s/%s/pulls/%d", g.Owner, repo, n), nil, &out); err != nil {
		return PR{}, err
	}
	return PR{Number: out.Number, URL: out.HTMLURL, State: out.State, Merged: out.Merged, MergeSHA: out.MergeCommitSHA}, nil
}

// Check verifies the token and owner without creating anything.
func (g *GitHub) Check(ctx context.Context) []string {
	var me struct{ Login string }
	if err := g.call(ctx, "GET", "/user", nil, &me); err != nil {
		return []string{"FAIL  GitHub auth: " + err.Error()}
	}
	rep := []string{"ok    GitHub auth as " + me.Login}
	if strings.EqualFold(me.Login, g.Owner) {
		rep = append(rep, "ok    owner is your personal account; repos will be created as "+g.Prefix+"<name>")
	} else if err := g.call(ctx, "GET", "/orgs/"+g.Owner, nil, nil); err != nil {
		rep = append(rep, "FAIL  owner "+g.Owner+" is neither your user nor an org you can see: "+err.Error())
	} else {
		rep = append(rep, "ok    org "+g.Owner+" visible (creating repos there needs org permission)")
	}
	return rep
}
