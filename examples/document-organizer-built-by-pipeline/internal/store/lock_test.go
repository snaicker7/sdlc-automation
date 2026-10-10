package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var errBoom = errors.New("boom")

func failingCreateTemp(string, string) (tmpFile, error) { return nil, errBoom }

func TestLockTimeout(t *testing.T) {
	s, path := openStore(t)
	lock := path + ".lock"
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := s.Add(ctxBG, Input{Title: "x"})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
	if elapsed < time.Second || elapsed > 3*time.Second {
		t.Fatalf("elapsed = %v, want 1-3s", elapsed)
	}
	abs, _ := filepath.Abs(lock)
	if !strings.Contains(err.Error(), abs) || !strings.Contains(err.Error(), "may be deleted if no other instance is running") {
		t.Fatalf("message lacks path or hint: %v", err)
	}
	if _, serr := os.Stat(lock); serr != nil {
		t.Fatalf("foreign lock must be left alone: %v", serr)
	}
}

func TestLockRemovedAfterSuccessAndFailure(t *testing.T) {
	s, path := openStore(t)
	mustAdd(t, s, "ok")
	if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock remains after success: %v", err)
	}
	s.env.createTemp = failingCreateTemp
	if _, err := s.Add(ctxBG, Input{Title: "bad"}); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock remains after failure: %v", err)
	}
}

func TestStaleLockRecovered(t *testing.T) {
	s, path := openStore(t)
	lock := path + ".lock"
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-11 * time.Second)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, s, "after stale")
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock remains: %v", err)
	}
}

func TestStaleRecheckSkipsRefreshedLock(t *testing.T) {
	s, path := openStore(t)
	s.lockTimeout = 200 * time.Millisecond
	lock := path + ".lock"
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-11 * time.Second)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	s.env.afterStaleStat = func() {
		now := time.Now()
		if err := os.Chtimes(lock, now, now); err != nil {
			t.Error(err)
		}
	}
	_, err := s.Add(ctxBG, Input{Title: "x"})
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
	if _, serr := os.Stat(lock); serr != nil {
		t.Fatalf("refreshed lock was removed: %v", serr)
	}
}

func TestLockStatSharingViolationIsBusy(t *testing.T) {
	useFakeViolations(t)
	s, path := openStore(t)
	s.lockTimeout = 100 * time.Millisecond
	if err := os.WriteFile(path+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.env.stat = func(name string) (os.FileInfo, error) {
		if strings.HasSuffix(name, ".lock") {
			return nil, &os.PathError{Op: "CreateFile", Path: name, Err: errViolation}
		}
		return os.Stat(name)
	}
	if _, err := s.Add(ctxBG, Input{Title: "x"}); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
}

func TestLockWaitContextCancel(t *testing.T) {
	s, path := openStore(t)
	s.lockTimeout = 10 * time.Second
	if err := os.WriteFile(path+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ctxBG)
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := s.Add(ctx, Input{Title: "x"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("cancel took %v", time.Since(start))
	}
}

func TestReleaseErrorJoined(t *testing.T) {
	errRelease := errors.New("release failed")
	for _, tc := range []struct {
		name  string
		opErr bool
	}{{"op succeeds", false}, {"op fails", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openStore(t)
			s.env.remove = func(name string) error {
				if strings.HasSuffix(name, ".lock") {
					return errRelease
				}
				return os.Remove(name)
			}
			if tc.opErr {
				s.env.createTemp = failingCreateTemp
			}
			_, err := s.Add(ctxBG, Input{Title: "x"})
			if !errors.Is(err, errRelease) {
				t.Fatalf("err = %v, want release error", err)
			}
			if tc.opErr && !errors.Is(err, errBoom) {
				t.Fatalf("err = %v, want operation error too", err)
			}
		})
	}
}
