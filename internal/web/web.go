// Package web serves the dashboard: pipeline view, "needs you" inbox (questions + approvals),
// documents, tickets and an activity feed.
package web

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"regexp"
	"strings"

	"sdlc/internal/engine"
)

type Server struct {
	E          *engine.Engine
	BudgetUSD  float64
	MaxStories int
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /run/{id}", s.run)
	mux.HandleFunc("GET /api/run/{id}", s.api)
	mux.HandleFunc("POST /start", s.start)
	mux.HandleFunc("POST /answer", s.answer)
	mux.HandleFunc("POST /decide", s.decide)
	return mux
}

type page struct {
	V     *engine.RunView
	Tab   string
	Attn  int
	Runs  []runCard
	Flash string
	Title string
	Open  bool // index: expand the "new run" form
	// OpenDocs expands the PRD/ADD on the Documents tab (?open=1).
	OpenDocs bool
}

type runCard struct {
	Run                engine.Run
	Done, Total, Needs int
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	runs, err := s.E.ListRuns(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	p := page{Title: "Runs", Open: r.URL.Query().Get("new") != ""}
	for _, ru := range runs {
		v, err := s.E.View(r.Context(), ru.ID)
		if err != nil {
			continue
		}
		c := runCard{Run: ru, Needs: attention(v)}
		for _, ph := range v.Phases {
			c.Total++
			if ph.Status == "complete" || ph.Status == "skipped" {
				c.Done++
			}
		}
		p.Runs = append(p.Runs, c)
	}
	render(w, "index", p)
}

func attention(v *engine.RunView) int {
	n := len(v.Gates)
	for _, q := range v.Questions {
		if q.Status == "open" {
			n++
		}
	}
	return n
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	v, err := s.E.View(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	tab := r.URL.Query().Get("tab")
	if tab == "" {
		tab = "overview"
	}
	render(w, "run", page{V: v, Tab: tab, Attn: attention(v), Title: v.Run.Title, OpenDocs: r.URL.Query().Get("open") != ""})
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	v, err := s.E.View(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	title, idea := strings.TrimSpace(r.FormValue("title")), strings.TrimSpace(r.FormValue("idea"))
	if title == "" || idea == "" {
		http.Error(w, "title and idea are required", 400)
		return
	}
	id, err := s.E.StartRun(r.Context(), engine.StartOpts{Title: title, Idea: idea, Initiator: user(r), Team: "default",
		Size: r.FormValue("size"), BudgetUSD: s.BudgetUSD, MaxStories: s.MaxStories})
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/run/"+id, http.StatusSeeOther)
}

func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	err := s.E.ResolveQuestion(r.Context(), r.FormValue("q"), user(r), r.FormValue("answer"), r.FormValue("accept") != "")
	back(w, r, err)
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	err := s.E.Decide(r.Context(), r.FormValue("gate"), user(r), r.FormValue("decision"), r.FormValue("comment"))
	back(w, r, err)
}

func user(r *http.Request) string {
	if u := strings.TrimSpace(r.FormValue("user")); u != "" {
		return u
	}
	return "web-user"
}

func back(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/run/"+r.FormValue("run"), http.StatusSeeOther)
}

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.ExecuteTemplate(w, name, data); err != nil {
		fmt.Fprintf(w, "<pre>template error: %v</pre>", err)
	}
}

// ---------------------------------------------------------------- template helpers

var phaseLabel = map[string]string{
	"prd": "PRD", "add": "Architecture", "repo_breakdown": "Repo split", "jira_creation": "Jira tickets",
	"workspace": "Workspace", "ticket_dev": "Develop", "testing": "Test", "mr_creation": "Merge request", "post_mr": "Review & scan",
}
var statusLabel = map[string]string{
	"complete": "Done", "awaiting_human": "Needs you", "in_progress": "Working", "blocked": "Blocked", "pending": "Waiting", "skipped": "Skipped",
}
var stepLabel = map[string]string{
	"clarify": "asking questions", "generate": "drafting", "critique": "critic reviewing", "validate": "validating",
	"gate": "awaiting approval", "publish": "finishing up",
}
var gateHelp = map[string]string{
	"pm_approval":           "Confirm this PRD captures what should be built. Open questions are all resolved.",
	"tech_lead_approval":    "Confirm the design is sound and ready to be broken down into work.",
	"jira_preview_approval": "This is a dry run: nothing is created in Jira until you approve.",
	"plan_approval":         "Approve the implementation plan before any code is written.",
	"dev_signoff":           "Review the test results and diff summary before an MR is opened.",
	"dev_submit":            "Review the merge request description before it is submitted.",
	"reviewer_approval":     "Review findings and scanner output. Approving merges the MR.",
}

var (
	boldRe = regexp.MustCompile(`\*\*(.+?)\*\*`)
	codeRe = regexp.MustCompile("`([^`]+)`")
	numRe  = regexp.MustCompile(`^\d+\.\s+`)
)

// md is a small, safe Markdown renderer (escape first, then structure) for PRDs/ADDs/plans.
func md(src string) template.HTML {
	var sb strings.Builder
	inList, inCode := "", false
	closeList := func() {
		if inList != "" {
			sb.WriteString("</" + inList + ">")
			inList = ""
		}
	}
	inline := func(s string) string {
		s = html.EscapeString(s)
		s = boldRe.ReplaceAllString(s, "<strong>$1</strong>")
		return codeRe.ReplaceAllString(s, "<code>$1</code>")
	}
	for _, line := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "```"):
			closeList()
			if inCode {
				sb.WriteString("</pre>")
			} else {
				sb.WriteString("<pre>")
			}
			inCode = !inCode
		case inCode:
			sb.WriteString(html.EscapeString(line) + "\n")
		case t == "":
			closeList()
		case strings.HasPrefix(t, "#"):
			closeList()
			lvl := len(t) - len(strings.TrimLeft(t, "#"))
			if lvl > 4 {
				lvl = 4
			}
			fmt.Fprintf(&sb, "<h%d>%s</h%d>", lvl+1, inline(strings.TrimSpace(t[lvl:])), lvl+1)
		case strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* "):
			if inList != "ul" {
				closeList()
				sb.WriteString("<ul>")
				inList = "ul"
			}
			sb.WriteString("<li>" + inline(t[2:]) + "</li>")
		case numRe.MatchString(t):
			if inList != "ol" {
				closeList()
				sb.WriteString("<ol>")
				inList = "ol"
			}
			sb.WriteString("<li>" + inline(numRe.ReplaceAllString(t, "")) + "</li>")
		default:
			closeList()
			sb.WriteString("<p>" + inline(t) + "</p>")
		}
	}
	closeList()
	if inCode {
		sb.WriteString("</pre>")
	}
	return template.HTML(sb.String())
}

