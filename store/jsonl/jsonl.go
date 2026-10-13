// Package jsonl provides a local durable Store. One checksummed JSON line is one
// atomic logical commit, including all its entries. An unterminated final line
// is an uncommitted crash tail; Append discards only that tail. Committed lines
// are never rewritten. The host must give one process exclusive directory ownership.
package jsonl

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	astral "github.com/oines/astral-core"
)

type Store struct {
	mu        *sync.Mutex
	directory string
}

// Handles opened through aliases of one directory share compare-and-append
// serialization. This is process-local; it is not an execution lease.
var directoryLocks sync.Map

type commitBody struct {
	SessionID string         `json:"session_id"`
	Version   uint64         `json:"version"`
	Entries   []astral.Entry `json:"entries"`
}
type commit struct {
	Body   json.RawMessage `json:"commit"`
	SHA256 string          `json:"sha256"`
}

func Open(directory string) (*Store, error) {
	abs, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	lock, _ := directoryLocks.LoadOrStore(abs, &sync.Mutex{})
	return &Store{directory: abs, mu: lock.(*sync.Mutex)}, nil
}
func (s *Store) filename(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(s.directory, hex.EncodeToString(sum[:])+".jsonl")
}
func (s *Store) Read(ctx context.Context, id string) (astral.Rollout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, _, err := s.read(ctx, id)
	return r, err
}
func (s *Store) read(ctx context.Context, id string) (astral.Rollout, int64, error) {
	if err := ctx.Err(); err != nil {
		return astral.Rollout{}, 0, err
	}
	f, err := os.Open(s.filename(id))
	if errors.Is(err, os.ErrNotExist) {
		return astral.Rollout{}, 0, astral.ErrNotFound
	}
	if err != nil {
		return astral.Rollout{}, 0, err
	}
	defer f.Close()
	var r astral.Rollout
	var offset int64
	reader := bufio.NewReader(f)
	for {
		if err := ctx.Err(); err != nil {
			return r, offset, err
		}
		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return r, offset, err
		}
		var c commit
		if err = json.Unmarshal(line, &c); err != nil {
			return r, offset, fmt.Errorf("%w: invalid commit at byte %d", astral.ErrCorrupt, offset)
		}
		digest := sha256.Sum256(c.Body)
		if c.SHA256 != hex.EncodeToString(digest[:]) {
			return r, offset, fmt.Errorf("%w: commit checksum at byte %d", astral.ErrCorrupt, offset)
		}
		var b commitBody
		if err = json.Unmarshal(c.Body, &b); err != nil {
			return r, offset, fmt.Errorf("%w: commit body", astral.ErrCorrupt)
		}
		if b.SessionID != id || b.Version != r.Version+1 || len(b.Entries) == 0 {
			return r, offset, fmt.Errorf("%w: commit sequence or identity", astral.ErrCorrupt)
		}
		r.Version = b.Version
		r.Entries = append(r.Entries, b.Entries...)
		offset += int64(len(line))
	}
	if len(r.Entries) == 0 {
		return r, offset, astral.ErrNotFound
	}
	if err = astral.ValidateRollout(r); err != nil {
		return r, offset, err
	}
	if r.Entries[0].Created.SessionID != id {
		return r, offset, astral.ErrCorrupt
	}
	// Core uses Read to reconcile lost Append acknowledgements. Flush visible
	// complete commits before returning them so an earlier sync error cannot
	// make an unacknowledged tool intent appear durably committed.
	if err = f.Sync(); err != nil {
		return r, offset, err
	}
	if err = s.syncDirectory(); err != nil {
		return r, offset, err
	}
	return r, offset, nil
}
func (s *Store) Append(ctx context.Context, id string, version uint64, entries []astral.Entry) error {
	if id == "" || len(entries) == 0 {
		return astral.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, offset, err := s.read(ctx, id)
	if err != nil && !errors.Is(err, astral.ErrNotFound) {
		return err
	}
	if r.Version != version {
		return astral.ErrConflict
	}
	next := astral.Rollout{Version: version + 1, Entries: append(r.Entries, entries...)}
	if err = astral.ValidateRollout(next); err != nil {
		return err
	}
	if next.Entries[0].Created.SessionID != id {
		return astral.ErrCorrupt
	}
	body, err := json.Marshal(commitBody{SessionID: id, Version: version + 1, Entries: entries})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	line, err := json.Marshal(commit{Body: body, SHA256: hex.EncodeToString(digest[:])})
	if err != nil {
		return err
	}
	line = append(line, '\n')
	f, err := os.OpenFile(s.filename(id), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = f.Truncate(offset); err != nil {
		return err
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	n, err := f.Write(line)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	// Sync the directory as well so the first acknowledged commit survives loss
	// of the new directory entry on filesystems requiring a directory fsync.
	return s.syncDirectory()
}

func (s *Store) syncDirectory() error {
	d, err := os.Open(s.directory)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
