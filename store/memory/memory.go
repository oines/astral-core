// Package memory supplies an isolated, concurrency-safe, non-durable Store.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	astral "github.com/oines/astral-core"
)

type Store struct {
	mu       sync.Mutex
	sessions map[string]astral.Rollout
}

func New() *Store { return &Store{sessions: map[string]astral.Rollout{}} }
func (s *Store) Read(ctx context.Context, id string) (astral.Rollout, error) {
	if err := ctx.Err(); err != nil {
		return astral.Rollout{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.sessions[id]
	if !ok {
		return astral.Rollout{}, astral.ErrNotFound
	}
	return copyValue(r)
}
func (s *Store) Append(ctx context.Context, id string, version uint64, entries []astral.Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "" || len(entries) == 0 {
		return astral.ErrInvalid
	}
	owned, err := copyValue(entries)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]astral.Rollout{}
	}
	r := s.sessions[id]
	if r.Version != version {
		return astral.ErrConflict
	}
	next := astral.Rollout{Version: version + 1, Entries: append(append([]astral.Entry(nil), r.Entries...), owned...)}
	if err := astral.ValidateRollout(next); err != nil {
		return err
	}
	if next.Entries[0].Created.SessionID != id {
		return fmt.Errorf("%w: session identity mismatch", astral.ErrCorrupt)
	}
	s.sessions[id] = next
	return nil
}
func copyValue[T any](v T) (T, error) {
	var out T
	b, err := json.Marshal(v)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(b, &out)
	return out, err
}
