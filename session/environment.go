package session

import (
	"context"

	protocol "github.com/oines/astral-core"
	targetenv "github.com/oines/astral-core/environment"
)

func (s *Session) RefreshEnvironment(ctx context.Context, snapshot targetenv.Snapshot) error {
	if err := s.agent.checkOpen(); err != nil {
		return err
	}
	owned, err := copyJSON(snapshot)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return protocol.ErrClosed
	}
	if s.busyLocked() {
		return protocol.ErrBusy
	}
	if _, err = s.appendLocked(ctx, Entry{Kind: EntryEnvironment, Environment: &owned}); err != nil {
		return err
	}
	s.needsEnvironment = false
	return nil
}

func effectiveEnvironment(path []Entry) (*targetenv.Snapshot, targetenv.Binding) {
	for i := len(path) - 1; i >= 0; i-- {
		e := path[i]
		if e.Kind == EntryEnvironment {
			return e.Environment, targetenv.Binding{SnapshotID: e.ID, RuntimeID: e.Environment.RuntimeID, ComputerID: e.Environment.ComputerID, WorkspaceID: e.Environment.WorkspaceID}
		}
	}
	return nil, targetenv.Binding{}
}
