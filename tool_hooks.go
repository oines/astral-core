package astral

import (
	"context"
	"encoding/json"
	"fmt"
)

type AfterToolCall func(context.Context, Invocation, ToolResult) (ToolResultPatch, error)

type ToolDecision struct {
	Block     bool   `json:"block,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Terminate bool   `json:"terminate,omitempty"`
}

// ToolResultPatch changes presentation or the terminate hint. Nil fields keep
// their original values; a non-nil false pointer explicitly clears a boolean.
// Execution outcome cannot be rewritten by this hook.
type ToolResultPatch struct {
	Content   []Content
	Details   json.RawMessage
	IsError   *bool
	Code      *string
	Artifact  *string
	Terminate *bool
}

func beforeTool(ctx context.Context, hook BeforeToolCall, inv ToolContext) (decision ToolDecision, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("before-tool hook panicked: %v", p)
		}
	}()
	return hook(ctx, inv)
}

func afterTool(ctx context.Context, hook AfterToolCall, inv Invocation, result ToolResult) (changed ToolResultPatch, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("after-tool hook panicked: %v", p)
		}
	}()
	return hook(ctx, clone(inv), clone(result))
}

func applyToolPatch(result ToolResult, patch ToolResultPatch) ToolResult {
	if patch.Content != nil {
		result.Content = patch.Content
	}
	if patch.Details != nil {
		result.Details = patch.Details
	}
	if patch.IsError != nil {
		result.IsError = *patch.IsError
	}
	if patch.Code != nil {
		result.Code = *patch.Code
	}
	if patch.Artifact != nil {
		result.Artifact = *patch.Artifact
	}
	if patch.Terminate != nil {
		result.Terminate = *patch.Terminate
	}
	return result
}
