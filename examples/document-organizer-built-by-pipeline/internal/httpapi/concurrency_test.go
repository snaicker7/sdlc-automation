package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"docs/internal/cli"
	"docs/internal/store"
)

func TestMixedConcurrency(t *testing.T) {
	const seeded, writers, readers = 100, 40, 20
	e := newEnv(t, 60*time.Second)
	for i := 0; i < seeded; i++ {
		e.seed(t, fmt.Sprintf("seed %d", i))
	}
	srv := httptest.NewServer(e.h)
	defer srv.Close()

	errs := make(chan error, writers+readers)
	var writing, reading sync.WaitGroup
	var done atomic.Bool

	for i := 0; i < writers; i++ {
		writing.Add(1)
		go func() {
			defer writing.Done()
			title := fmt.Sprintf("w%d", i)
			if i%2 == 0 {
				var out, stderr strings.Builder
				args := []string{"add", "--title", title, "--data", e.path, "--lock-timeout", "60s"}
				if code := cli.Run(context.Background(), args, strings.NewReader("c"), &out, &stderr); code != 0 {
					errs <- fmt.Errorf("cli add %d: exit %d: %s", i, code, stderr.String())
				}
				return
			}
			body := fmt.Sprintf(`{"title":%q,"content":"c"}`, title)
			resp, err := http.Post(srv.URL+"/documents", "application/json", strings.NewReader(body))
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 201 {
				b, _ := io.ReadAll(resp.Body)
				errs <- fmt.Errorf("POST %d: %d %s", i, resp.StatusCode, b)
			}
		}()
	}
	for i := 0; i < readers; i++ {
		reading.Add(1)
		go func() {
			defer reading.Done()
			for !done.Load() {
				resp, err := http.Get(srv.URL + "/documents")
				if err != nil {
					errs <- err
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					errs <- fmt.Errorf("GET: %d", resp.StatusCode)
					return
				}
			}
		}()
	}
	writing.Wait()
	done.Store(true)
	reading.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	raw, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatal(err)
	}
	var fd struct {
		Documents []store.Document `json:"documents"`
	}
	if err := json.Unmarshal(raw, &fd); err != nil {
		t.Fatalf("final file invalid: %v", err)
	}
	ids := map[int64]bool{}
	for _, d := range fd.Documents {
		ids[d.ID] = true
	}
	if len(fd.Documents) != seeded+writers || len(ids) != seeded+writers {
		t.Errorf("documents=%d distinct ids=%d, want %d", len(fd.Documents), len(ids), seeded+writers)
	}
}
