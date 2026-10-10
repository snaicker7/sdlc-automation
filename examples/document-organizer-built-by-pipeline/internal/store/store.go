package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"sync"
	"time"
)

var (
	// ErrNotFound is returned for an unknown document ID.
	ErrNotFound = errors.New("document not found")
	// ErrCorrupt is returned when the data file is not valid.
	ErrCorrupt = errors.New("data file is corrupt")
)

// readerYield bounds how long a reader waits for a writer's lock before reading anyway.
const readerYield = 250 * time.Millisecond

// Options configures Open.
type Options struct {
	Path        string
	LockTimeout time.Duration
}

// fileData is the on-disk format.
type fileData struct {
	NextID    int64      `json:"next_id"`
	Documents []Document `json:"documents"`
}

// Store is the only code that touches the data file. It is safe for concurrent use.
type Store struct {
	path        string
	lockTimeout time.Duration
	env         env

	mu      sync.Mutex // guards the cache below and serialises operations on this Store
	data    fileData
	cached  bool
	modTime time.Time
	size    int64
}

// Open returns a Store for opts.Path. A missing file is an empty store.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	if opts.Path == "" {
		return nil, invalid("data", "path must not be empty")
	}
	if opts.LockTimeout <= 0 {
		return nil, invalid("lock-timeout", "must be positive")
	}
	return &Store{path: opts.Path, lockTimeout: opts.LockTimeout, env: defaultEnv(), data: emptyData()}, nil
}

func emptyData() fileData { return fileData{NextID: 1, Documents: []Document{}} }

// Add validates in, then stores it as a new document.
func (s *Store) Add(ctx context.Context, in Input) (doc Document, err error) {
	norm, err := Normalize(in)
	if err != nil {
		return Document{}, err
	}
	err = s.mutate(ctx, func(fd *fileData) error {
		doc = Document{ID: fd.NextID, Title: norm.Title, Content: norm.Content, Tags: norm.Tags, CreatedAt: s.env.now().UTC()}
		fd.NextID++
		fd.Documents = append(fd.Documents, doc)
		return nil
	})
	return doc, err
}

// Delete removes the document with the given ID; next_id is never decremented.
func (s *Store) Delete(ctx context.Context, id int64) error {
	return s.mutate(ctx, func(fd *fileData) error {
		for i, d := range fd.Documents {
			if d.ID == id {
				fd.Documents = append(fd.Documents[:i], fd.Documents[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("document %d: %w", id, ErrNotFound)
	})
}

// Get returns the document with the given ID.
func (s *Store) Get(ctx context.Context, id int64) (Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(ctx); err != nil {
		return Document{}, err
	}
	for _, d := range s.data.Documents {
		if d.ID == id {
			return d, nil
		}
	}
	return Document{}, fmt.Errorf("document %d: %w", id, ErrNotFound)
}

// List returns the documents matching f, newest first. The result is never nil.
func (s *Store) List(ctx context.Context, f Filter) ([]Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(ctx); err != nil {
		return nil, err
	}
	out := []Document{}
	for _, d := range s.data.Documents {
		if Match(d, f) {
			out = append(out, d)
		}
	}
	sortDocs(out)
	return out, nil
}

// mutate runs reload-modify-write under the lock. A release error is joined with the operation's.
func (s *Store) mutate(ctx context.Context, apply func(*fileData) error) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	release, err := s.acquireLock(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if rerr := release(); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()
	fd, err := s.loadFile(ctx)
	if err != nil {
		return err
	}
	if err := apply(&fd); err != nil {
		return err
	}
	b, err := json.Marshal(fd)
	if err != nil {
		return fmt.Errorf("encode %s: %w", s.path, err)
	}
	if err := s.writeAtomic(ctx, b); err != nil {
		return err
	}
	s.data = fd
	if fi, serr := s.env.stat(s.path); serr == nil {
		s.cached, s.modTime, s.size = true, fi.ModTime(), fi.Size()
	} else {
		s.cached = false // the write succeeded; just force a reload on the next read
	}
	return nil
}

// refresh reloads the cache if the file's mtime or size changed. Callers hold s.mu.
func (s *Store) refresh(ctx context.Context) error {
	fi, err := s.statFile(ctx)
	if errors.Is(err, fs.ErrNotExist) {
		s.data, s.cached = emptyData(), false
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", s.path, err)
	}
	if s.cached && fi.ModTime().Equal(s.modTime) && fi.Size() == s.size {
		return nil
	}
	if err := s.yieldToWriter(ctx); err != nil {
		return err
	}
	fd, err := s.loadFile(ctx)
	if err != nil {
		return err
	}
	s.data, s.cached, s.modTime, s.size = fd, true, fi.ModTime(), fi.Size()
	return nil
}

// yieldToWriter lets a writer holding the lock finish before a reader opens the file (without
// taking the lock itself). Windows cannot rename over a file that readers keep open, so
// back-to-back readers would otherwise starve writers. The wait is bounded in case the lock is stale.
func (s *Store) yieldToWriter(ctx context.Context) error {
	e := &s.env
	start := e.now()
	for e.now().Sub(start) < readerYield {
		if _, err := e.stat(s.path + ".lock"); err != nil {
			return nil // no writer active (or lock state unknowable): just read
		}
		wait := 10*time.Millisecond + time.Duration(rand.Int63n(int64(40*time.Millisecond)+1))
		if err := e.sleep(ctx, wait); err != nil {
			return fmt.Errorf("waiting for writer: %w", err)
		}
	}
	return nil
}

// loadFile reads and validates the data file; a missing file is an empty store.
func (s *Store) loadFile(ctx context.Context) (fileData, error) {
	raw, err := s.readFile(ctx)
	if errors.Is(err, fs.ErrNotExist) {
		return emptyData(), nil
	}
	if err != nil {
		return fileData{}, fmt.Errorf("read %s: %w", s.path, err)
	}
	var fd fileData
	if err := json.Unmarshal(raw, &fd); err != nil {
		return fileData{}, fmt.Errorf("%w: %s: %w", ErrCorrupt, s.path, err)
	}
	var maxID int64
	for i := range fd.Documents {
		maxID = max(maxID, fd.Documents[i].ID)
		if fd.Documents[i].Tags == nil {
			fd.Documents[i].Tags = []string{}
		}
	}
	if fd.NextID < maxID+1 || fd.NextID < 1 {
		return fileData{}, fmt.Errorf("%w: %s: next_id %d is lower than max id %d + 1", ErrCorrupt, s.path, fd.NextID, maxID)
	}
	if fd.Documents == nil {
		fd.Documents = []Document{}
	}
	return fd, nil
}
