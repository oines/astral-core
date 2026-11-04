package provider_test

import (
	"encoding/json"
	"strings"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models/provider"
	"github.com/oines/astral-core/models/provider/openai/completions"
)

func TestCompletionToolTextKeepsExactImageHintsAndPartOrder(t *testing.T) {
	const hint = "\n[Image returned by this tool; attached after the tool-result batch.]"
	image := core.Content{Type: "image", MediaType: "image/png", Data: json.RawMessage(`"eA=="`)}
	longText := strings.Repeat("x", 64<<10)
	for _, test := range []struct {
		name  string
		parts []core.Content
		want  string
	}{
		{"empty text", core.Text(""), ""},
		{"text", core.Text(longText), longText},
		{"image suffix", []core.Content{{Type: "text", Text: longText}, image}, longText + hint},
		{"multiple images", []core.Content{image, {Type: "text", Text: longText}, image, {Type: "text", Text: "tail <>&"}}, hint + longText + hint + "tail <>&"},
	} {
		t.Run(test.name, func(t *testing.T) {
			model, err := provider.New(completions.Protocol, config("target", "scope"))
			if err != nil {
				t.Fatal(err)
			}
			request := requestFor(model, []core.Message{
				{Role: core.RoleAssistant, Content: []core.Content{{Type: "tool_call", ToolCall: &core.ToolCall{ID: "call", Name: "lookup", Arguments: json.RawMessage(`{}`)}}}, FinishReason: core.FinishToolCalls},
				{Role: core.RoleTool, ToolCallID: "call", Content: test.parts},
			})
			request.Tools = []core.ToolDefinition{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}
			body, err := model.RequestBody(request)
			if err != nil {
				t.Fatal(err)
			}
			tool := body["messages"].([]any)[1].(map[string]any)
			if tool["role"] != "tool" || tool["content"] != test.want {
				t.Fatal("tool text changed order, empty-string shape or image hints")
			}
		})
	}
}
