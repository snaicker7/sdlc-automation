package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdlc/internal/agent"
	"sdlc/internal/engine"
	"sdlc/internal/integrations"
	"sdlc/internal/web"
)

const defaultDB = "postgres://sdlc:sdlc@localhost:5433/sdlc?sslmode=disable"

const usage = `sdlc - SDLC automation orchestrator

  sdlc demo  [flags]          unattended end-to-end run (embedded Postgres, auto-approver) on a sample idea
  sdlc up    [flags]          embedded Postgres + workers + dashboard (humans approve via UI/CLI)
  sdlc start -title T -idea I start a run (needs 'up' running, or SDLC_DB)
  sdlc status [run-id]        phase stepper, questions, gates
  sdlc answer <question-id> [-accept] [text...]
  sdlc approve <gate-id> [-reject -comment "..."]
  sdlc retry <phase-run-id>   re-queue a blocked phase
  sdlc trace <run-id>         traceability matrix
  sdlc ci-check <run-id> [ticket]   the required CI check (exit 1 = block merge)

Common flags (demo/up): -agent mock|claude  -root DIR  -addr :8080  -auto-approve
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	integrations.LoadDotEnv("sdlc.env") // credentials live here (git-ignored), never on the command line
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "demo":
		err = cmdUp(ctx, log, args, true)
	case "up":
		err = cmdUp(ctx, log, args, false)
	case "serve":
		err = cmdServe(ctx, log, args)
	case "check-integrations":
		err = cmdCheck(ctx)
	case "start", "status", "answer", "approve", "retry", "trace", "ci-check":
		err = cmdClient(ctx, log, cmd, args)
	default:
		fmt.Print(usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func connect(ctx context.Context, root string, runner agent.Runner, log *slog.Logger) (*engine.Engine, error) {
	url := os.Getenv("SDLC_DB")
	if url == "" {
		url = defaultDB
	}
	var pool *pgxpool.Pool
	var err error
	for i := 0; i < 30; i++ {
		pool, err = pgxpool.New(ctx, url)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				break
			}
			pool.Close()
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("cannot reach Postgres at %s (is 'sdlc up' running?): %w", url, err)
	}
	e := &engine.Engine{DB: pool, Agent: runner, Root: root, Log: log, Int: integrations.FromEnv()}
	if e.Int.JiraEnabled() || e.Int.ConfluenceEnabled() || e.Int.GH != nil {
		log.Info("integrations", "jira", e.Int.JiraEnabled(), "confluence", e.Int.ConfluenceEnabled(), "github", e.Int.GH != nil)
	}
	return e, nil
}

// cmdCheck verifies credentials and permissions read-only: nothing is created anywhere.
func cmdCheck(ctx context.Context) error {
	s := integrations.FromEnv()
	if s.Jira == nil && s.GH == nil {
		fmt.Println("No integrations configured. Put credentials in sdlc.env (see sdlc.env.example); everything stays local until then.")
		return nil
	}
	bad := false
	var lines []string
	if s.Jira != nil {
		lines = append(lines, s.Jira.Check(ctx)...)
	} else {
		lines = append(lines, "skip  Atlassian not configured (ATLASSIAN_SITE / ATLASSIAN_EMAIL / ATLASSIAN_API_TOKEN)")
	}
	if s.GH != nil {
		lines = append(lines, s.GH.Check(ctx)...)
	} else {
		lines = append(lines, "skip  GitHub not configured (GITHUB_TOKEN / GITHUB_OWNER)")
	}
	for _, l := range lines {
		fmt.Println(l)
		if strings.HasPrefix(l, "FAIL") {
			bad = true
		}
	}
	if bad {
		return fmt.Errorf("one or more checks failed")
	}
	return nil
}

func startPG(root string, log *slog.Logger) (func(), error) {
	if os.Getenv("SDLC_DB") != "" {
		return func() {}, nil // external Postgres
	}
	cfg := embeddedpostgres.DefaultConfig().
		Username("sdlc").Password("sdlc").Database("sdlc").Port(5433).
		DataPath(filepath.Join(root, "pg", "data")).
		RuntimePath(filepath.Join(root, "pg", "runtime")).
		Version(embeddedpostgres.V16).
		Logger(io.Discard)
	pg := embeddedpostgres.NewDatabase(cfg)
	log.Info("starting embedded Postgres (first run downloads the binaries)", "port", 5433)
	if err := pg.Start(); err != nil {
		return nil, fmt.Errorf("embedded postgres: %w", err)
	}
	return func() { pg.Stop() }, nil
}

func cmdUp(ctx context.Context, log *slog.Logger, args []string, demo bool) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	agentName := fs.String("agent", "mock", "agent backend: mock | claude")
	root := fs.String("root", "data", "data root (artifacts, workspaces, postgres)")
	addr := fs.String("addr", ":8080", "dashboard address")
	auto := fs.Bool("auto-approve", demo, "auto-approve questions and gates (demo only)")
	workers := fs.Int("workers", 2, "worker goroutines")
	budget := fs.Float64("budget", 25, "per-run budget in USD")
	title := fs.String("title", "Document organizer", "demo run title")
	idea := fs.String("idea", demoIdea, "demo run idea")
	stories := fs.Int("max-stories", 2, "max stories for the demo run")
	keep := fs.Bool("keep", false, "demo: keep serving the dashboard after the run finishes")
	mStrong := fs.String("model-strong", "claude-sonnet-5-5", "model for PRD/ADD/plan/review")
	mCode := fs.String("model-code", "claude-sonnet-5-5", "model for implementation")
	mFast := fs.String("model-fast", "claude-haiku-5-5", "model for formatting/summaries")
	fs.Parse(args)

	absRoot, _ := filepath.Abs(*root)
	os.MkdirAll(absRoot, 0o755)
	var runner agent.Runner = agent.Mock{}
	if *agentName == "claude" {
		c := agent.NewClaude(map[agent.Tier]string{agent.TierStrong: *mStrong, agent.TierCode: *mCode, agent.TierFast: *mFast})
		c.ScratchDir = filepath.Join(absRoot, "scratch")
		os.MkdirAll(c.ScratchDir, 0o755)
		runner = c
	} else if *agentName != "mock" {
		return fmt.Errorf("unknown agent %q", *agentName)
	}

	stopPG, err := startPG(absRoot, log)
	if err != nil {
		return err
	}
	defer stopPG()
	e, err := connect(ctx, absRoot, runner, log)
	if err != nil {
		return err
	}
	defer e.DB.Close()
	if err := e.Migrate(ctx); err != nil {
		return err
	}

	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i := 0; i < *workers; i++ {
		go e.Work(wctx, fmt.Sprintf("worker-%d", i+1))
	}
	go e.Relay(wctx)
	if *auto {
		if e.Int.Any() {
			log.Warn("auto-approve is ON while REAL integrations are configured: Jira issues, Confluence pages and GitHub PRs will be created without a human review")
		}
		log.Warn("auto-approve is ON: all questions and gates are resolved by 'demo-auto-approver'")
		go e.AutoApprove(wctx)
	}
	srv := &http.Server{Addr: *addr, Handler: (&web.Server{E: e, BudgetUSD: *budget, MaxStories: *stories}).Handler()}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("dashboard", "err", err)
		}
	}()
	defer srv.Shutdown(context.Background())
	log.Info("orchestrator up", "dashboard", "http://localhost"+*addr, "agent", *agentName, "root", absRoot)

	if !demo {
		<-ctx.Done()
		return nil
	}

	runID, err := e.StartRun(ctx, engine.StartOpts{Title: *title, Idea: *idea, Initiator: "demo-user", Team: "platform", BudgetUSD: *budget, MaxStories: *stories})
	if err != nil {
		return err
	}
	log.Info("run started", "id", runID)
	for ctx.Err() == nil {
		time.Sleep(2 * time.Second)
		v, err := e.View(ctx, runID)
		if err != nil {
			return err
		}
		if v.Run.Status != "active" {
			break
		}
		for _, p := range v.Phases {
			if p.Status == "blocked" {
				printStatus(v)
				return fmt.Errorf("phase %s %s is blocked: %s", p.Phase, p.Scope, p.LastError)
			}
		}
	}
	e.FlushOutbox(ctx)
	v, err := e.View(ctx, runID)
	if err != nil {
		return err
	}
	printStatus(v)
	tr, _ := e.Trace(ctx, runID)
	fmt.Println("\nTraceability:\n" + tr)
	ok, why := e.CICheck(ctx, runID, "")
	fmt.Printf("CI check (run-level): ok=%v  %s\n", ok, why)
	fmt.Printf("\nWorkspace (the built app): %s\n", filepath.Join(absRoot, "runs", runID, "ws"))
	fmt.Printf("Artifacts:                 %s\n", filepath.Join(absRoot, "runs", runID, "artifacts"))
	fmt.Printf("Event stream:              %s\n", filepath.Join(absRoot, "events.jsonl"))
	if *keep {
		fmt.Println("dashboard still serving; Ctrl-C to quit")
		<-ctx.Done()
	}
	return nil
}

// cmdServe runs only the dashboard against an already-running Postgres (e.g. next to 'sdlc up').
func cmdServe(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	root := fs.String("root", "data", "data root")
	addr := fs.String("addr", ":8081", "dashboard address")
	fs.Parse(args)
	absRoot, _ := filepath.Abs(*root)
	e, err := connect(ctx, absRoot, agent.Mock{}, log)
	if err != nil {
		return err
	}
	defer e.DB.Close()
	srv := &http.Server{Addr: *addr, Handler: (&web.Server{E: e, BudgetUSD: 25, MaxStories: 2}).Handler()}
	go func() { <-ctx.Done(); srv.Shutdown(context.Background()) }()
	log.Info("dashboard only", "url", "http://localhost"+*addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

const demoIdea = `A simple document organizer: a small Go command-line/HTTP tool where a user adds text/markdown documents with a title and tags, lists them, and searches them by tag or text. Local single-user app, data persisted to a JSON file, standard library only.`

func cmdClient(ctx context.Context, log *slog.Logger, cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	root := fs.String("root", "data", "data root")
	user := fs.String("user", currentUser(), "your identity for the audit trail")
	title := fs.String("title", "", "run title (start)")
	idea := fs.String("idea", "", "what to build (start)")
	team := fs.String("team", "default", "team (start)")
	size := fs.String("size", "standard", "trivial|standard|large (start)")
	budget := fs.Float64("budget", 25, "USD budget (start)")
	stories := fs.Int("max-stories", 2, "max stories (start)")
	accept := fs.Bool("accept", false, "accept the proposed default as an assumption (answer)")
	reject := fs.Bool("reject", false, "request changes (approve)")
	comment := fs.String("comment", "", "comment (approve)")
	// allow flags after positional args
	var pos []string
	for len(args) > 0 {
		fs.Parse(args)
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	absRoot, _ := filepath.Abs(*root)
	e, err := connect(ctx, absRoot, agent.Mock{}, log)
	if err != nil {
		return err
	}
	defer e.DB.Close()
	need := func(n int) error {
		if len(pos) < n {
			return fmt.Errorf("missing argument; see usage")
		}
		return nil
	}
	switch cmd {
	case "start":
		if *title == "" || *idea == "" {
			return fmt.Errorf("-title and -idea are required")
		}
		id, err := e.StartRun(ctx, engine.StartOpts{Title: *title, Idea: *idea, Initiator: *user, Team: *team, Size: *size, BudgetUSD: *budget, MaxStories: *stories})
		if err == nil {
			fmt.Println(id)
		}
		return err
	case "status":
		if len(pos) == 0 {
			runs, err := e.ListRuns(ctx)
			for _, r := range runs {
				fmt.Printf("%s  %-10s  $%.2f  %s\n", r.ID, r.Status, r.SpentUSD, r.Title)
			}
			return err
		}
		v, err := e.View(ctx, pos[0])
		if err == nil {
			printStatus(v)
		}
		return err
	case "answer":
		if err := need(1); err != nil {
			return err
		}
		return e.ResolveQuestion(ctx, pos[0], *user, strings.Join(pos[1:], " "), *accept)
	case "approve":
		if err := need(1); err != nil {
			return err
		}
		d := "approve"
		if *reject {
			d = "request_changes"
		}
		return e.Decide(ctx, pos[0], *user, d, *comment)
	case "retry":
		if err := need(1); err != nil {
			return err
		}
		return e.Retry(ctx, pos[0])
	case "trace":
		if err := need(1); err != nil {
			return err
		}
		s, err := e.Trace(ctx, pos[0])
		fmt.Print(s)
		return err
	case "ci-check":
		if err := need(1); err != nil {
			return err
		}
		t := ""
		if len(pos) > 1 {
			t = pos[1]
		}
		ok, why := e.CICheck(ctx, pos[0], t)
		fmt.Println(why)
		if !ok {
			os.Exit(1)
		}
	}
	return nil
}

func currentUser() string {
	if u := os.Getenv("USERNAME"); u != "" {
		return u
	}
	return "cli-user"
}

func printStatus(v *engine.RunView) {
	r := v.Run
	fmt.Printf("\nRun %s  [%s]  %s\n  spent $%.2f / $%.2f, %d agent calls\n\n", r.ID, r.Status, r.Title, r.SpentUSD, r.BudgetUSD, v.Agents)
	mark := map[string]string{"complete": "[done]", "in_progress": "[ .. ]", "awaiting_human": "[HUMAN]", "blocked": "[BLOCK]", "pending": "[    ]", "skipped": "[skip]"}
	for _, p := range v.Phases {
		name := p.Phase
		if p.Scope != "" {
			name = "  " + p.Scope + "/" + p.Phase
		}
		extra := ""
		if p.Loops > 0 {
			extra = fmt.Sprintf(" (loops=%d)", p.Loops)
		}
		fmt.Printf("  %-8s %-34s %s%s\n", mark[p.Status], name, p.Step, extra)
	}
	for _, q := range v.Questions {
		fmt.Printf("  Q[%s] %s -> %s: %s (%s)\n", q.Phase, q.Text, q.Status, q.Answer, q.ResolvedBy)
		if q.Status == "open" {
			fmt.Printf("      id=%s default=%q\n", q.ID, q.Default)
		}
	}
	for _, g := range v.Gates {
		fmt.Printf("  GATE %s (%s %s) waiting for %s: id=%s\n", g.Type, g.Phase, g.Scope, g.Role, g.ID)
	}
	for _, m := range v.MRs {
		fmt.Printf("  MR !%d %s %s %s\n", m.ID, m.Ticket, m.Status, m.MergeSHA)
	}
}
