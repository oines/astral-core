package client

import (
	"context"
	"encoding/json"

	core "github.com/oines/astral-core"
	models "github.com/oines/astral-core/models"
)

func (c *Client) RecoverRequest(ctx context.Context, req core.Request, cause error) (core.Request, bool, error) {
	if err := ctx.Err(); err != nil {
		return core.Request{}, false, err
	}
	if !models.IsEncryptedReasoningError(cause) {
		return core.Request{}, false, nil
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return core.Request{}, false, err
	}
	var owned core.Request
	if err = json.Unmarshal(raw, &owned); err != nil {
		return core.Request{}, false, err
	}
	changed := false
	messages := owned.Messages[:0]
	for _, message := range owned.Messages {
		if message.Role != core.RoleAssistant {
			messages = append(messages, message)
			continue
		}
		parts := message.Content[:0]
		for _, part := range message.Content {
			var origin metadata
			if json.Unmarshal(part.Metadata, &origin) != nil || origin.Origin.Protocol != c.Protocol || origin.Origin.Scope != c.Scope || origin.Origin.Model != c.Config.Model {
				parts = append(parts, part)
				continue
			}
			data, err := Object(part.Data)
			if err != nil {
				return core.Request{}, false, err
			}
			drop := false
			switch c.Protocol {
			case "openai-responses":
				if part.Type == "reasoning" && data["encrypted_content"] != nil {
					drop = true
				}
			case "anthropic-messages":
				if data["type"] == "redacted_thinking" || data["type"] == "thinking" && data["signature"] != nil {
					drop = true
				}
			case "google-generative-ai":
				if data["thoughtSignature"] != nil {
					delete(data, "thoughtSignature")
					changed = true
					if thought, _ := data["thought"].(bool); thought {
						drop = true
					} else {
						part.Data, _ = json.Marshal(data)
					}
				}
			case "openai-completions":
				if details, ok := data["reasoning_details"].([]any); ok {
					kept := []any{}
					for _, value := range details {
						entry, _ := value.(map[string]any)
						if entry["type"] == "reasoning.encrypted" {
							changed = true
							continue
						}
						kept = append(kept, value)
					}
					data["reasoning_details"] = kept
					part.Data, _ = json.Marshal(data)
					if len(kept) == 0 && part.Text == "" && data["reasoning_content"] == nil && data["reasoning"] == nil {
						drop = true
					}
				}
			}
			if drop {
				changed = true
				continue
			}
			parts = append(parts, part)
		}
		message.Content = parts
		if len(parts) > 0 {
			messages = append(messages, message)
		}
	}
	owned.Messages = messages
	return owned, changed, nil
}
