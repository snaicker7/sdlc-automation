package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"path/filepath"
	"time"
)

const (
	retryBudget = 5 * time.Second
	retryMin    = 25 * time.Millisecond
	retryJitter = 75 * time.Millisecond
)

// isSharingViolation is a variable so tests can classify their own error values.
var isSharingViolation = platformIsSharingViolation

// retry runs op, retrying with 25-100 ms jitter while it fails with a sharing violation,
// for at most retryBudget.
func (e *env) retry(ctx context.Context, op func() error) error {
	start := e.now()
	for {
		err := op()
		if err == nil || !isSharingViolation(err) {
			return err
		}
		if e.now().Sub(start) >= retryBudget {
			return fmt.Errorf("file still in use after %v: %w", retryBudget, err)
		}
		d := retryMin + time.Duration(rand.Int63n(int64(retryJitter)+1))
		if serr := e.sleep(ctx, d); serr != nil {
			return fmt.Errorf("waiting to retry: %w", serr)
		}
	}
}

// readFile reads the data file, retrying sharing violations.
func (s *Store) readFile(ctx context.Context) ([]byte, error) {
	var raw []byte
	err := s.env.retry(ctx, func() (err error) {
		raw, err = s.env.readFile(s.path)
		return err
	})
	return raw, err
}

// statFile stats the data file, retrying sharing violations.
func (s *Store) statFile(ctx context.Context) (fs.FileInfo, error) {
	var fi fs.FileInfo
	err := s.env.retry(ctx, func() (err error) {
		fi, err = s.env.stat(s.path)
		return err
	})
	return fi, err
}

// writeAtomic replaces the data file with data via temp file, fsync and rename.
// On any failure the temp file is removed and the original is left untouched.
func (s *Store) writeAtomic(ctx context.Context, data []byte) (err error) {
	e := &s.env
	dir := filepath.Dir(s.path)
	f, err := e.createTemp(dir, ".tmp*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	name := f.Name()
	closed := false
	defer func() {
		if err == nil {
			return
		}
		errs := []error{err}
		if !closed {
			if cerr := f.Close(); cerr != nil {
				errs = append(errs, fmt.Errorf("close %s: %w", name, cerr))
			}
		}
		if rerr := e.remove(name); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %s: %w", name, rerr))
		}
		err = errors.Join(errs...)
	}()
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", name, err)
	}
	closed = true
	if err = f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err = e.retry(ctx, func() error { return e.rename(name, s.path) }); err != nil {
		return fmt.Errorf("replace %s: %w", s.path, err)
	}
	return nil
}
