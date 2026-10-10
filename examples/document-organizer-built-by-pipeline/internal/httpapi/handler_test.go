package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"docs/internal/httpapi"
	"docs/internal/store"
)

type env struct {
	path string
	st   *store.Store
	h    http.Handler
}

func newEnv(t *testing.T, lockTimeout time.Duration) *env {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docs.json")
	st, err := store.Open(context.Background(), store.Options{Path: path, LockTimeout: lockTimeout})
	if err != nil {
		t.Fatal(err)
	}
	return &env{path: path, st: st, h: httpapi.NewHandler(st)}
}

// fileBytes returns the data file's content, or nil if it does not exist.
func (e *env) fileBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(e.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (e *env) seed(t *testing.T, title string, tags ...string) store.Document {
	t.Helper()
	d, err := e.st.Add(context.Background(), store.Input{Title: title, Content: "body of " + title, Tags: tags})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

type opts struct {
	host   string
	ctype  string
	origin string
	body   string
}

const jsonCT = "application/json"

func (e *env) do(method, target string, o opts) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(o.body))
	req.Host = "localhost:8080"
	if o.host != "" {
		req.Host = o.host
	}
	if o.ctype != "" {
		req.Header.Set("Content-Type", o.ctype)
	}
	if o.origin != "" {
		req.Header.Set("Origin", o.origin)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) post(body string) *httptest.ResponseRecorder {
	return e.do("POST", "/documents", opts{ctype: jsonCT, body: body})
}

const goodBody = `{"title":"hello","content":"world","tags":["a"]}`

func assertErrorShape(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if v := rec.Header().Get("X-Content-Type-Options"); v != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", v)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
		t.Errorf("body %q is not a JSON error: %v", rec.Body.String(), err)
	}
}

func TestRouting(t *testing.T) {
	e := newEnv(t, time.Second)
	rec := e.post(goodBody)
	if rec.Code != 201 {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	var doc store.Document
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || doc.Title != "hello" || doc.ID != 1 {
		t.Fatalf("created = %+v, %v", doc, err)
	}
	if rec := e.do("GET", "/documents/1", opts{}); rec.Code != 200 {
		t.Errorf("GET id = %d", rec.Code)
	}
	e.seed(t, "second", "b")

	want, _ := e.st.List(context.Background(), store.Filter{})
	wantJSON, _ := json.Marshal(want)
	rec = e.do("GET", "/documents", opts{})
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != string(wantJSON) {
		t.Errorf("GET list = %d %s, want %s", rec.Code, rec.Body, wantJSON)
	}

	rec = e.do("GET", "/documents?tag=b&q=SECOND", opts{})
	var got []store.Document
	if err := json.Unmarshal(rec.Body.Bytes(), &got); rec.Code != 200 || err != nil || len(got) != 1 || got[0].Title != "second" {
		t.Errorf("filter = %d %s", rec.Code, rec.Body)
	}
	rec = e.do("GET", "/documents?tag=nomatch", opts{})
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("zero match = %d %q", rec.Code, rec.Body)
	}
	rec = e.do("GET", "/documents?foo=1", opts{})
	if rec.Code != 200 || strings.Count(rec.Body.String(), `"id"`) != 2 {
		t.Errorf("unknown param = %d %s", rec.Code, rec.Body)
	}
	for _, q := range []string{"?tag=", "?q=", "?q=%20", "?tag=a,b"} {
		if rec := e.do("GET", "/documents"+q, opts{}); rec.Code != 400 {
			t.Errorf("GET %s = %d, want 400", q, rec.Code)
		} else {
			assertErrorShape(t, rec)
		}
	}

	if rec := e.do("DELETE", "/documents/1", opts{}); rec.Code != 204 || rec.Body.Len() != 0 {
		t.Errorf("DELETE = %d %q", rec.Code, rec.Body)
	}
	for _, c := range []struct{ method, target string }{
		{"PUT", "/documents"}, {"PATCH", "/documents/2"}, {"OPTIONS", "/documents"},
	} {
		rec := e.do(c.method, c.target, opts{})
		if rec.Code != 405 || rec.Header().Get("Allow") == "" {
			t.Errorf("%s %s = %d Allow=%q", c.method, c.target, rec.Code, rec.Header().Get("Allow"))
		}
		assertErrorShape(t, rec)
	}
	rec = e.do("GET", "/nope", opts{})
	if rec.Code != 404 {
		t.Errorf("unknown path = %d", rec.Code)
	}
	assertErrorShape(t, rec)
}

