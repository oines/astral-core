package astral

import (
	"context"
	"fmt"
	"time"
)

func (s *Session) appendLocked(ctx context.Context, drafts ...Entry) ([]Entry, error) {
	if len(drafts) == 0 {
		return nil, fmtInvalid("empty append")
	}
	entries, err := copyJSON(drafts)
	if err != nil {
		return nil, fmtInvalid("entry JSON: %v", err)
	}
	head := headOf(s.rollout.Entries)
	for i := range entries {
		e := &entries[i]
		if e.ID == "" {
			e.ID = newID()
		}
		if e.Time.IsZero() {
			e.Time = time.Now().UTC()
		}
		if e.IsHistory() {
			e.ParentID = head
			head = e.ID
		} else {
			e.ParentID = ""
		}
		if e.Kind == EntryHeadSelected {
			head = e.Head.EntryID
		}
	}
	next := Rollout{Version: s.rollout.Version + 1, Entries: append(append([]Entry(nil), s.rollout.Entries...), entries...)}
	if err = ValidateRollout(next); err != nil {
		return nil, err
	}
	err = s.agent.config.Store.Append(ctx, s.id, s.rollout.Version, clone(entries))
	if err != nil {
		// The remote store may have committed before its acknowledgement was lost.
		// Observe the exact IDs before deciding whether dependent effects are safe.
		check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		observed, readErr := s.agent.config.Store.Read(check, s.id)
		cancel()
		if readErr != nil {
			return nil, err
		}
		if validation := ValidateRollout(observed); validation != nil {
			return nil, validation
		}
		seen := map[string]Entry{}
		for _, e := range observed.Entries {
			seen[e.ID] = e
		}
		matched := 0
		for _, e := range entries {
			if v, ok := seen[e.ID]; ok {
				if !sameJSON(v, e) {
					return nil, fmt.Errorf("%w: entry ID changed contents", ErrCorrupt)
				}
				matched++
			}
		}
		if matched == 0 {
			return nil, err
		}
		if matched != len(entries) {
			return nil, fmt.Errorf("%w: partially committed batch", ErrCorrupt)
		}
		if observed.Version != next.Version || !sameJSON(observed.Entries, next.Entries) {
			return nil, fmt.Errorf("%w: another owner changed this session", ErrConflict)
		}
	}
	s.rollout = next
	close(s.changed)
	s.changed = make(chan struct{})
	for _, e := range entries {
		s.publishLocked(Event{Type: EventCommitted, EntryID: e.ID, RunID: e.RunID, TurnID: e.TurnID})
	}
	return clone(entries), nil
}

func (s *Session) append(ctx context.Context, entries ...Entry) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(ctx, entries...)
}

// Reload is an explicit recovery step after a Store conflict. The host must
// re-establish exclusive ownership first; this method never steals a lease.
func (s *Session) Reload(ctx context.Context) error {
	if err := s.agent.checkOpen(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.busyLocked() {
		return ErrBusy
	}
	r, err := s.agent.config.Store.Read(ctx, s.id)
	if err != nil {
		return err
	}
	if err = ValidateRollout(r); err != nil {
		return err
	}
	if len(r.Entries) == 0 || r.Entries[0].Created.SessionID != s.id {
		return ErrCorrupt
	}
	s.rollout = clone(r)
	env, _ := effectiveEnvironment(s.pathLocked())
	s.needsEnvironment = env != nil
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}