func eventText(typ, detail string) string {
	var d map[string]any
	json.Unmarshal([]byte(detail), &d)
	s := func(k string) string { v, _ := d[k].(string); return v }
	switch typ {
	case "run_started":
		return "Run started"
	case "run_completed":
		return "Run completed"
	case "phase_started":
		return "Phase started"
	case "phase_completed":
		return "Phase completed"
	case "phase_blocked":
		return "Phase blocked: " + s("error")
	case "questions_raised":
		return fmt.Sprintf("Agent raised %v clarifying question(s)", d["count"])
	case "question_resolved":
		return "Question answered"
	case "assumption_accepted":
		return "Suggested default accepted as an assumption"
	case "artifact_created":
		return fmt.Sprintf("Created %s v%v", s("kind"), d["version"])
	case "critique_completed":
		if n, _ := d["blocking"].(float64); n > 0 {
			return fmt.Sprintf("Critic found %d blocking issue(s); sending back for revision", int(n))
		}
		return "Critic found no blocking issues"
	case "validation_failed":
		return "Automatic checks failed: " + s("error")
	case "gate_opened":
		return "Waiting for approval: " + s("gate")
	case "gate_approved":
		return "Approved " + s("gate")
	case "gate_rejected":
		return "Changes requested: " + s("comment")
	case "agent_invoked":
		return fmt.Sprintf("Agent call (%s, $%.3f)", s("model"), d["cost_usd"])
	case "jira_created":
		return "Jira issues created"
	case "confluence_published":
		return "Published " + s("kind") + " to Confluence"
	case "waiting_for_pr_merge":
		return "Waiting for " + s("ticket") + " pull request to be merged on GitHub"
	case "mr_created":
		return "Merge request opened for " + s("ticket")
	case "mr_merged":
		return "Merged " + s("ticket") + " into main"
	case "budget_exceeded":
		return "Run budget exhausted"
	}
	return typ
}

