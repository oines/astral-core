package session

import (
	"context"
	"fmt"

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
		snapshot := e.Environment
		if e.Kind == EntryToolResult || e.Kind == EntryToolResolution {
			snapshot, _ = resultEnvironment(e)
		}
		if snapshot != nil {
			return snapshot, targetenv.Binding{SnapshotID: e.ID, RuntimeID: snapshot.RuntimeID, ComputerID: snapshot.ComputerID, WorkspaceID: snapshot.WorkspaceID}
		}
	}
	return nil, targetenv.Binding{}
}

// A result's environment effect belongs to that inclusive history boundary,
// even before the following explicit environment node in the atomic batch.
func resultEnvironment(e Entry) (*targetenv.Snapshot, error) {
	if e.Tool == nil || e.Tool.Result == nil || len(e.Tool.Result.Context) == 0 {
		return nil, nil
	}
	payload, err := targetenv.Decode(e.Tool.Result.Context)
	if err != nil || payload.Snapshot == nil {
		return nil, fmt.Errorf("%w: tool result environment snapshot", ErrCorrupt)
	}
	return payload.Snapshot, nil
}
