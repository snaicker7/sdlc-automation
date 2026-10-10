package cli

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"docs/internal/httpapi"
	"docs/internal/store"
)

func TestParseListenAddr(t *testing.T) {
	good := map[string]string{
		"127.0.0.1:8080": "127.0.0.1:8080",
		"localhost:8080": "127.0.0.1:8080",
		"[::1]:8080":     "[::1]:8080",
	}
	if defaultAddr != "127.0.0.1:8080" {
		t.Errorf("defaultAddr = %q", defaultAddr)
	}
	for in, want := range good {
		got, err := parseListenAddr(in)
		if err != nil || got != want {
			t.Errorf("parseListenAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "192.168.1.5:8080", "example.com:8080",
		"[fe80::1%eth0]:8080", "[::ffff:127.0.0.1]:8080", "127.0.0.1:http", "127.0.0.1", ""} {
		if got, err := parseListenAddr(in); err == nil {
			t.Errorf("parseListenAddr(%q) = %q, want error", in, got)
		}
	}
}

func TestServeBadAddrExits2(t *testing.T) {
	for _, a := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "192.168.1.5:8080", "example.com:8080", "[fe80::1%eth0]:8080", "[::ffff:127.0.0.1]:8080"} {
		var out, errb strings.Builder
		data := filepath.Join(t.TempDir(), "d.json")
		code := Run(context.Background(), []string{"serve", "--addr", a, "--data", data}, strings.NewReader(""), &out, &errb)
		if code != 2 {
			t.Errorf("serve --addr %s = %d, want 2 (%s)", a, code, errb.String())
		}
		if strings.Contains(errb.String(), "listening") {
			t.Errorf("serve --addr %s started listening", a)
		}
	}
}

func TestServeConstants(t *testing.T) {
	if readHeaderTimeout != 5*time.Second || shutdownTimeout != 30*time.Second {
		t.Errorf("readHeaderTimeout=%v shutdownTimeout=%v", readHeaderTimeout, shutdownTimeout)
	}
}

// startServe runs serve on a loopback listener and returns its address, the data path and a stop func.
func startServe(t *testing.T, wrap func(http.Handler) http.Handler, timeout time.Duration) (addr, path string, cancel context.CancelFunc, result <-chan error) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "docs.json")
	st, err := store.Open(context.Background(), store.Options{Path: path, LockTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := httpapi.NewHandler(st)
	if wrap != nil {
		h = wrap(h)
	}
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- serve(ctx, ln, h, timeout) }()
	t.Cleanup(cancel)
	return ln.Addr().String(), path, cancel, res
}

// raw sends request bytes and returns the status line (or "" if the connection closed without one).
func raw(t *testing.T, addr, req string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line)
}

func TestServeRawConnHostHandling(t *testing.T) {
	addr, path, _, _ := startServe(t, nil, time.Second)
	cases := []struct{ name, req, want string }{
		{"no host 1.1", "GET /documents HTTP/1.1\r\n\r\n", "HTTP/1.1 400"},
		{"empty host", "GET /documents HTTP/1.1\r\nHost:\r\n\r\n", "HTTP/1.1 403"},
		{"no host 1.0", "GET /documents HTTP/1.0\r\n\r\n", "HTTP/1.0 403"},
		{"evil host", "GET /documents HTTP/1.1\r\nHost: evil.example\r\n\r\n", "HTTP/1.1 403"},
		{"cross-origin post", "POST /documents HTTP/1.1\r\nHost: localhost\r\nOrigin: http://evil.example\r\nContent-Type: text/plain\r\nContent-Length: 16\r\n\r\n{\"title\":\"evil\"}", "HTTP/1.1 403"},
	}
	for _, c := range cases {
		if got := raw(t, addr, c.req); !strings.HasPrefix(got, c.want) {
			t.Errorf("%s: status line %q, want prefix %q", c.name, got, c.want)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("data file exists after rejected requests: %v", err)
	}
}

func TestGracefulShutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	slow := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-release
			h.ServeHTTP(w, r)
		})
	}
	addr, _, cancel, result := startServe(t, slow, 10*time.Second)

	status := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/documents")
		if err != nil {
			status <- -1
			return
		}
		resp.Body.Close()
		status <- resp.StatusCode
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		t.Fatalf("serve returned %v before in-flight request finished", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if got := <-status; got != 200 {
		t.Errorf("in-flight request = %d, want 200", got)
	}
	if err := <-result; err != nil {
		t.Errorf("serve = %v, want nil", err)
	}
}

func TestServeShutdownError(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	stuck := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			close(started)
			<-release
		})
	}
	addr, _, cancel, result := startServe(t, stuck, 50*time.Millisecond)
	defer close(release)
	go http.Get("http://" + addr + "/documents")
	<-started
	cancel()
	if err := <-result; err == nil {
		t.Error("serve = nil, want shutdown error")
	} else if exitCode(err) != 1 {
		t.Errorf("exit code = %d, want 1", exitCode(err))
	}
}
