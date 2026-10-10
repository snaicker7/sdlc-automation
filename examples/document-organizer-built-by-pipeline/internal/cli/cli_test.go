package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"docs/internal/store"
)

type fixture struct {
	t    *testing.T
	path string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, path: filepath.Join(t.TempDir(), "docs.json")}
}

// run executes `docs <cmd> --data <path> --lock-timeout 1s <rest...>`.
func (f *fixture) run(stdin, cmd string, rest ...string) (int, string, string) {
	f.t.Helper()
	args := append([]string{cmd, "--data", f.path, "--lock-timeout", "1s"}, rest...)
	var out, errOut bytes.Buffer
	code := Run(context.Background(), args, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

func (f *fixture) add(title string, rest ...string) store.Document {
	f.t.Helper()
	code, out, errOut := f.run("body", "add", append([]string{"--title", title}, rest...)...)
	if code != 0 {
		f.t.Fatalf("add %q: code %d, stderr %q", title, code, errOut)
	}
	var d store.Document
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		f.t.Fatalf("add output %q: %v", out, err)
	}
	return d
}

func (f *fixture) assertNoData() {
	f.t.Helper()
	if _, err := os.Stat(f.path); !errors.Is(err, os.ErrNotExist) {
		f.t.Fatalf("data file should not exist: %v", err)
	}
}

func TestPathPrecedence(t *testing.T) {
	env := map[string]string{"DOCS_FILE": "env.json"}
	getenv := func(k string) string { return env[k] }
	if got := resolvePath("flag.json", getenv); got != "flag.json" {
		t.Errorf("flag = %q", got)
	}
	if got := resolvePath("", getenv); got != "env.json" {
		t.Errorf("env = %q", got)
	}
	if got := resolvePath("", func(string) string { return "" }); got != "./docs.json" {
		t.Errorf("default = %q", got)
	}

	dir := t.TempDir()
	envFile, flagFile := filepath.Join(dir, "env.json"), filepath.Join(dir, "flag.json")
	t.Setenv("DOCS_FILE", envFile)
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"add", "--title", "a"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
	if _, err := os.Stat(envFile); err != nil {
		t.Fatalf("DOCS_FILE not used: %v", err)
	}
	if code := Run(context.Background(), []string{"add", "--title", "a", "--data", flagFile}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
	if _, err := os.Stat(flagFile); err != nil {
		t.Fatalf("--data not used: %v", err)
	}
}

func TestHelpAndUsage(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{{[]string{"help"}, 0}, {nil, 2}, {[]string{"bogus"}, 2}} {
		var out, errOut bytes.Buffer
		code := Run(context.Background(), tc.args, strings.NewReader(""), &out, &errOut)
		if code != tc.code || !strings.Contains(errOut.String(), "usage:") || out.Len() != 0 {
			t.Errorf("%v: code %d stdout %q stderr %q", tc.args, code, out.String(), errOut.String())
		}
	}
}

func TestMissingTitle(t *testing.T) {
	f := newFixture(t)
	if code, _, errOut := f.run("", "add"); code != 2 || !strings.HasPrefix(errOut, "docs: ") {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
	f.assertNoData()
}

func TestAddFileAndStdin(t *testing.T) {
	f := newFixture(t)
	file := filepath.Join(t.TempDir(), "c.txt")
	if err := os.WriteFile(file, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := f.add("A", "--file", file, "--tag", " Go ", "--tag", "go", "--tag", "Web")
	b := f.add("B") // stdin: "body"
	if a.ID == 0 || b.ID == 0 || a.ID == b.ID {
		t.Fatalf("ids %d %d", a.ID, b.ID)
	}
	for _, want := range []store.Document{a, b} {
		code, out, _ := f.run("", "get", fmt.Sprint(want.ID))
		var got store.Document
		if code != 0 || json.Unmarshal([]byte(out), &got) != nil {
			t.Fatalf("get: %d %q", code, out)
		}
		if got.Title != want.Title || got.Content != want.Content || strings.Join(got.Tags, ",") != strings.Join(want.Tags, ",") {
			t.Fatalf("got %+v want %+v", got, want)
		}
	}
	if a.Content != "from file" || strings.Join(a.Tags, ",") != "go,web" || b.Content != "body" {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
	// "-" also means stdin.
	if code, _, _ := f.run("x", "add", "--title", "C", "--file", "-"); code != 0 {
		t.Fatal("--file - failed")
	}
}

func TestAddValidationExit2(t *testing.T) {
	big := strings.Repeat("a", 1<<20)
	file := func(s string) string {
		p := filepath.Join(t.TempDir(), "c.txt")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	bad := []struct {
		name  string
		stdin string
		args  []string
	}{
		{"blank title", "", []string{"--title", "   "}},
		{"title 201", "", []string{"--title", strings.Repeat("a", 201)}},
		{"stdin 1MiB+1", big + "a", []string{"--title", "t"}},
		{"file 1MiB+1", "", []string{"--title", "t", "--file", file(big + "a")}},
		{"tag comma", "", []string{"--title", "t", "--tag", "a,b"}},
		{"tag 51", "", []string{"--title", "t", "--tag", strings.Repeat("a", 51)}},
		{"tag blank", "", []string{"--title", "t", "--tag", " "}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			code, out, errOut := f.run(tc.stdin, "add", tc.args...)
			if code != 2 || out != "" || !strings.HasPrefix(errOut, "docs: ") {
				t.Fatalf("code %d stdout %.50q stderr %.100q", code, out, errOut)
			}
			f.assertNoData()
		})
	}
	good := []struct {
		name  string
		stdin string
		args  []string
	}{
		{"title 200", "", []string{"--title", strings.Repeat("a", 200)}},
		{"stdin 1MiB", big, []string{"--title", "t"}},
		{"file 1MiB", "", []string{"--title", "t", "--file", file(big)}},
	}
	for _, tc := range good {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if code, _, errOut := f.run(tc.stdin, "add", tc.args...); code != 0 {
				t.Fatalf("code %d stderr %.100q", code, errOut)
			}
		})
	}
}

func TestDeleteOutput(t *testing.T) {
	f := newFixture(t)
	d := f.add("x")
	code, out, errOut := f.run("", "delete", fmt.Sprint(d.ID))
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("code %d stdout %q stderr %q", code, out, errOut)
	}
	if code, _, _ := f.run("", "get", fmt.Sprint(d.ID)); code != 1 {
		t.Fatalf("get after delete code %d", code)
	}
}

func TestGetDeleteUnknownID(t *testing.T) {
	f := newFixture(t)
	for _, cmd := range []string{"get", "delete"} {
		if code, _, errOut := f.run("", cmd, "99"); code != 1 || !strings.HasPrefix(errOut, "docs: ") {
			t.Errorf("%s: code %d stderr %q", cmd, code, errOut)
		}
	}
}

func TestNonNumericID(t *testing.T) {
	f := newFixture(t)
	for _, cmd := range []string{"get", "delete"} {
		if code, _, _ := f.run("", cmd, "abc"); code != 2 {
			t.Errorf("%s abc: code %d", cmd, code)
		}
		if code, _, _ := f.run("", cmd); code != 2 {
			t.Errorf("%s without id: code %d", cmd, code)
		}
	}
	// Flags may follow the ID.
	d := f.add("x")
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"get", fmt.Sprint(d.ID), "--data", f.path}, nil, &out, &errOut); code != 0 {
		t.Errorf("get ID --data: code %d %q", code, errOut.String())
	}
}

