package astral

import (
	"context"
	"encoding/json"
	"fmt"
)

type Tool interface {
	Definition() ToolDefinition
	Execute(context.Context, Invocation, func(ToolProgress)) (ToolExecution, error)
}

// TargetedTool optionally resolves a resource's original execution target before
// the invocation is persisted or approved. This is useful for process handles
// whose runtime is selected and managed by the host.
// The host must validate resource ownership; a Binding is not an authorization.
type TargetedTool interface {
	Bind(context.Context, Invocation) (json.RawMessage, error)
}

// ToolFunc is a small adapter for host functions.
type ToolFunc struct {
	Spec ToolDefinition
	Run  func(context.Context, Invocation, func(ToolProgress)) (ToolExecution, error)
}

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	// Version identifies the host implementation expected by persisted sessions.
	Version string `json:"version,omitempty"`
	// A sequential tool makes its entire model-generated batch run in order.
	Sequential bool `json:"sequential,omitempty"`
	// Exclusive requires this to be the only tool call in a model response.
	Exclusive bool `json:"exclusive,omitempty"`
	// MayEndRun permits an EndRun execution. Such a response must contain only
	// this call and optional reasoning blocks, even when it returns a Result.
	MayEndRun bool `json:"may_end_run,omitempty"`
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

type Invocation struct {
	SessionID   string          `json:"session_id"`
	RunID       string          `json:"run_id"`
	TurnID      string          `json:"turn_id"`
	AssistantID string          `json:"assistant_id"`
	ExecutionID string          `json:"execution_id"`
	Call        ToolCall        `json:"call"`
	Binding     json.RawMessage `json:"binding,omitempty"`
}

type ToolResult struct {
	Content   []Content       `json:"content,omitempty"`
	Details   json.RawMessage `json:"details,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Code      string          `json:"code,omitempty"`
	Outcome   Outcome         `json:"outcome"`
	Artifact  string          `json:"artifact,omitempty"`
	Terminate bool            `json:"terminate,omitempty"`
}

// ToolExecution distinguishes model-facing output from a harness control.
// A successful execution must set exactly one of Result and EndRun.
type ToolExecution struct {
	Result *ToolResult `json:"result,omitempty"`
	EndRun *RunEnd     `json:"end_run,omitempty"`
}

// RunEnd completes the current Run without producing a tool result. Reason is
// host-defined diagnostic data, not a message sent to the model.
type RunEnd struct {
	Reason string `json:"reason,omitempty"`
}

func (e ToolExecution) Validate() error {
	if (e.Result == nil) == (e.EndRun == nil) {
		return fmtInvalid("tool execution requires exactly one of Result and EndRun")
	}
	_, err := copyJSON(e)
	return err
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

func (t ToolFunc) Execute(ctx context.Context, call Invocation, emit func(ToolProgress)) (ToolExecution, error) {
	return t.Run(ctx, call, emit)
}

type argumentValidator interface{ Validate(any) error }

type registeredTool struct {
	tool       Tool
	definition ToolDefinition
	schema     argumentValidator
}

func (a *Session) toolFor(def ToolDefinition) (registeredTool, error) {
	t, ok := a.tools[def.Name]
	if !ok || !sameJSON(t.definition, def) {
		return registeredTool{}, fmt.Errorf("tool %q is missing or changed; explicitly reconfigure the session", def.Name)
	}
	return t, nil
}

func bindTool(ctx context.Context, tool TargetedTool, inv Invocation) (binding json.RawMessage, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("tool binding panicked: %v", p)
		}
	}()
	binding, err = tool.Bind(ctx, clone(inv))
	if err == nil {
		binding, err = copyJSON(binding)
	}
	return
}

func invokeTool(ctx context.Context, tool Tool, inv Invocation, emit func(ToolProgress)) (execution ToolExecution, err error) {
	defer func() {
		if p := recover(); p != nil {
			execution = ToolExecution{Result: &ToolResult{Outcome: OutcomeUnknown}}
			err = fmt.Errorf("tool panicked: %v", p)
		}
	}()
	execution, err = tool.Execute(ctx, clone(inv), func(progress ToolProgress) {
		owned, err := copyJSON(progress)
		if err == nil {
			emit(owned)
		}
	})
	if err == nil {
		var owned ToolExecution
		owned, err = copyJSON(execution)
		if err != nil {
			return ToolExecution{}, &ToolError{Outcome: OutcomeUnknown, Code: "invalid_tool_execution", Cause: err}
		}
		execution = owned
	}
	return execution, err
}
