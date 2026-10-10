package httpapi_test

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestHostGuard(t *testing.T) {
	e := newEnv(t, time.Second)
	for _, h := range []string{"localhost:8080", "127.0.0.1:8080", "[::1]:8080", "[::1]", "LOCALHOST:1", "127.0.0.1:9999", "LocalHost"} {
		if rec := e.do("GET", "/documents", opts{host: h}); rec.Code != 200 {
			t.Errorf("Host %q = %d, want 200", h, rec.Code)
		}
	}
	for _, h := range []string{"evil.example", "localhost.evil.example", "evil.example:8080", "127.0.0.1.evil.example"} {
		rec := e.do("GET", "/documents", opts{host: h})
		if rec.Code != 403 {
			t.Errorf("Host %q = %d, want 403", h, rec.Code)
		}
		assertErrorShape(t, rec)
	}
	rec := e.do("GET", "/documents", opts{host: " "})
	if rec.Code != 403 {
		t.Errorf("blank Host = %d, want 403", rec.Code)
	}
}

func TestHostGuardEmptyHost(t *testing.T) {
	e := newEnv(t, time.Second)
	req := httptest.NewRequest("GET", "/documents", nil)
	req.Host = ""
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("empty Host = %d, want 403", rec.Code)
	}
	assertErrorShape(t, rec)
}

func TestOriginGuard(t *testing.T) {
	e := newEnv(t, time.Second)
	e.seed(t, "keep")
	before := e.fileBytes(t)
	for _, o := range []string{"http://evil.example", "null", "http://[::1", "://bad", "http://localhost.evil.example"} {
		post := e.do("POST", "/documents", opts{ctype: jsonCT, origin: o, body: goodBody})
		del := e.do("DELETE", "/documents/1", opts{origin: o})
		if post.Code != 403 || del.Code != 403 {
			t.Errorf("Origin %q: POST=%d DELETE=%d, want 403", o, post.Code, del.Code)
		}
		assertErrorShape(t, post)
	}
	if string(e.fileBytes(t)) != string(before) {
		t.Error("file changed after rejected origin")
	}
	for _, o := range []string{"http://localhost:8080", "http://127.0.0.1:3000", ""} {
		if rec := e.do("POST", "/documents", opts{ctype: jsonCT, origin: o, body: goodBody}); rec.Code != 201 {
			t.Errorf("Origin %q = %d, want 201", o, rec.Code)
		}
	}
}

func TestContentTypeGuard(t *testing.T) {
	e := newEnv(t, time.Second)
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", ""} {
		rec := e.do("POST", "/documents", opts{ctype: ct, body: goodBody})
		if rec.Code != 415 {
			t.Errorf("Content-Type %q = %d, want 415", ct, rec.Code)
		}
		assertErrorShape(t, rec)
	}
	if b := e.fileBytes(t); b != nil {
		t.Errorf("persisted after 415: %s", b)
	}
	for _, ct := range []string{"application/json", "application/json; charset=utf-8"} {
		if rec := e.do("POST", "/documents", opts{ctype: ct, body: goodBody}); rec.Code != 201 {
			t.Errorf("Content-Type %q = %d, want 201", ct, rec.Code)
		}
	}
	if rec := e.do("PUT", "/documents", opts{ctype: "text/plain", body: goodBody}); rec.Code != 405 {
		t.Errorf("PUT text/plain = %d, want 405", rec.Code)
	}
}