func listIDs(t *testing.T, f *fixture, cmd string, args ...string) []int64 {
	t.Helper()
	code, out, errOut := f.run("", cmd, args...)
	if code != 0 {
		t.Fatalf("%s %v: code %d stderr %q", cmd, args, code, errOut)
	}
	var docs []store.Document
	if err := json.Unmarshal([]byte(out), &docs); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	ids := []int64{}
	for _, d := range docs {
		ids = append(ids, d.ID)
	}
	return ids
}

func TestFilters(t *testing.T) {
	f := newFixture(t)
	a := f.add("Alpha Guide", "--tag", "go", "--tag", "web")
	b := f.add("Beta", "--tag", "Go")
	c := f.add("Gamma", "--tag", "web")
	eq := func(got []int64, want ...int64) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	eq(listIDs(t, f, "list"), c.ID, b.ID, a.ID)
	eq(listIDs(t, f, "search", "--tag", "GO"), b.ID, a.ID)
	eq(listIDs(t, f, "search", "--tag", "go", "--tag", "web"), a.ID)
	eq(listIDs(t, f, "search", "--q", "  ALPHA "), a.ID)
	eq(listIDs(t, f, "search", "--q", "body", "--tag", "web"), c.ID, a.ID)
	eq(listIDs(t, f, "search", "--q", "alpha", "--tag", "nope"))
	code, out, _ := f.run("", "search", "--tag", "nope")
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty result: %d %q", code, out)
	}
	for _, args := range [][]string{
		{"list", "--tag", ""}, {"list", "--q", "  "}, {"list", "--tag", "a,b"},
		{"search", "--tag", ""}, {"search", "--q", "  "}, {"search"},
	} {
		code, out, errOut := f.run("", args[0], args[1:]...)
		if code != 2 || out != "" || !strings.HasPrefix(errOut, "docs: ") {
			t.Errorf("%v: code %d stdout %q stderr %q", args, code, out, errOut)
		}
	}
}

