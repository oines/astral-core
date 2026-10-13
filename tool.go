package astral

import (
	"context"
	"encoding/json"
	"fmt"
)

type Tool interface {
	Definition() ToolDefinition
	Execute(context.Context, Invocation, func(ToolProgress)) (ToolResult, error)
}

// TargetedTool optionally resolves a resource's original execution target before
// the invocation is persisted or approved. This is useful for process handles
// whose runtime differs from the session's currently selected environment.
// The host must validate resource ownership; a Binding is not an authorization.
type TargetedTool interface {
	Bind(context.Context, Invocation) (Binding, *EnvironmentSnapshot, error)
}

// ToolFunc is a small adapter for host functions.
type ToolFunc struct {
	Spec ToolDefinition
	Run  func(context.Context, Invocation, func(ToolProgress)) (ToolResult, error)
}

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	// Version identifies the host implementation expected by persisted sessions.
	Version string `json:"version,omitempty"`
	// A sequential tool makes its entire model-generated batch run in order.
	Sequential bool `json:"sequential,omitempty"`
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

type Invocation struct {
	SessionID     string               `json:"session_id"`
	RunID         string               `json:"run_id"`
	TurnID        string               `json:"turn_id"`
	AssistantID   string               `json:"assistant_id"`
	ExecutionID   string               `json:"execution_id"`
	Call          ToolCall             `json:"call"`
	Binding       Binding              `json:"binding"`
	Environment   *EnvironmentSnapshot `json:"environment,omitempty"`
	awaitApproval func(context.Context, ApprovalRequest) (ApprovalDecision, error)
}

type ToolResult struct {
	Content   []Content       `json:"content,omitempty"`
	Details   json.RawMessage `json:"details,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Code      string          `json:"code,omitempty"`
	Outcome   Outcome         `json:"outcome"`
	Artifact  string          `json:"artifact,omitempty"`
	Terminate bool            `json:"terminate,omitempty"`
	// Only a sequential tool may return an environment change. Core commits the
	// result and the new complete snapshot atomically before the next dispatch.
	Environment *EnvironmentSnapshot `json:"environment,omitempty"`
}

type ToolProgress struct {
	Text    string          `json:"text,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

type Outcome string

const (
	OutcomeSucceeded   Outcome = "succeeded"
	OutcomeFailed      Outcome = "failed"
	OutcomeNotExecuted Outcome = "not_executed"
	OutcomeUnknown     Outcome = "unknown"
)

func (t ToolFunc) Definition() ToolDefinition { return t.Spec }

func (t ToolFunc) Execute(ctx context.Context, call Invocation, emit func(ToolProgress)) (ToolResult, error) {
	return t.Run(ctx, call, emit)
}

type argumentValidator interface{ Validate(any) error }

type registeredTool struct {
	tool       Tool
	definition ToolDefinition
	schema     argumentValidator
}

func (a *Agent) toolFor(def ToolDefinition) (registeredTool, error) {
	t, ok := a.tools[def.Name]
	if !ok || !sameJSON(t.definition, def) {
		return registeredTool{}, fmt.Errorf("tool %q is missing or changed; explicitly reconfigure the session", def.Name)
	}
	return t, nil
}

func bindTool(ctx context.Context, tool TargetedTool, inv Invocation) (binding Binding, environment *EnvironmentSnapshot, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("tool binding panicked: %v", p)
		}
	}()
	binding, environment, err = tool.Bind(ctx, clone(inv))
	if err == nil {
		environment, err = copyJSON(environment)
	}
	return
}

func invokeTool(ctx context.Context, tool Tool, inv Invocation, emit func(ToolProgress)) (result ToolResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			result.Outcome = OutcomeUnknown
			err = fmt.Errorf("tool panicked: %v", p)
		}
	}()
	return tool.Execute(ctx, clone(inv), func(progress ToolProgress) {
		owned, err := copyJSON(progress)
		if err == nil {
			emit(owned)
		}
	})
}
