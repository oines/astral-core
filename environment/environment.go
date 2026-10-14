// Package environment supplies optional typed target snapshots over opaque Core bindings.
package environment

import (
	"context"
	"encoding/json"
)

// Snapshot describes what the model was told, not a filesystem or
// process snapshot. Runtime/workspace creation and discovery belong to the host.
type Snapshot struct {
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

type Request struct {
	SessionID string
	Previous  *Snapshot
}

type Provider func(context.Context, Request) (Snapshot, error)

type Binding struct {
	SnapshotID  string `json:"snapshot_id,omitempty"`
	RuntimeID   string `json:"runtime_id,omitempty"`
	ComputerID  string `json:"computer_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
}

// Context is the optional environment payload carried in Invocation.Binding.
type Context struct {
	Binding  Binding   `json:"binding"`
	Snapshot *Snapshot `json:"snapshot,omitempty"`
}

// Encode creates an isolated, protocol-neutral binding or tool context update.
func Encode(binding Binding, snapshot *Snapshot) (json.RawMessage, error) {
	return json.Marshal(Context{Binding: binding, Snapshot: snapshot})
}

// Decode rejects malformed bindings before a backend can choose a target.
func Decode(raw json.RawMessage) (Context, error) {
	var c Context
	if len(raw) == 0 {
		return c, nil
	}
	err := json.Unmarshal(raw, &c)
	return c, err
}
