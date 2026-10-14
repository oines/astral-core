// Package environment carries host-supplied context and opaque execution bindings.
package environment

import (
	"context"
	"encoding/json"
)

// Snapshot separates model-visible context from internal execution metadata.
// Discovery, authorization and interpretation of Binding belong to the host.
type Snapshot struct {
	Binding json.RawMessage `json:"binding,omitempty"`
	Context string          `json:"context,omitempty"`
}

type Request struct {
	SessionID string
	Previous  *Snapshot
}

type Provider func(context.Context, Request) (Snapshot, error)
