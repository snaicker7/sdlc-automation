package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var ctxBG = context.Background()

func openAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(ctxBG, Options{Path: path, LockTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docs.json")
	return openAt(t, path), path
}

func mustAdd(t *testing.T, s *Store, title string, tags ...string) Document {
	t.Helper()
	d, err := s.Add(ctxBG, Input{Title: title, Tags: tags})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func readData(t *testing.T, path string) fileData {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fd fileData
	if err := json.Unmarshal(b, &fd); err != nil {
		t.Fatalf("data file is not valid JSON: %v", err)
	}
	return fd
}

func writeFixture(t *testing.T, path string, n int) {
	t.Helper()
	fd := fileData{NextID: int64(n + 1), Documents: []Document{}}
	for i := 1; i <= n; i++ {
		fd.Documents = append(fd.Documents, Document{ID: int64(i), Title: fmt.Sprintf("fixture %d", i), Tags: []string{}, CreatedAt: time.Now().UTC()})
	}
	b, err := json.Marshal(fd)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOpenValidatesOptions(t *testing.T) {
	for _, o := range []Options{{Path: "", LockTimeout: time.Second}, {Path: "x", LockTimeout: 0}} {
		_, err := Open(ctxBG, o)
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("Open(%+v) err = %v, want ValidationError", o, err)
		}
	}
}

func TestMissingFileEmptyStore(t *testing.T) {
	s, path := openStore(t)
	docs, err := s.List(ctxBG, Filter{})
	if err != nil || len(docs) != 0 {
		t.Fatalf("List = %v, %v", docs, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read created the file: %v", err)
	}
	d := mustAdd(t, s, "first")
	if d.ID != 1 {
		t.Fatalf("first id = %d", d.ID)
	}
	if fd := readData(t, path); fd.NextID != 2 || len(fd.Documents) != 1 {
		t.Fatalf("file = %+v", fd)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	s, path := openStore(t)
	d := mustAdd(t, s, "keep", " Go ")
	s2 := openAt(t, path)
	docs, err := s2.List(ctxBG, Filter{})
	if err != nil || len(docs) != 1 || docs[0].ID != d.ID || docs[0].Title != "keep" || docs[0].Tags[0] != "go" {
		t.Fatalf("List = %+v, %v", docs, err)
	}
}

func TestNextIDNeverDecremented(t *testing.T) {
	s, path := openStore(t)
	mustAdd(t, s, "a")
	b := mustAdd(t, s, "b")
	if err := s.Delete(ctxBG, b.ID); err != nil {
		t.Fatal(err)
	}
	if fd := readData(t, path); fd.NextID != b.ID+1 {
		t.Fatalf("next_id after delete = %d, want %d", fd.NextID, b.ID+1)
	}
	c := mustAdd(t, openAt(t, path), "c")
	if c.ID <= b.ID {
		t.Fatalf("new id %d not greater than deleted %d", c.ID, b.ID)
	}
}

func TestGetDeleteNotFound(t *testing.T) {
	s, _ := openStore(t)
	if _, err := s.Get(ctxBG, 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get err = %v", err)
	}
	if err := s.Delete(ctxBG, 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete err = %v", err)
	}
	d := mustAdd(t, s, "x")
	got, err := s.Get(ctxBG, d.ID)
	if err != nil || got.Title != "x" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if err := s.Delete(ctxBG, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctxBG, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete err = %v", err)
	}
}

func assertCorrupt(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s := openAt(t, path)
	ops := map[string]func() error{
		"List":   func() error { _, err := s.List(ctxBG, Filter{}); return err },
		"Add":    func() error { _, err := s.Add(ctxBG, Input{Title: "t"}); return err },
		"Get":    func() error { _, err := s.Get(ctxBG, 1); return err },
		"Delete": func() error { return s.Delete(ctxBG, 1) },
	}
	for name, op := range ops {
		err := op()
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("%s err = %v, want ErrCorrupt", name, err)
		}
		if !strings.Contains(err.Error(), path) {
			t.Fatalf("%s error does not name file: %v", name, err)
		}
		got, rerr := os.ReadFile(path)
		if rerr != nil || !bytes.Equal(got, []byte(content)) {
			t.Fatalf("%s changed the file: %q, %v", name, got, rerr)
		}
		if _, serr := os.Stat(path + ".lock"); !errors.Is(serr, os.ErrNotExist) {
			t.Fatalf("%s left a lock: %v", name, serr)
		}
	}
}

func TestCorruptInvalidJSON(t *testing.T) {
	assertCorrupt(t, filepath.Join(t.TempDir(), "docs.json"), "{not json")
}

func TestCorruptNextIDTooLow(t *testing.T) {
	doc := `{"id":5,"title":"x","content":"","tags":[],"created_at":"2024-01-01T00:00:00Z"}`
	assertCorrupt(t, filepath.Join(t.TempDir(), "docs.json"), `{"next_id":5,"documents":[`+doc+`]}`)
}

func TestReadRefreshOnChange(t *testing.T) {
	s1, path := openStore(t)
	mustAdd(t, s1, "one")
	if docs, _ := s1.List(ctxBG, Filter{}); len(docs) != 1 {
		t.Fatalf("len = %d", len(docs))
	}
	mustAdd(t, openAt(t, path), "two with a longer title")
	docs, err := s1.List(ctxBG, Filter{})
	if err != nil || len(docs) != 2 {
		t.Fatalf("List after external write = %d docs, %v", len(docs), err)
	}
}

func TestReaderYieldsToLockThenReadsAnyway(t *testing.T) {
	s, path := openStore(t)
	mustAdd(t, s, "one")
	reader := openAt(t, path)
	clk := &fakeClock{t: time.Now()}
	clk.install(reader)
	if err := os.WriteFile(path+".lock", nil, 0o600); err != nil { // a stuck lock must not block reads forever
		t.Fatal(err)
	}
	docs, err := reader.List(ctxBG, Filter{})
	if err != nil || len(docs) != 1 {
		t.Fatalf("List = %v, %v", docs, err)
	}
	var slept time.Duration
	for _, d := range clk.sleeps {
		slept += d
	}
	if slept < readerYield {
		t.Fatalf("reader yielded only %v", slept)
	}
}

func TestInvalidInputNeverLocks(t *testing.T) {
	s, path := openStore(t)
	// A held lock must not matter: validation happens before locking.
	if err := os.WriteFile(path+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := s.Add(ctxBG, Input{Title: "  "})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("validation waited for the lock")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("data file created: %v", err)
	}
}

func TestConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docs.json")
	writeFixture(t, path, 100)
	const writers, readers = 40, 20
	runConcurrent(t, path, writers, readers, func(i int) error {
		s, err := Open(ctxBG, Options{Path: path, LockTimeout: 30 * time.Second})
		if err != nil {
			return err
		}
		_, err = s.Add(ctxBG, Input{Title: fmt.Sprintf("w%d", i)})
		return err
	})
	fd := readData(t, path)
	if len(fd.Documents) != 140 {
		t.Fatalf("documents = %d, want 140", len(fd.Documents))
	}
	seen := map[int64]bool{}
	for _, d := range fd.Documents {
		seen[d.ID] = true
	}
	if len(seen) != 140 {
		t.Fatalf("distinct ids = %d, want 140", len(seen))
	}
}

// runConcurrent runs writers writer calls and readers looping List until all writers finish.
func runConcurrent(t *testing.T, path string, writers, readers int, write func(i int) error) {
	t.Helper()
	done := make(chan struct{})
	var rw, ww sync.WaitGroup
	for r := 0; r < readers; r++ {
		rw.Add(1)
		go func() {
			defer rw.Done()
			s, err := Open(ctxBG, Options{Path: path, LockTimeout: time.Second})
			if err != nil {
				t.Errorf("reader open: %v", err)
				return
			}
			for {
				select {
				case <-done:
					return
				default:
				}
				if _, err := s.List(ctxBG, Filter{}); err != nil {
					t.Errorf("reader: %v", err)
					return
				}
			}
		}()
	}
	for i := 0; i < writers; i++ {
		ww.Add(1)
		go func() {
			defer ww.Done()
			if err := write(i); err != nil {
				t.Errorf("writer %d: %v", i, err)
			}
		}()
	}
	ww.Wait()
	close(done)
	rw.Wait()
}