func TestValidationStatus(t *testing.T) {
	e := newEnv(t, time.Second)
	big := strings.Repeat("x", 1<<20+1)
	cases := []struct {
		name, body string
		want       int
	}{
		{"missing title", `{"content":"c"}`, 400},
		{"long title", `{"title":"` + strings.Repeat("a", 201) + `"}`, 400},
		{"comma tag", `{"title":"t","tags":["a,b"]}`, 400},
		{"malformed", `{"title":`, 400},
		{"unknown field", `{"title":"t","extra":1}`, 400},
		{"content over 1MiB in 1.5MiB body", `{"title":"t","content":"` + big + strings.Repeat("y", 1<<19) + `"}`, 400},
		{"body over 2MiB", `{"title":"t","content":"` + strings.Repeat("x", 2<<20) + `"}`, 413},
	}
	for _, c := range cases {
		rec := e.post(c.body)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, rec.Code, c.want)
			continue
		}
		assertErrorShape(t, rec)
	}
	for _, m := range []string{"GET", "DELETE"} {
		if rec := e.do(m, "/documents/abc", opts{}); rec.Code != 400 {
			t.Errorf("%s non-numeric id = %d", m, rec.Code)
		}
		if rec := e.do(m, "/documents/99", opts{}); rec.Code != 404 {
			t.Errorf("%s unknown id = %d", m, rec.Code)
		} else {
			assertErrorShape(t, rec)
		}
	}
	if b := e.fileBytes(t); b != nil {
		t.Errorf("file written after 4xx: %s", b)
	}
}

func TestStoreErrorMapping(t *testing.T) {
	t.Run("lock timeout", func(t *testing.T) {
		e := newEnv(t, 50*time.Millisecond)
		lock := e.path + ".lock"
		if err := os.WriteFile(lock, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		rec := e.post(goodBody)
		abs, _ := filepath.Abs(lock)
		if rec.Code != 503 || !strings.Contains(rec.Body.String(), strings.ReplaceAll(abs, `\`, `\\`)) {
			t.Errorf("got %d %s, want 503 naming %s", rec.Code, rec.Body, abs)
		}
		assertErrorShape(t, rec)
	})
	t.Run("corrupt file", func(t *testing.T) {
		e := newEnv(t, time.Second)
		corrupt := []byte("{not json")
		if err := os.WriteFile(e.path, corrupt, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, rec := range []*httptest.ResponseRecorder{e.do("GET", "/documents", opts{}), e.post(goodBody)} {
			if rec.Code != 500 {
				t.Errorf("got %d, want 500", rec.Code)
			}
			assertErrorShape(t, rec)
		}
		if got := e.fileBytes(t); string(got) != string(corrupt) {
			t.Errorf("file changed: %q", got)
		}
	})
	t.Run("write failure", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("cannot enforce a read-only directory here")
		}
		e := newEnv(t, time.Second)
		e.seed(t, "orig")
		before := e.fileBytes(t)
		dir := filepath.Dir(e.path)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0o700)
		if rec := e.post(goodBody); rec.Code != 500 {
			t.Errorf("got %d, want 500", rec.Code)
		}
		os.Chmod(dir, 0o700)
		if got := e.fileBytes(t); string(got) != string(before) {
			t.Error("file changed")
		}
	})
}

func TestResponseShape(t *testing.T) {
	e := newEnv(t, time.Second)
	e.seed(t, "x")
	reqs := []*httptest.ResponseRecorder{
		e.do("GET", "/documents", opts{}),
		e.post(goodBody),
		e.do("PUT", "/documents", opts{}),
		e.do("GET", "/nope", opts{}),
		e.do("GET", "/documents", opts{host: "evil.example"}),
		e.do("POST", "/documents", opts{ctype: "text/plain", body: goodBody}),
		e.do("POST", "/documents", opts{ctype: jsonCT, origin: "http://evil.example", body: goodBody}),
	}
	for i, rec := range reqs {
		for k := range rec.Header() {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				t.Errorf("response %d has header %s", i, k)
			}
		}
		if i >= 2 {
			assertErrorShape(t, rec)
		}
	}
	if ct := reqs[0].Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("list Content-Type = %q", ct)
	}
}
