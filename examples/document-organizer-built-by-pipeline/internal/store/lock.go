package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"time"
)

// ErrLockTimeout is returned when the lock could not be acquired in time.
var ErrLockTimeout = errors.New("timed out waiting for lock")

const staleAfter = 10 * time.Second

// acquireLock takes <data>.lock, recovering locks older than staleAfter, and returns its release func.
func (s *Store) acquireLock(ctx context.Context) (func() error, error) {
	e := &s.env
	lock := s.path + ".lock"
	deadline := e.now().Add(s.lockTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("acquire lock %s: %w", lock, err)
		}
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			if err := f.Close(); err != nil {
				return nil, errors.Join(fmt.Errorf("close lock %s: %w", lock, err), e.remove(lock))
			}
			return func() error {
				if err := e.remove(lock); err != nil {
					return fmt.Errorf("release lock %s: %w", lock, err)
				}
				return nil
			}, nil
		}
		// Windows reports a lock that is being deleted as access denied.
		if !errors.Is(err, fs.ErrExist) && !isSharingViolation(err) {
			return nil, fmt.Errorf("create lock %s: %w", lock, err)
		}
		recovered, err := s.removeIfStale(lock)
		if err != nil {
			return nil, err
		}
		if recovered {
			continue
		}
		if !e.now().Before(deadline) {
			abs, aerr := filepath.Abs(lock)
			if aerr != nil {
				abs = lock
			}
			return nil, fmt.Errorf("%w after %v: lock file %s may be deleted if no other instance is running",
				ErrLockTimeout, s.lockTimeout, abs)
		}
		wait := 10*time.Millisecond + time.Duration(rand.Int63n(int64(40*time.Millisecond)+1))
		if err := e.sleep(ctx, wait); err != nil {
			return nil, fmt.Errorf("waiting for lock %s: %w", lock, err)
		}
	}
}

// removeIfStale removes lock if it is older than staleAfter and unchanged since it was first seen.
// It reports whether the caller should retry immediately.
func (s *Store) removeIfStale(lock string) (bool, error) {
	e := &s.env
	fi, err := e.stat(lock)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if isSharingViolation(err) { // Windows: the lock is being deleted right now
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat lock %s: %w", lock, err)
	}
	if e.now().Sub(fi.ModTime()) <= staleAfter {
		return false, nil
	}
	if e.afterStaleStat != nil {
		e.afterStaleStat()
	}
	fi2, err := e.stat(lock)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if isSharingViolation(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat lock %s: %w", lock, err)
	}
	if !fi2.ModTime().Equal(fi.ModTime()) {
		return false, nil // refreshed by its owner
	}
	if err := e.remove(lock); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("remove stale lock %s: %w", lock, err)
	}
	return true, nil
}
