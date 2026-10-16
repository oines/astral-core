// Package demo supplies deterministic model adapters for runnable examples.
// It does not call a provider or interpret natural language. Its token estimate
// is illustrative; production adapters must count their actual wire request.
package demo

import (
	"context"
	"encoding/json"

	astral "github.com/oines/astral-core"
)

func Model(reply func(astral.Request) (astral.Reply, error)) astral.Model {
	return astral.ModelFunc{
		Specification: astral.ModelSpec{ID: "demo", ContextWindow: 128000, MaxOutputTokens: 1024},
		Generate: func(ctx context.Context, request astral.Request, emit func(astral.ModelEvent)) (astral.Reply, error) {
			if err := ctx.Err(); err != nil {
				return astral.Reply{}, err
			}
			result, err := reply(request)
			if err == nil {
				for i, part := range result.Message.Content {
					event := astral.ModelEvent{ContentIndex: &i, Text: part.Text, Data: part.Data, Metadata: part.Metadata}
					switch part.Type {
					case "text":
						event.Type = "text_delta"
					case "reasoning":
						event.Type = "reasoning_delta"
					case "tool_call":
						event.Type = "tool_call_delta"
						if part.ToolCall != nil {
							event.ToolCallID = part.ToolCall.ID
							event.Text = string(part.ToolCall.Arguments)
						}
					default:
						event.Type = "provider_content"
					}
					emit(event)
				}
			}
			return result, err
		},
	}
}

func Answer(text string) astral.Reply {
	return astral.Reply{Message: astral.Message{Role: astral.RoleAssistant, Content: astral.Text(text), FinishReason: astral.FinishStop}}
}
func Calls(calls ...astral.ToolCall) astral.Reply {
	content := make([]astral.Content, len(calls))
	for i := range calls {
		content[i] = astral.Content{Type: "tool_call", ToolCall: &calls[i]}
	}
	return astral.Reply{Message: astral.Message{Role: astral.RoleAssistant, Content: content, FinishReason: astral.FinishToolCalls}}
}
func Call(id, name, arguments string) astral.ToolCall {
	return astral.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(arguments)}
}
func LastTool(request astral.Request, name string) *astral.Message {
	for i := len(request.Messages) - 1; i >= 0; i-- {
		if request.Messages[i].Role == astral.RoleTool && request.Messages[i].Name == name {
			message := request.Messages[i]
			return &message
		}
	}
	return nil
}

func EstimateTokens(_ context.Context, _ astral.Model, request astral.Request) (int, error) {
	encoded, err := json.Marshal(request)
	return (len(encoded) + 3) / 4, err
}