func TestListSummary(t *testing.T) {
	f := newFixture(t)
	f.add("x", "--tag", "go")
	code, out, _ := f.run("", "list", "--summary")
	if code != 0 || strings.Contains(out, "content") || !strings.Contains(out, `"title"`) {
		t.Fatalf("code %d out %q", code, out)
	}
	code, out, _ = f.run("", "list")
	if code != 0 || !strings.Contains(out, `"content"`) {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestSuccessStderrEmpty(t *testing.T) {
	f := newFixture(t)
	d := f.add("x")
	for _, args := range [][]string{{"list"}, {"get", fmt.Sprint(d.ID)}, {"search", "--q", "x"}} {
		code, out, errOut := f.run("", args[0], args[1:]...)
		if code != 0 || errOut != "" || !json.Valid([]byte(out)) {
			t.Errorf("%v: code %d stdout %q stderr %q", args, code, out, errOut)
		}
	}
	// Diagnostics are a single "docs: ..." line even if the error text has newlines.
	_, _, errOut := f.run("", "get", "999")
	if strings.Count(errOut, "\n") != 1 || !strings.HasPrefix(errOut, "docs: ") {
		t.Fatalf("stderr %q", errOut)
	}
	if got := oneLine("a\nb\r\nc"); got != "a b c" {
		t.Fatalf("oneLine = %q", got)
	}
}

func TestCorruptExit1(t *testing.T) {
	f := newFixture(t)
	orig := []byte(`{"next_id":1,"documents":[{"id":3,"title":"x","content":"","tags":[],"created_at":"2024-01-01T00:00:00Z"}]}`)
	if err := os.WriteFile(f.path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	cmds := [][]string{{"list"}, {"add", "--title", "t"}, {"get", "3"}, {"search", "--q", "x"}, {"delete", "3"}}
	for _, args := range cmds {
		code, out, errOut := f.run("", args[0], args[1:]...)
		if code != 1 || out != "" || !strings.Contains(errOut, f.path) || !strings.HasPrefix(errOut, "docs: ") {
			t.Errorf("%v: code %d stdout %q stderr %q", args, code, out, errOut)
		}
		if got, _ := os.ReadFile(f.path); !bytes.Equal(got, orig) {
			t.Fatalf("%v changed the file", args)
		}
	}
}

func TestLockTimeoutCLI(t *testing.T) {
	f := newFixture(t)
	lock := f.path + ".lock"
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	code, _, errOut := f.run("", "add", "--title", "x")
	elapsed := time.Since(start)
	if code != 1 || elapsed < time.Second || elapsed > 3*time.Second {
		t.Fatalf("code %d after %v", code, elapsed)
	}
	if !strings.Contains(errOut, lock) || !strings.Contains(errOut, "may be deleted if no other instance is running") {
		t.Fatalf("stderr %q", errOut)
	}
}

func TestExitCodeMapping(t *testing.T) {
	ve := &store.ValidationError{Field: "f", Message: "m"}
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"validation", ve, 2},
		{"wrapped validation", fmt.Errorf("x: %w", ve), 2},
		{"usage", usagef("bad"), 2},
		{"wrapped usage", fmt.Errorf("x: %w", usagef("bad")), 2},
		{"not found", store.ErrNotFound, 1},
		{"wrapped not found", fmt.Errorf("x: %w", store.ErrNotFound), 1},
		{"corrupt", store.ErrCorrupt, 1},
		{"lock timeout", fmt.Errorf("x: %w", store.ErrLockTimeout), 1},
		{"canceled", context.Canceled, 1},
		{"joined", errors.Join(store.ErrNotFound, io.EOF), 1},
		{"joined with validation", errors.Join(io.EOF, ve), 2},
	} {
		if got := exitCode(tc.err); got != tc.want {
			t.Errorf("%s: exitCode = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestConcurrentMixed runs 40 writers (half Store.Add, half CLI Run) and 20 readers on a 100-document file.
func TestConcurrentMixed(t *testing.T) {
	f := newFixture(t)
	fd := map[string]any{"next_id": 101}
	docs := []store.Document{}
	for i := 1; i <= 100; i++ {
		docs = append(docs, store.Document{ID: int64(i), Title: fmt.Sprint("fixture ", i), Tags: []string{}, CreatedAt: time.Now().UTC()})
	}
	fd["documents"] = docs
	b, err := json.Marshal(fd)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var readers, writers sync.WaitGroup
	for r := 0; r < 20; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				var out, errOut bytes.Buffer
				if code := Run(context.Background(), []string{"list", "--summary", "--data", f.path}, nil, &out, &errOut); code != 0 {
					t.Errorf("reader: code %d %q", code, errOut.String())
					return
				}
			}
		}()
	}
	for i := 0; i < 40; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			if i%2 == 0 {
				s, err := store.Open(context.Background(), store.Options{Path: f.path, LockTimeout: 30 * time.Second})
				if err == nil {
					_, err = s.Add(context.Background(), store.Input{Title: fmt.Sprint("store ", i)})
				}
				if err != nil {
					t.Errorf("store writer %d: %v", i, err)
				}
				return
			}
			var out, errOut bytes.Buffer
			args := []string{"add", "--title", fmt.Sprint("cli ", i), "--data", f.path, "--lock-timeout", "30s"}
			if code := Run(context.Background(), args, strings.NewReader(""), &out, &errOut); code != 0 {
				t.Errorf("cli writer %d: code %d %q", i, code, errOut.String())
			}
		}()
	}
	writers.Wait()
	close(done)
	readers.Wait()

	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	var final struct {
		Documents []store.Document `json:"documents"`
	}
	if err := json.Unmarshal(raw, &final); err != nil {
		t.Fatalf("final file invalid: %v", err)
	}
	ids := map[int64]bool{}
	for _, d := range final.Documents {
		ids[d.ID] = true
	}
	if len(final.Documents) != 140 || len(ids) != 140 {
		t.Fatalf("documents %d, distinct ids %d, want 140", len(final.Documents), len(ids))
	}
}
