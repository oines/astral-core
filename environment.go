package astral

import (
	"context"
	"encoding/json"
)

// EnvironmentSnapshot describes what the model was told, not a filesystem or
// process snapshot. Runtime/workspace creation and discovery belong to the host.
type EnvironmentSnapshot struct {
	RuntimeID    string          `json:"runtime_id,omitempty"`
	ComputerID   string          `json:"computer_id,omitempty"`
	WorkspaceID  string          `json:"workspace_id,omitempty"`
	OS           string          `json:"os,omitempty"`
	Shell        string          `json:"shell,omitempty"`
	CWD          string          `json:"cwd,omitempty"`
	Computers    []Resource      `json:"computers,omitempty"`
	Skills       []Resource      `json:"skills,omitempty"`
	Instructions []Resource      `json:"instructions,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
}

type Resource struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	Path        string `json:"path,omitempty"`
	Description string `json:"description,omitempty"`
}

type EnvironmentRequest struct {
	SessionID string
	Previous  *EnvironmentSnapshot
}

type EnvironmentProvider func(context.Context, EnvironmentRequest) (EnvironmentSnapshot, error)

type Binding struct {
	SnapshotID  string `json:"snapshot_id,omitempty"`
	RuntimeID   string `json:"runtime_id,omitempty"`
	ComputerID  string `json:"computer_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
}

func (s *Session) RefreshEnvironment(ctx context.Context, snapshot EnvironmentSnapshot) error {
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
		return ErrClosed
	}
	if s.busyLocked() {
		return ErrBusy
	}
	if _, err = s.appendLocked(ctx, Entry{Kind: EntryEnvironment, Environment: &owned}); err != nil {
		return err
	}
	s.needsEnvironment = false
	return nil
}

func effectiveEnvironment(path []Entry) (*EnvironmentSnapshot, Binding) {
	for i := len(path) - 1; i >= 0; i-- {
		e := path[i]
		if e.Kind == EntryEnvironment {
			return e.Environment, Binding{SnapshotID: e.ID, RuntimeID: e.Environment.RuntimeID, ComputerID: e.Environment.ComputerID, WorkspaceID: e.Environment.WorkspaceID}
		}
	}
	return nil, Binding{}
}
