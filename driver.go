package astral

import (
	"context"
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
	Control         *RunEnd       `json:"control,omitempty"`
	NeedsResolution bool          `json:"needs_resolution,omitempty"`
}

func (s CallState) Complete() bool { return s.Result != nil || s.Control != nil }

// ExecutionState is only the current execution boundary, not a history tree.
type ExecutionState struct {
	ID               string
	Configuration    Configuration
	BatchTools       []ToolDefinition
	Last             *Frame
	Calls            map[string]CallState
	LastRunStatus    string
	LastRunEndReason string
	Turn             *TurnState
	// HasQueuedInputs counts driving inputs only; passive context cannot wake a run.
	HasQueuedInputs bool
	// QueueOnly lets queued input lead after cancellation before model generation.
	QueueOnly bool
}

type TransitionKind string

const (
	TransitionRunStart       TransitionKind = "run_start"
	TransitionMessage        TransitionKind = "message"
	TransitionPrepared       TransitionKind = "prepared"
	TransitionAuthorized     TransitionKind = "authorized"
	TransitionStarted        TransitionKind = "started"
	TransitionCompleted      TransitionKind = "completed"
	TransitionResult         TransitionKind = "result"
	TransitionToolControl    TransitionKind = "tool_control"
	TransitionToolUnresolved TransitionKind = "tool_unresolved"
	TransitionSelectModel    TransitionKind = "select_model"
	TransitionTurnStart      TransitionKind = "turn_start"
	TransitionTurnDecision   TransitionKind = "turn_decision"
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
	Control           *RunEnd
	NeedsResolution   bool
	ModelID           string
	Usage             Usage
	Contexts          []Message
	SyntheticResults  []ToolCompletion
	Turn              *TurnState
}

// ToolCompletion records a synthetic, unexecuted result in the same commit as
// the assistant that introduced its call (for example a length-truncated reply).
type ToolCompletion struct {
	ID         string
	Invocation Invocation
	Result     ToolResult
}

// Continuable uses execution facts, never the projected role of passive context.
func (s ExecutionState) Continuable(queued bool) bool {
	if s.Last == nil {
		return false
	}
	if s.Last.Message.Role == RoleUser {
		return queued || s.LastRunStatus != "cancelled"
	}
	if s.Last.Message.Role != RoleAssistant {
		return false
	}
	if s.controlEnd() != nil {
		return queued
	}
	if s.Turn != nil && s.Turn.Decided && s.Turn.Decision == TurnEnd {
		if s.LastRunStatus == "completed" {
			return queued
		}
		if !s.Turn.Advanced {
			return true
		}
	}
	if !s.Last.Message.Failed() && s.Turn != nil && s.Turn.ID != "" && !s.Turn.Decided && s.LastRunStatus != "cancelled" {
		return true
	}
	if !s.Last.Message.Failed() && len(s.Last.Message.ToolCalls()) > 0 {
		for _, c := range s.Last.Message.ToolCalls() {
			if r := s.Calls[c.ID]; r.Result == nil || !r.Result.Terminate {
				return true
			}
		}
	}
	if s.Turn != nil && s.Turn.AssistantID == s.Last.ID && s.Turn.Decided && s.Turn.Decision == TurnContinue && (!s.Turn.Advanced || s.LastRunStatus == "started") {
		return true
	}
	return queued
}

func (s ExecutionState) controlEnd() *RunEnd {
	for _, call := range s.Calls {
		if call.Control != nil {
			return call.Control
		}
	}
	return nil
}

type Configuration struct {
	SystemPrompt string           `json:"system_prompt"`
	ModelID      string           `json:"model_id"`
	Tools        []ToolDefinition `json:"tools,omitempty"`
}