var funcs = template.FuncMap{
	"md":     md,
	"isMD":   func(k string) bool { return strings.HasSuffix(k, ".md") },
	"label":  func(p string) string { return phaseLabel[p] },
	"status": func(s string) string { return statusLabel[s] },
	"step":   func(s string) string { return stepLabel[s] },
	"help":   func(t string) string { return gateHelp[t] },
	"event":  eventText,
	"planning": func(v *engine.RunView) []engine.PhaseView {
		var out []engine.PhaseView
		for _, p := range v.Phases {
			if p.Scope == "" {
				out = append(out, p)
			}
		}
		return out
	},
	"stories": func(v *engine.RunView) []engine.Issue {
		var out []engine.Issue
		for _, i := range v.Issues {
			if i.Kind == "story" {
				out = append(out, i)
			}
		}
		return out
	},
	"phasesFor": func(v *engine.RunView, key string) []engine.PhaseView {
		var out []engine.PhaseView
		for _, p := range v.Phases {
			if p.Scope == key {
				out = append(out, p)
			}
		}
		return out
	},
	"mrFor": func(v *engine.RunView, key string) *engine.MRView {
		for i := range v.MRs {
			if v.MRs[i].Ticket == key {
				return &v.MRs[i]
			}
		}
		return nil
	},
	"openQs": func(v *engine.RunView) []engine.QuestionView {
		var out []engine.QuestionView
		for _, q := range v.Questions {
			if q.Status == "open" {
				out = append(out, q)
			}
		}
		return out
	},
	"doneQs": func(v *engine.RunView) []engine.QuestionView {
		var out []engine.QuestionView
		for _, q := range v.Questions {
			if q.Status != "open" {
				out = append(out, q)
			}
		}
		return out
	},
	"link": func(v *engine.RunView, kind, ref string) string {
		for _, l := range v.Links {
			if l.Kind == kind && l.Ref == ref {
				return l.URL
			}
		}
		return ""
	},
	"docLinks": func(v *engine.RunView) []engine.LinkView {
		var out []engine.LinkView
		for _, l := range v.Links {
			if l.Kind == "confluence" || l.Kind == "repo" {
				out = append(out, l)
			}
		}
		return out
	},
	"doneCount": func(v *engine.RunView) int {
		n := 0
		for _, p := range v.Phases {
			if p.Status == "complete" || p.Status == "skipped" {
				n++
			}
		}
		return n
	},
	"merged": func(v *engine.RunView) int {
		n := 0
		for _, m := range v.MRs {
			if m.Status == "merged" {
				n++
			}
		}
		return n
	},
	"pct": func(d, t int) int {
		if t == 0 {
			return 0
		}
		return d * 100 / t
	},
	"icon": func(s string, i int) string {
		switch s {
		case "complete":
			return "✓"
		case "awaiting_human":
			return "!"
		case "blocked":
			return "✕"
		case "skipped":
			return "–"
		}
		return fmt.Sprint(i + 1)
	},
	"inc":  func(i int) int { return i + 1 },
	"prio": func(p int) string { return map[int]string{1: "Blocking", 2: "Important", 3: "Nice to know"}[p] },
}

var tpl = template.Must(template.New("ui").Funcs(funcs).Parse(baseCSS + indexPage + runPage))
