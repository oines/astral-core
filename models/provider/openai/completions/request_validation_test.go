package completions

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models"
)

func validationFixture() (models.Config, core.Request) {
	streaming := false
	config := models.Config{Spec: core.ModelSpec{ID: "target", Revision: "1", ContextWindow: 100000, MaxOutputTokens: 8192}, Model: "remote", ReplayScope: "scope", Streaming: &streaming}
	request := core.Request{ModelID: "target", ModelRevision: "1", MaxOutputTokens: 1000, Tools: []core.ToolDefinition{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}, Messages: []core.Message{
		{Role: core.RoleAssistant, Content: []core.Content{{Type: "text", Text: strings.Repeat("assistant line\n", 4096)}, {Type: "tool_call", ToolCall: &core.ToolCall{ID: "call", Name: "lookup", Arguments: json.RawMessage(`{}`)}}}, FinishReason: core.FinishToolCalls},
		{Role: core.RoleTool, ToolCallID: "call", Content: []core.Content{{Type: "text", Text: strings.Repeat("tool line\n", 8192)}, {Type: "image", MediaType: "image/png", Data: json.RawMessage(`"eA=="`)}}},
	}}
	return config, request
}

func validationOrigin(field string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"astral_models": map[string]string{"protocol": Protocol, "scope": "scope", "model": "remote", "field": field}})
	return raw
}

func TestValidationOnlyOmitsMaterializingText(t *testing.T) {
	config, request := validationFixture()
	m, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := m.requestBody(request, false)
	if err != nil {
		t.Fatal(err)
	}
	checked, err := m.requestBody(request, true)
	if err != nil {
		t.Fatal(err)
	}
	actualMessages, checkedMessages := actual["messages"].([]any), checked["messages"].([]any)
	if actualMessages[0].(map[string]any)["content"] != request.Messages[0].Content[0].Text || actualMessages[1].(map[string]any)["content"] != request.Messages[1].Content[0].Text+toolImageHint {
		t.Fatal("the actual sending path no longer includes complete text")
	}
	if checkedMessages[0].(map[string]any)["content"] != "" || checkedMessages[1].(map[string]any)["content"] != "" {
		t.Fatal("validation materialized assistant/tool text that will not be sent")
	}
	for _, text := range []string{"", "nonempty"} {
		request.Messages[0].Content[0].Text = text
		body, err := m.requestBody(request, true)
		if err != nil {
			t.Fatal(err)
		}
		content := body["messages"].([]any)[0].(map[string]any)["content"]
		if text == "" && content != nil || text != "" && content != "" {
			t.Fatal("validation changed the assistant's nil/empty content branch")
		}
	}
}

