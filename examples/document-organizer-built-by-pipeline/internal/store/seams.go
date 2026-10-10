package store

import (
	"context"
	"os"
	"time"
)

// tmpFile is the part of *os.File the atomic writer uses, so tests can inject failures.
type tmpFile interface {
	Write(p []byte) (int, error)
	Sync() error
	Close() error
	Name() string
}

// env holds the OS operations the store uses; tests replace fields after Open.
type env struct {
	createTemp     func(dir, pattern string) (tmpFile, error)
	rename         func(oldpath, newpath string) error
	remove         func(name string) error
	readFile       func(name string) ([]byte, error)
	stat           func(name string) (os.FileInfo, error)
	sleep          func(ctx context.Context, d time.Duration) error
	now            func() time.Time
	afterStaleStat func() // called between the stale Stat and the Remove
}

func defaultEnv() env {
	return env{
		createTemp: func(dir, pattern string) (tmpFile, error) {
			f, err := os.CreateTemp(dir, pattern)
			if err != nil {
				return nil, err
			}
			return f, nil
		},
		rename:   os.Rename,
		remove:   os.Remove,
		readFile: os.ReadFile,
		stat:     os.Stat,
		sleep:    sleepCtx,
		now:      time.Now,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
