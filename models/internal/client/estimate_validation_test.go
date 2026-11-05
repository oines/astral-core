package client

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models"
)

// Retain the previous operation order as an oracle: normalize, validate every
// real image, validate all original messages, then estimate original content.
func estimateBeforeImagePlaceholders(c *Client, req core.Request) (int, error) {
	if needsRequestNormalization(req) {
		var err error
		req, err = cloneRequest(req)
		if err != nil {
			return 0, err
		}
	}
	for _, message := range req.Messages {
		for _, part := range message.Content {
			if part.Type == "image" {
				if _, err := c.imageData(part); err != nil {
					return 0, err
				}
			}
		}
	}
	if err := c.Validate(req); err != nil {
		return 0, err
	}
	return c.Estimate(req)
}

func estimateValidationFixture() core.Request {
	return core.Request{ModelID: "target", ModelRevision: "1", SystemPrompt: "rules <>&中文", MaxOutputTokens: 1000,
		Tools:   []core.ToolDefinition{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Options: core.GenerationOptions{ProviderOptions: map[string]json.RawMessage{"openai-responses": json.RawMessage(`{"opaque":"keep"}`)}},
		Messages: []core.Message{
			{Role: core.RoleUser, Metadata: json.RawMessage(`{"source":"keep"}`), Content: []core.Content{
				{Type: "text", Text: "<>&中文😀\n"},
				{Type: "image", MediaType: "image/png", URL: "host-file://frozen", Data: json.RawMessage(`"` + strings.Repeat("eHh4", 2048) + `"`), Metadata: json.RawMessage(`{"image":"keep"}`)},
				{Type: "provider", Data: json.RawMessage(`{"opaque":9007199254740993}`)},
			}},
			{Role: core.RoleAssistant, Content: []core.Content{{Type: "tool_call", ToolCall: &core.ToolCall{ID: "call", Name: "lookup", Arguments: json.RawMessage(` {"query":"keep"} `), Metadata: json.RawMessage(`{"call":"keep"}`)}}}},
			{Role: core.RoleTool, ToolCallID: "call", Content: core.Text("tool result")},
		}}
}

func TestEstimateImagePlaceholdersMatchPreviousCountsAndErrors(t *testing.T) {
	for name, test := range map[string]struct {
		valid  bool
		mutate func(*Client, *core.Request)
	}{
		"frozen":         {true, func(*Client, *core.Request) {}},
		"escaped base64": {true, func(_ *Client, r *core.Request) { r.Messages[0].Content[1].Data = json.RawMessage(`"\u0065A=="`) }},
		"base64 CRLF":    {true, func(_ *Client, r *core.Request) { r.Messages[0].Content[1].Data = json.RawMessage(`"eA==\r\n"`) }},
		"padded bits":    {true, func(_ *Client, r *core.Request) { r.Messages[0].Content[1].Data = json.RawMessage(`"AB=="`) }},
		"HTTP URL only": {true, func(_ *Client, r *core.Request) {
			r.Messages[0].Content[1].Data = nil
			r.Messages[0].Content[1].URL = "https://example.invalid/image.png"
		}},
		"data URL only": {true, func(_ *Client, r *core.Request) {
			r.Messages[0].Content[1].Data = nil
			r.Messages[0].Content[1].URL = "data:image/png;base64,eA=="
		}},
		"normalization": {true, func(_ *Client, r *core.Request) {
			r.Options.Stop = []string{}
			r.Messages[1].Content[0].ToolCall.Arguments = nil
			r.Messages[0].Content[0].Text = "bad\xff"
		}},
		"custom image weight":   {true, func(c *Client, _ *core.Request) { c.Config.ImageTokenBudget = 3000 }},
		"negative image weight": {false, func(c *Client, _ *core.Request) { c.Config.ImageTokenBudget = -1 }},
		"invalid base64 before model": {false, func(_ *Client, r *core.Request) {
			r.ModelID = "wrong"
			r.Messages[0].Content[1].Data = json.RawMessage(`"invalid!"`)
		}},
		"invalid image JSON": {false, func(_ *Client, r *core.Request) { r.Messages[0].Content[1].Data = json.RawMessage(`{`) }},
		"invalid image URL": {false, func(_ *Client, r *core.Request) {
			r.Messages[0].Content[1].Data = nil
			r.Messages[0].Content[1].URL = "file://unresolved"
		}},
		"invalid image metadata":           {false, func(_ *Client, r *core.Request) { r.Messages[0].Content[1].Metadata = json.RawMessage(`{`) }},
		"accepted malformed UTF8 metadata": {true, func(_ *Client, r *core.Request) { r.Messages[0].Content[1].Metadata = json.RawMessage{'"', 0xff, '"'} }},
		"invalid message metadata":         {false, func(_ *Client, r *core.Request) { r.Messages[0].Metadata = json.RawMessage(`{`) }},
		"invalid opaque data":              {false, func(_ *Client, r *core.Request) { r.Messages[0].Content[2].Data = json.RawMessage(`{`) }},
		"invalid tool metadata":            {false, func(_ *Client, r *core.Request) { r.Messages[1].Content[0].ToolCall.Metadata = json.RawMessage(`{`) }},
		"invalid tool arguments":           {false, func(_ *Client, r *core.Request) { r.Messages[1].Content[0].ToolCall.Arguments = json.RawMessage(`{`) }},
		"valid array arguments":            {true, func(_ *Client, r *core.Request) { r.Messages[1].Content[0].ToolCall.Arguments = json.RawMessage(`[]`) }},
		"invalid tool definition":          {false, func(_ *Client, r *core.Request) { r.Tools[0].Parameters = json.RawMessage(`[]`) }},
	} {
		t.Run(name, func(t *testing.T) {
			c := &Client{Protocol: "openai-responses", Scope: "scope", Config: models.Config{Spec: core.ModelSpec{ID: "target", Revision: "1"}, Model: "remote"}}
			request, original := estimateValidationFixture(), estimateValidationFixture()
			test.mutate(c, &request)
			unused := &Client{}
			test.mutate(unused, &original)
			want, oldErr := estimateBeforeImagePlaceholders(c, request)
			got, err := c.EstimateRequest(request)
			if (oldErr == nil) != test.valid || (oldErr == nil) != (err == nil) || got != want || err != nil && (reflect.TypeOf(oldErr) != reflect.TypeOf(err) || oldErr.Error() != err.Error()) {
				t.Fatalf("estimate/count/error order changed: old=%d/%v new=%d/%v", want, oldErr, got, err)
			}
			if !reflect.DeepEqual(request, original) {
				t.Fatal("estimation changed its source request")
			}
		})
	}
}

func TestEstimateImagePlaceholdersRetainNativePayloadValidation(t *testing.T) {
	for protocol, field := range map[string]string{"openai-completions": "chat.image", "openai-responses": "responses.item", "anthropic-messages": "anthropic.block", "google-generative-ai": "google.part"} {
		for _, raw := range []json.RawMessage{json.RawMessage(`{"opaque":"native"}`), json.RawMessage(`{`)} {
			c := &Client{Protocol: protocol, Scope: "scope", Config: models.Config{Spec: core.ModelSpec{ID: "target", Revision: "1"}, Model: "remote"}}
			metadata := c.Metadata(field)
			beforeData, beforeMetadata := string(raw), string(metadata)
			request := core.Request{ModelID: "target", ModelRevision: "1", Messages: []core.Message{{Role: core.RoleAssistant, Content: []core.Content{{Type: "image", Data: raw, Metadata: metadata}}}}}
			want, oldErr := estimateBeforeImagePlaceholders(c, request)
			got, err := c.EstimateRequest(request)
			if (oldErr == nil) != (err == nil) || got != want || err != nil && (reflect.TypeOf(oldErr) != reflect.TypeOf(err) || oldErr.Error() != err.Error()) {
				t.Fatalf("native image validation changed: protocol=%s old=%d/%v new=%d/%v", protocol, want, oldErr, got, err)
			}
			if &request.Messages[0].Content[0].Data[0] != &raw[0] || string(request.Messages[0].Content[0].Data) != beforeData || string(request.Messages[0].Content[0].Metadata) != beforeMetadata {
				t.Fatal("native source payload was replaced")
			}
		}
	}
}
