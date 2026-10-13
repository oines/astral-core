package astral

import (
	"context"
	"fmt"
)

type TreeNode struct {
	Entry    Entry
	Children []string
}

type ForkPosition string

const (
	ForkAt     ForkPosition = "at"
	ForkBefore ForkPosition = "before"
)

func (s *Session) Rollout() Rollout {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.rollout)
}

func (s *Session) Entries() []Entry { return s.Rollout().Entries }

func (s *Session) Head() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return headOf(s.rollout.Entries)
}

func (s *Session) Entry(id string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.rollout.Entries {
		if e.ID == id {
			return clone(e), true
		}
	}
	return Entry{}, false
}

func (s *Session) Children(id string) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Entry{}
	for _, e := range s.rollout.Entries {
		if e.IsHistory() && e.ParentID == id {
			out = append(out, e)
		}
	}
	return clone(out)
}

func (s *Session) BranchPath(id string) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := pathOf(s.rollout.Entries, id)
	return clone(p), err
}

func (s *Session) Tree() []TreeNode {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []TreeNode{}
	index := map[string]int{}
	for _, e := range s.rollout.Entries {
		if e.IsHistory() {
			index[e.ID] = len(out)
			out = append(out, TreeNode{Entry: e})
		}
	}
	for _, n := range out {
		if i, ok := index[n.Entry.ParentID]; ok {
			out[i].Children = append(out[i].Children, n.Entry.ID)
		}
	}
	return clone(out)
}

func (s *Session) SelectHead(ctx context.Context, id string) error {
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
	path, err := pathOf(s.rollout.Entries, id)
	if err != nil {
		return err
	}
	if len(path) == 0 {
		return ErrNotFound
	}
	if _, err = s.appendLocked(ctx, Entry{Kind: EntryHeadSelected, Head: &HeadRecord{EntryID: id}}); err != nil {
		return err
	}
	env, _ := effectiveEnvironment(path)
	s.needsEnvironment = env != nil
	return nil
}

// Branch selects an existing history entry; subsequent writes form a new child.
func (s *Session) Branch(ctx context.Context, id string) error { return s.SelectHead(ctx, id) }

func (s *Session) Fork(ctx context.Context, id string, position ForkPosition, options SessionOptions) (*Session, error) {
	if err := s.agent.checkOpen(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.busyLocked() {
		s.mu.Unlock()
		return nil, ErrBusy
	}
	path, err := pathOf(s.rollout.Entries, id)
	path = clone(path)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if len(path) == 0 {
		return nil, ErrNotFound
	}
	switch position {
	case ForkAt:
	case ForkBefore:
		if path[len(path)-1].Kind != EntryMessage || path[len(path)-1].Message.Role != RoleUser {
			return nil, fmtInvalid("ForkBefore requires a user entry")
		}
		path = path[:len(path)-1]
	default:
		return nil, fmtInvalid("unknown fork position")
	}
	if options.ID == "" {
		options.ID = newID()
	}
	entries := []Entry{{Kind: EntryCreated, Created: &CreationRecord{FormatVersion: 1, SessionID: options.ID, ForkSessionID: s.id, ForkEntryID: id}}}
	entries = append(entries, path...)
	env, _ := effectiveEnvironment(path)
	needsRefresh := env != nil
	if options.Environment != nil {
		entries = append(entries, Entry{Kind: EntryEnvironment, Environment: options.Environment})
		needsRefresh = false
	}
	return s.agent.create(ctx, options.ID, entries, needsRefresh)
}

func headOf(entries []Entry) string {
	head := ""
	for _, e := range entries {
		if e.IsHistory() {
			head = e.ID
		}
		if e.Kind == EntryHeadSelected {
			head = e.Head.EntryID
		}
	}
	return head
}

func pathOf(entries []Entry, head string) ([]Entry, error) {
	index := make(map[string]Entry, len(entries))
	for _, e := range entries {
		if e.IsHistory() {
			index[e.ID] = e
		}
	}
	var reversed []Entry
	for id := head; id != ""; {
		e, ok := index[id]
		if !ok {
			return nil, ErrNotFound
		}
		reversed = append(reversed, e)
		id = e.ParentID
		if len(reversed) > len(entries) {
			return nil, fmt.Errorf("%w: history cycle", ErrCorrupt)
		}
	}
	path := make([]Entry, len(reversed))
	for i := range reversed {
		path[len(reversed)-i-1] = reversed[i]
	}
	return path, nil
}

func (s *Session) pathLocked() []Entry {
	p, err := pathOf(s.rollout.Entries, headOf(s.rollout.Entries))
	if err != nil {
		panic(err)
	}
	return p
}

func (s *Session) configurationLocked() Configuration {
	c, _, err := effectiveConfiguration(s.pathLocked())
	if err != nil {
		panic(err)
	}
	return c
}

func (s *Session) boundaryRequiredLocked() bool {
	required := s.rollout.Entries[0].Created.ForkSessionID != ""
	for _, e := range s.rollout.Entries {
		if e.Kind == EntryHeadSelected {
			required = true
		}
		if e.Kind == EntryRun && (e.Run.Status == "started" || e.Run.Status == "resumed") {
			required = false
		}
	}
	return required
}
