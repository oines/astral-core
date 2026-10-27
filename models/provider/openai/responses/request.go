package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/models/internal/client"
)

// RequestBody returns the exact credential-free body. Core owns conversation
// state; requests never depend on server-side previous_response_id storage.
func (m *Model) RequestBody(req astral.Request) (map[string]any, error) {
	var err error
	req, err = m.client.WireRequest(req, false)
	if err != nil {
		return nil, err
	}
	if err := m.client.Validate(req); err != nil {
		return nil, err
	}
	input := []any{}
	for _, message := range req.Messages {
		switch message.Role {
		case astral.RoleAssistant:
			for _, part := range message.Content {
				item, err := m.assistantItem(part)
				if err != nil {
					return nil, err
				}
				input = append(input, item)
			}
		case astral.RoleTool:
			parts, err := inputParts(message.Content)
			if err != nil {
				return nil, err
			}
			var output any = parts
			if len(parts) == 1 && message.Content[0].Type == "text" {
				output = message.Content[0].Text
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": message.ToolCallID, "output": output})
		default:
			parts, err := inputParts(message.Content)
			if err != nil {
				return nil, err
			}
			input = append(input, map[string]any{"role": string(message.Role), "content": parts})
		}
	}
	body := map[string]any{
		"model": m.client.Config.Model, "input": input,
		"stream": m.client.Streaming(),
	}
	if req.MaxOutputTokens > 0 {
		body["max_output_tokens"] = req.MaxOutputTokens
	}
	compatibility := m.client.Config.ResponsesCompatibility
	if compatibility == nil || compatibility.SupportsStore == nil || *compatibility.SupportsStore {
		body["store"] = false
	}
	if compatibility == nil || ((compatibility.SupportsInclude == nil || *compatibility.SupportsInclude) &&
		(compatibility.SupportsEncryptedReasoningInclude == nil || *compatibility.SupportsEncryptedReasoningInclude)) {
		body["include"] = []string{"reasoning.encrypted_content"}
	}
	if req.SystemPrompt != "" {
		body["instructions"] = req.SystemPrompt
	}
	if m.client.Config.Codex {
		delete(body, "max_output_tokens")
		body["instructions"] = req.SystemPrompt
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			tools = append(tools, map[string]any{"type": "function", "name": tool.Name,
				"description": tool.Description, "parameters": tool.Parameters, "strict": false})
		}
		body["tools"] = tools
	}
	options := req.Options
	if options.Seed != nil || options.Stop != nil || options.FrequencyPenalty != nil || options.PresencePenalty != nil {
		return nil, fmt.Errorf("%w: Responses does not support seed, stop, or frequency/presence penalties", astral.ErrInvalid)
	}
	if options.Temperature != nil {
		body["temperature"] = *options.Temperature
	}
	if options.TopP != nil {
		body["top_p"] = *options.TopP
	}
	if choice := options.ToolChoice; choice.Mode != "" {
		if choice.Mode == "tool" {
			body["tool_choice"] = map[string]any{"type": "function", "name": choice.Name}
		} else {
			body["tool_choice"] = choice.Mode
		}
	}
	if format := options.ResponseFormat; format != nil {
		wire := map[string]any{"type": format.Type}
		if format.Type == "json_schema" {
			wire["name"], wire["schema"] = format.Name, format.Schema
			if format.Strict != nil {
				wire["strict"] = *format.Strict
			}
		}
		body["text"] = map[string]any{"format": wire}
	}
	if err := m.client.ApplyReasoning(body, req); err != nil {
		return nil, err
	}
	if err := m.client.ApplyExtensions(body, req); err != nil {
		return nil, err
	}
	return body, nil
}

func inputParts(parts []astral.Content) ([]any, error) {
	result := make([]any, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "text":
			result = append(result, map[string]any{"type": "input_text", "text": part.Text})
		case "image":
			if part.URL == "" {
				return nil, fmt.Errorf("%w: image requires a resolved URL or data URL", astral.ErrInvalid)
			}
			result = append(result, map[string]any{"type": "input_image", "image_url": part.URL})
		default:
			return nil, fmt.Errorf("%w: unsupported Responses input part %q", astral.ErrInvalid, part.Type)
		}
	}
	return result, nil
}

func (m *Model) assistantItem(part astral.Content) (any, error) {
	if len(part.Data) > 0 {
		if err := m.client.Replay(part, "responses.item"); err != nil {
			return nil, err
		}
		// The accepted native item is the replay authority. Parsed fields are
		// projections for execution/display, not a second copy to reconcile.
		return json.RawMessage(part.Data), nil
	}
	switch part.Type {
	case "text":
		return map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": part.Text}}}, nil
	case "tool_call":
		call := part.ToolCall
		return map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Name, "arguments": string(call.Arguments)}, nil
	default:
		return nil, fmt.Errorf("%w: %q requires its native Responses replay item", astral.ErrInvalid, part.Type)
	}
}

func decodeItem(raw json.RawMessage, complete bool) (astral.Content, error) {
	var item struct {
		Type, Role, ID  string
		CallID          string `json:"call_id"`
		Name, Arguments string
		Content         []struct{ Type, Text, Refusal string }
		Summary         []struct{ Type, Text string }
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return astral.Content{}, err
	}
	part := astral.Content{Data: append(json.RawMessage(nil), raw...)}
	switch item.Type {
	case "reasoning":
		part.Type = "reasoning"
		var display strings.Builder
		for _, summary := range item.Summary {
			display.WriteString(summary.Text)
		}
		if len(item.Summary) == 0 {
			for _, content := range item.Content {
				display.WriteString(content.Text)
			}
		}
		part.Text = display.String()
	case "message":
		if item.Role != "" && item.Role != "assistant" {
			return part, fmt.Errorf("%w: output message has role %q", astral.ErrIncompleteResponse, item.Role)
		}
		part.Type = "text"
		var display strings.Builder
		for _, content := range item.Content {
			if content.Type == "refusal" {
				display.WriteString(content.Refusal)
			} else {
				display.WriteString(content.Text)
			}
		}
		part.Text = display.String()
	case "function_call":
		part.Type = "tool_call"
		if complete && (item.CallID == "" || item.Name == "" || !client.IsObject([]byte(item.Arguments))) {
			return part, fmt.Errorf("%w: incomplete function-call identity or arguments", astral.ErrIncompleteResponse)
		}
		arguments := json.RawMessage(item.Arguments)
		if !json.Valid(arguments) {
			arguments = json.RawMessage(`{}`)
		}
		part.ToolCall = &astral.ToolCall{ID: item.CallID, Name: item.Name, Arguments: arguments}
	case "":
		return part, fmt.Errorf("%w: output item without type", astral.ErrIncompleteResponse)
	default:
		part.Type = "provider"
	}
	return part, nil
}
