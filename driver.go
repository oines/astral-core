package astral

import (
	"context"
	"encoding/json"
)

// Driver connects execution to context and state policies. It is called by the
// single executor; it must never execute models or tools. Commit is an awaited
// acknowledgement: an error prevents dependent external effects.
// Implementations serialize Commit and Boundary with input enqueueing.
type Driver interface {
	Snapshot() ExecutionState
	Prepare(context.Context, string) (Model, Request, error)
	Commit(context.Context, Transition) error
	Boundary(context.Context, string, bool) (bool, RunResult, error)
	Finish(context.Context, string, string, error) (RunResult, error)
	AcceptResult(context.Context, Invocation, ToolResult) (ToolResult, error)
}

// Frame identifies one message independently of any storage representation.
type Frame struct {
	ID      string  `json:"id"`
	TurnID  string  `json:"turn_id,omitempty"`
	Message Message `json:"message"`
}

type CallState struct {
	Invocation      Invocation    `json:"invocation"`
	Prepared        bool          `json:"prepared,omitempty"`
	Decision        *ToolDecision `json:"decision,omitempty"`
	Started         bool          `json:"started,omitempty"`
	Completed       *ToolResult   `json:"completed,omitempty"`
	Result          *ToolResult   `json:"result,omitempty"`
	NeedsResolution bool          `json:"needs_resolution,omitempty"`
}

// ExecutionState is only the current execution boundary, not a history tree.
type ExecutionState struct {
	ID            string
	Configuration Configuration
	BatchTools    []ToolDefinition
	Last          *Frame
	Calls         map[string]CallState
	// Preserve an absent binding through JSON copies; an explicit JSON null is
	// a nonempty host binding and must remain distinct.
	Binding json.RawMessage `json:",omitempty"`
}

type TransitionKind string

const (
	TransitionRunStart    TransitionKind = "run_start"
	TransitionMessage     TransitionKind = "message"
	TransitionPrepared    TransitionKind = "prepared"
	TransitionAuthorized  TransitionKind = "authorized"
	TransitionStarted     TransitionKind = "started"
	TransitionCompleted   TransitionKind = "completed"
	TransitionResult      TransitionKind = "result"
	TransitionSelectModel TransitionKind = "select_model"
)

// Transition describes an execution fact. IDs are assigned before Commit and
// remain stable while a persistent driver reconciles an acknowledgement.
type Transition struct {
	Kind              TransitionKind
	ID, RunID, TurnID string
	Message           *Message
	Invocation        Invocation
	Decision          *ToolDecision
	Result            *ToolResult
	NeedsResolution   bool
	ModelID           string
	Usage             Usage
}

type Configuration struct {
	SystemPrompt string           `json:"system_prompt"`
	ModelID      string           `json:"model_id"`
	Tools        []ToolDefinition `json:"tools,omitempty"`
}
