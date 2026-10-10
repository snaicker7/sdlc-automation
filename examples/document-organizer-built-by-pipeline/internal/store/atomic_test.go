package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// failFile wraps a real temp file and fails the chosen method after doing the real work.
type failFile struct {
	tmpFile
	write, sync, close error
}

func (f *failFile) Write(p []byte) (int, error) {
	if f.write != nil {
		return 0, f.write
	}
	return f.tmpFile.Write(p)
}

func (f *failFile) Sync() error {
	if f.sync != nil {
		return f.sync
	}
	return f.tmpFile.Sync()
}

func (f *failFile) Close() error {
	err := f.tmpFile.Close()
	if f.close != nil {
		return f.close
	}
	return err
}

func tmpLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, ".tmp*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// seeded returns a store whose data file already holds one document.
func seeded(t *testing.T) (*Store, string, []byte) {
	t.Helper()
	s, path := openStore(t)
	mustAdd(t, s, "original")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, path, b
}

func assertUntouched(t *testing.T, path string, orig []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, orig) {
		t.Fatalf("data file changed: %q, %v", got, err)
	}
	if left := tmpLeftovers(t, filepath.Dir(path)); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock left: %v", err)
	}
}

func TestAtomicWriteFailures(t *testing.T) {
	real := func(dir, pattern string) (tmpFile, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		return f, nil
	}
	cases := map[string]func(dir, pattern string) (tmpFile, error){
		"createTemp": failingCreateTemp,
		"write": func(d, p string) (tmpFile, error) {
			f, err := real(d, p)
			return &failFile{tmpFile: f, write: errBoom}, err
		},
		"sync": func(d, p string) (tmpFile, error) {
			f, err := real(d, p)
			return &failFile{tmpFile: f, sync: errBoom}, err
		},
		"close": func(d, p string) (tmpFile, error) {
			f, err := real(d, p)
			return &failFile{tmpFile: f, close: errBoom}, err
		},
	}
	for name, ct := range cases {
		t.Run(name, func(t *testing.T) {
			s, path, orig := seeded(t)
			s.env.createTemp = ct
			_, err := s.Add(ctxBG, Input{Title: "new"})
			if !errors.Is(err, errBoom) {
				t.Fatalf("err = %v, want errBoom", err)
			}
			assertUntouched(t, path, orig)
		})
	}
}

func TestReadOnlyDir(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permissions cannot be enforced here")
	}
	s, path, orig := seeded(t)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := s.Add(ctxBG, Input{Title: "new"}); err == nil {
		t.Fatal("Add succeeded in a read-only dir")
	}
	assertUntouched(t, path, orig)
}

func TestStrayTmpNeverRead(t *testing.T) {
	s, path, _ := seeded(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), ".tmp12345"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	docs, err := s.List(ctxBG, Filter{})
	if err != nil || len(docs) != 1 {
		t.Fatalf("List = %v, %v", docs, err)
	}
	mustAdd(t, openAt(t, path), "more")
}

var errViolation = errors.New("sharing violation")

// useFakeViolations makes errViolation count as a sharing violation for the test's duration.
func useFakeViolations(t *testing.T) {
	t.Helper()
	old := isSharingViolation
	isSharingViolation = func(err error) bool { return errors.Is(err, errViolation) }
	t.Cleanup(func() { isSharingViolation = old })
}

type fakeClock struct {
	t      time.Time
	sleeps []time.Duration
}

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) sleep(_ context.Context, d time.Duration) error {
	c.sleeps = append(c.sleeps, d)
	c.t = c.t.Add(d)
	return nil
}

func (c *fakeClock) install(s *Store) {
	s.env.now = c.now
	s.env.sleep = c.sleep
}

func TestRenameRetrySucceeds(t *testing.T) {
	useFakeViolations(t)
	s, path, _ := seeded(t)
	clk := &fakeClock{t: time.Now()}
	clk.install(s)
	calls := 0
	s.env.rename = func(o, n string) error {
		calls++
		if calls <= 5 {
			return &os.LinkError{Op: "rename", Old: o, New: n, Err: errViolation}
		}
		return os.Rename(o, n)
	}
	mustAdd(t, s, "second")
	if calls != 6 || len(clk.sleeps) != 5 {
		t.Fatalf("calls = %d, sleeps = %d", calls, len(clk.sleeps))
	}
	for _, d := range clk.sleeps {
		if d < 25*time.Millisecond || d > 100*time.Millisecond {
			t.Fatalf("sleep %v outside 25-100ms", d)
		}
	}
	if fd := readData(t, path); len(fd.Documents) != 2 {
		t.Fatalf("documents = %d", len(fd.Documents))
	}
	if left := tmpLeftovers(t, filepath.Dir(path)); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestRenameRetryExhausted(t *testing.T) {
	useFakeViolations(t)
	s, path, orig := seeded(t)
	clk := &fakeClock{t: time.Now()}
	start := clk.t
	clk.install(s)
	s.env.rename = func(string, string) error { return errViolation }
	_, err := s.Add(ctxBG, Input{Title: "second"})
	if !errors.Is(err, errViolation) {
		t.Fatalf("err = %v", err)
	}
	if clk.t.Sub(start) < 5*time.Second {
		t.Fatalf("gave up after only %v", clk.t.Sub(start))
	}
	assertUntouched(t, path, orig)
}

func TestRenameNonSharingErrorNotRetried(t *testing.T) {
	useFakeViolations(t)
	s, path, orig := seeded(t)
	clk := &fakeClock{t: time.Now()}
	clk.install(s)
	calls := 0
	s.env.rename = func(string, string) error { calls++; return errBoom }
	if _, err := s.Add(ctxBG, Input{Title: "second"}); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if calls != 1 || len(clk.sleeps) != 0 {
		t.Fatalf("calls = %d, sleeps = %d", calls, len(clk.sleeps))
	}
	assertUntouched(t, path, orig)
}

func TestReaderRetriesSharingViolation(t *testing.T) {
	useFakeViolations(t)
	s, path, _ := seeded(t)
	clk := &fakeClock{t: time.Now()}
	clk.install(s)
	reader := openAt(t, path)
	clk.install(reader)
	calls := 0
	reader.env.readFile = func(name string) ([]byte, error) {
		calls++
		if calls <= 2 {
			return nil, &os.PathError{Op: "open", Path: name, Err: errViolation}
		}
		return os.ReadFile(name)
	}
	docs, err := reader.List(ctxBG, Filter{})
	if err != nil || len(docs) != 1 {
		t.Fatalf("List = %v, %v", docs, err)
	}
	if calls != 3 || len(clk.sleeps) != 2 {
		t.Fatalf("calls = %d, sleeps = %d", calls, len(clk.sleeps))
	}
}

func TestWriteAtomicNamesTempWithPrefix(t *testing.T) {
	s, path := openStore(t)
	var name string
	s.env.createTemp = func(dir, pattern string) (tmpFile, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		name = filepath.Base(f.Name())
		return f, nil
	}
	mustAdd(t, s, "x")
	if !strings.HasPrefix(name, ".tmp") {
		t.Fatalf("temp name = %q", name)
	}
	_ = path
}
