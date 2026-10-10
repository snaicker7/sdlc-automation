// fakeapi is a dev/test stand-in for Jira + Confluence + GitHub so the orchestrator's integration
// code can be exercised end to end without real accounts. GitHub repos are real bare git repos
// under -root, so clone/push/merge genuinely run. PRs report "merged" after a few polls and the
// merge is performed for real in the bare repo (simulating a human clicking Merge).
//
//	fakeapi -addr 127.0.0.1:9900 -root C:\tmp\fakegh
//	ATLASSIAN_SITE=http://127.0.0.1:9900  GITHUB_API_URL=http://127.0.0.1:9900/gh
//	GITHUB_GIT_BASE=file:///C:/tmp/fakegh
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type pr struct {
	Number     int    `json:"number"`
	Repo       string `json:"repo"`
	Head, Base string
	Title      string `json:"title"`
	Polls      int    `json:"polls"`
	Merged     bool   `json:"merged"`
}

type state struct {
	mu          sync.Mutex
	root, owner string
	issues      []map[string]any
	transitions []string
	pages       map[string]map[string]any
	prs         []*pr
	reqs        []string
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9900", "listen address")
	root := flag.String("root", "fakegh", "directory for bare repos")
	owner := flag.String("owner", "tester", "GitHub owner/login")
	pollsToMerge := flag.Int("polls", 2, "PR polls before it is 'merged by a human'")
	flag.Parse()
	abs, _ := filepath.Abs(*root)
	os.MkdirAll(abs, 0o755)
	s := &state{root: abs, owner: *owner, pages: map[string]map[string]any{}}
	mux := http.NewServeMux()
	j := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	body := func(r *http.Request) map[string]any {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		return m
	}

	// ---- Jira
	mux.HandleFunc("GET /rest/api/3/myself", func(w http.ResponseWriter, r *http.Request) { j(w, 200, map[string]any{"displayName": "Fake User"}) })
	mux.HandleFunc("GET /rest/api/3/project/{key}", func(w http.ResponseWriter, r *http.Request) {
		j(w, 200, map[string]any{"name": "Sandbox", "issueTypes": []map[string]string{{"name": "Epic"}, {"name": "Story"}, {"name": "Task"}}})
	})
	mux.HandleFunc("POST /rest/api/3/issue", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		f, _ := body(r)["fields"].(map[string]any)
		key := fmt.Sprintf("SBX-%d", len(s.issues)+1)
		f["key"] = key
		s.issues = append(s.issues, f)
		j(w, 201, map[string]any{"key": key})
	})
	mux.HandleFunc("GET /rest/api/3/issue/{key}/transitions", func(w http.ResponseWriter, r *http.Request) {
		t := func(id, n string) map[string]any {
			return map[string]any{"id": id, "name": n, "to": map[string]string{"name": n}}
		}
		j(w, 200, map[string]any{"transitions": []any{t("11", "In Progress"), t("21", "In Review"), t("31", "Done")}})
	})
	mux.HandleFunc("POST /rest/api/3/issue/{key}/transitions", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		tr, _ := body(r)["transition"].(map[string]any)
		s.transitions = append(s.transitions, r.PathValue("key")+"->"+fmt.Sprint(tr["id"]))
		w.WriteHeader(204)
	})

	// ---- Confluence
	mux.HandleFunc("GET /wiki/api/v2/spaces", func(w http.ResponseWriter, r *http.Request) {
		j(w, 200, map[string]any{"results": []map[string]string{{"id": "1001"}}})
	})
	page := func(w http.ResponseWriter, id string, p map[string]any) {
		j(w, 200, map[string]any{"id": id, "version": map[string]any{"number": p["version"]},
			"_links": map[string]string{"base": "https://fake.atlassian.net/wiki", "webui": "/spaces/SBX/pages/" + id}})
	}
	mux.HandleFunc("POST /wiki/api/v2/pages", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		b := body(r)
		id := strconv.Itoa(5000 + len(s.pages) + 1)
		s.pages[id] = map[string]any{"title": b["title"], "body": b["body"], "version": 1}
		page(w, id, s.pages[id])
	})
	mux.HandleFunc("GET /wiki/api/v2/pages/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		p, ok := s.pages[r.PathValue("id")]
		if !ok {
			j(w, 404, map[string]string{"error": "no page"})
			return
		}
		page(w, r.PathValue("id"), p)
	})
	mux.HandleFunc("PUT /wiki/api/v2/pages/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		b := body(r)
		p := s.pages[r.PathValue("id")]
		p["title"], p["body"], p["version"] = b["title"], b["body"], p["version"].(int)+1
		page(w, r.PathValue("id"), p)
	})

	// ---- GitHub (under /gh)
	bare := func(repo string) string { return filepath.Join(s.root, s.owner, repo+".git") }
	mux.HandleFunc("GET /gh/user", func(w http.ResponseWriter, r *http.Request) { j(w, 200, map[string]string{"login": s.owner}) })
	mux.HandleFunc("GET /gh/repos/{o}/{r}", func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat(bare(r.PathValue("r"))); err != nil {
			j(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		j(w, 200, map[string]string{"name": r.PathValue("r")})
	})
	mux.HandleFunc("POST /gh/user/repos", func(w http.ResponseWriter, r *http.Request) {
		name, _ := body(r)["name"].(string)
		os.MkdirAll(filepath.Dir(bare(name)), 0o755)
		if out, err := exec.Command("git", "init", "--bare", "-b", "main", bare(name)).CombinedOutput(); err != nil {
			j(w, 500, map[string]string{"message": string(out)})
			return
		}
		j(w, 201, map[string]string{"name": name})
	})
	mux.HandleFunc("POST /gh/repos/{o}/{r}/pulls", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		b := body(r)
		p := &pr{Number: len(s.prs) + 1, Repo: r.PathValue("r"), Head: fmt.Sprint(b["head"]), Base: fmt.Sprint(b["base"]), Title: fmt.Sprint(b["title"])}
		s.prs = append(s.prs, p)
		j(w, 201, map[string]any{"number": p.Number, "html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", s.owner, p.Repo, p.Number)})
	})
	mux.HandleFunc("GET /gh/repos/{o}/{r}/pulls", func(w http.ResponseWriter, r *http.Request) { j(w, 200, []any{}) })
	mux.HandleFunc("GET /gh/repos/{o}/{r}/pulls/{n}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		n, _ := strconv.Atoi(r.PathValue("n"))
		if n < 1 || n > len(s.prs) {
			j(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		p := s.prs[n-1]
		p.Polls++
		if !p.Merged && p.Polls > *pollsToMerge { // "a human clicked Merge"
			tmp, _ := os.MkdirTemp("", "fakemerge")
			defer os.RemoveAll(tmp)
			g := func(args ...string) error {
				out, err := exec.Command("git", append([]string{"-c", "user.name=human", "-c", "user.email=h@x"}, args...)...).CombinedOutput()
				if err != nil {
					log.Printf("git %v: %v\n%s", args, err, out)
				}
				return err
			}
			if g("clone", bare(p.Repo), tmp) == nil && g("-C", tmp, "merge", "--no-ff", "origin/"+p.Head, "-m", "Merge PR #"+strconv.Itoa(p.Number)) == nil &&
				g("-C", tmp, "push", "origin", "HEAD:main") == nil {
				p.Merged = true
			}
		}
		state := "open"
		if p.Merged {
			state = "closed"
		}
		j(w, 200, map[string]any{"number": p.Number, "state": state, "merged": p.Merged, "merge_commit_sha": "fake",
			"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", s.owner, p.Repo, p.Number)})
	})

	mux.HandleFunc("GET /_state", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		j(w, 200, map[string]any{"issues": s.issues, "transitions": s.transitions, "pages": s.pages, "prs": s.prs})
	})
	logged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, strings.TrimSpace(r.URL.RequestURI()))
		mux.ServeHTTP(w, r)
	})
	log.Printf("fakeapi on http://%s  (bare repos in %s)", *addr, abs)
	log.Fatal(http.ListenAndServe(*addr, logged))
}
