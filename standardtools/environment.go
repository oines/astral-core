package standardtools

import (
	"context"
	"encoding/json"
	"fmt"

	astral "github.com/oines/astral-core"
)

type EnvironmentSelection struct {
	Runtime   string `json:"runtime,omitempty"`
	Computer  string `json:"computer,omitempty"`
	Workspace string `json:"workspace,omitempty"`
}

// SwitchEnvironment is separate from the five standard tools. The host resolves
// and authorizes the requested selection and supplies its full current snapshot.
func SwitchEnvironment(resolve func(context.Context, astral.Invocation, EnvironmentSelection) (astral.EnvironmentSnapshot, error)) astral.Tool {
	return astral.ToolFunc{Spec: astral.ToolDefinition{Name: "switch_environment", Version: "1", Sequential: true, Description: "Switch or refresh the execution environment. The host returns a complete environment snapshot.", Parameters: json.RawMessage(`{"type":"object","properties":{"runtime":{"type":"string"},"computer":{"type":"string"},"workspace":{"type":"string"}},"additionalProperties":false}`)}, Run: func(ctx context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolResult, error) {
		if resolve == nil {
			return astral.ToolResult{}, fmt.Errorf("environment resolver is required")
		}
		var selection EnvironmentSelection
		if err := json.Unmarshal(inv.Call.Arguments, &selection); err != nil {
			return astral.ToolResult{}, err
		}
		snapshot, err := resolve(ctx, inv, selection)
		if err != nil {
			return astral.ToolResult{}, err
		}
		return astral.ToolResult{Content: astral.Text("Environment updated. The following environment context describes the selected target."), Environment: &snapshot}, nil
	}}
}