func TestValidationTextPlaceholdersPreserveAllProtocolErrors(t *testing.T) {
	for name, mutate := range map[string]func(*models.Config, *core.Request){
		"ordinary": func(*models.Config, *core.Request) {},
		"HTML and UTF8": func(_ *models.Config, r *core.Request) {
			r.Messages[0].Content[0].Text = "<>&中文\u2028\u2029"
			r.Messages[1].Content[0].Text = "<>&中文\u2028\u2029"
		},
		"malformed UTF8": func(_ *models.Config, r *core.Request) {
			r.Messages[0].Content[0].Text = "invalid\xff"
			r.Messages[1].Content[0].Text = "invalid\xff"
		},
		"native content": func(_ *models.Config, r *core.Request) {
			r.Messages[0].Content[0].Data = json.RawMessage(`{"content":[{"type":"thinking","thinking":"native","signature":"opaque"}]}`)
			r.Messages[0].Content[0].Metadata = validationOrigin("chat.content")
		},
		"native reasoning": func(_ *models.Config, r *core.Request) {
			r.Messages[0].Content = append(r.Messages[0].Content, core.Content{Type: "reasoning", Text: "plan", Data: json.RawMessage(`{"reasoning_content":"plan"}`), Metadata: validationOrigin("chat.reasoning")})
		},
		"foreign native origin": func(_ *models.Config, r *core.Request) {
			r.Messages[0].Content[0].Data = json.RawMessage(`{"content":[]}`)
			r.Messages[0].Content[0].Metadata = validationOrigin("different-field")
		},
		"encrypted reasoning wrong replay": func(_ *models.Config, r *core.Request) {
			r.Messages[0].Content = append(r.Messages[0].Content, core.Content{Type: "reasoning", Data: json.RawMessage(`{"reasoning_details":[{"type":"reasoning.encrypted","data":"opaque"}]}`), Metadata: validationOrigin("chat.reasoning")})
		},
		"missing required reasoning": func(c *models.Config, _ *core.Request) {
			c.RequireReasoningReplay, c.ReasoningReplay = true, "reasoning_details"
		},
		"invalid native object": func(_ *models.Config, r *core.Request) {
			r.Messages[0].Content = append(r.Messages[0].Content, core.Content{Type: "reasoning", Data: json.RawMessage(`[]`), Metadata: validationOrigin("chat.reasoning")})
		},
		"unsupported assistant part": func(_ *models.Config, r *core.Request) {
			r.Messages[0].Content = append(r.Messages[0].Content, core.Content{Type: "audio"})
		},
		"unsupported tool part": func(_ *models.Config, r *core.Request) {
			r.Messages[1].Content = append(r.Messages[1].Content, core.Content{Type: "audio"})
		},
		"invalid image URL": func(_ *models.Config, r *core.Request) {
			r.Messages[1].Content[1].Data = nil
			r.Messages[1].Content[1].URL = "file://not-resolved"
		},
		"invalid arguments": func(_ *models.Config, r *core.Request) {
			r.Messages[0].Content[1].ToolCall.Arguments = json.RawMessage(`{`)
		},
		"valid array arguments": func(_ *models.Config, r *core.Request) {
			r.Messages[0].Content[1].ToolCall.Arguments = json.RawMessage(`[]`)
		},
		"invalid options": func(_ *models.Config, r *core.Request) { n := math.NaN(); r.Options.Temperature = &n },
		"undeclared tool choice": func(_ *models.Config, r *core.Request) {
			r.Options.ToolChoice = core.ToolChoice{Mode: "tool", Name: "unknown"}
		},
		"forbidden message extension": func(_ *models.Config, r *core.Request) {
			r.Options.ProviderOptions = map[string]json.RawMessage{Protocol: json.RawMessage(`{"messages":[]}`)}
		},
		"conflicting extension": func(_ *models.Config, r *core.Request) {
			n := 0.3
			r.Options.Temperature = &n
			r.Options.ProviderOptions = map[string]json.RawMessage{Protocol: json.RawMessage(`{"temperature":0.5}`)}
		},
		"matching extension": func(_ *models.Config, r *core.Request) {
			n := 0.3
			r.Options.Temperature = &n
			r.Options.ProviderOptions = map[string]json.RawMessage{Protocol: json.RawMessage(`{"temperature":0.3}`)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			config, request := validationFixture()
			mutate(&config, &request)
			m, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			_, oldErr := m.requestBody(request, false)
			switch name {
			case "ordinary", "HTML and UTF8", "malformed UTF8", "native content", "native reasoning", "matching extension", "valid array arguments":
				if oldErr != nil {
					t.Fatalf("valid fixture failed the actual builder: %v", oldErr)
				}
			default:
				if oldErr == nil {
					t.Fatal("invalid fixture did not exercise an actual protocol error")
				}
			}
			validationErr := m.ValidateRequest(context.Background(), request)
			if (oldErr == nil) != (validationErr == nil) || oldErr != nil && (reflect.TypeOf(oldErr) != reflect.TypeOf(validationErr) || oldErr.Error() != validationErr.Error()) {
				t.Fatalf("validation changed the actual body builder's failure: old=%v validation=%v", oldErr, validationErr)
			}
		})
	}
}
