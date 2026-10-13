package astral

import (
	"encoding/json"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
	// RoleContext is model-visible context, never a system instruction. Adapters
	// must preserve its position and must not promote referenced file contents.
	RoleContext Role = "context"
)

// Content preserves text, multimodal parts and opaque provider payloads. Type is
// open-ended: adapters must reject unsupported parts rather than silently drop them.
type Content struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	MediaType string          `json:"media_type,omitempty"`
	URL       string          `json:"url,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

type Message struct {
	Role        Role            `json:"role"`
	Content     []Content       `json:"content,omitempty"`
	ToolCalls   []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID  string          `json:"tool_call_id,omitempty"`
	Name        string          `json:"name,omitempty"`
	IsError     bool            `json:"is_error,omitempty"`
	ContextKind string          `json:"context_kind,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
}

func Text(text string) []Content { return []Content{{Type: "text", Text: text}} }

func User(text string) Message { return Message{Role: RoleUser, Content: Text(text)} }

func validateUser(m Message) error {
	if m.Role != RoleUser {
		return fmtInvalid("input must be a user message")
	}
	return validateMessage(m)
}

func validateMessage(m Message) error {
	if m.Role != RoleUser && m.Role != RoleAssistant && m.Role != RoleTool && m.Role != RoleContext {
		return fmtInvalid("unknown message role %q", m.Role)
	}
	if len(m.Content) == 0 && len(m.ToolCalls) == 0 {
		return fmtInvalid("empty message")
	}
	if m.Role != RoleAssistant && len(m.ToolCalls) > 0 {
		return fmtInvalid("only assistant messages may call tools")
	}
	if m.Role == RoleTool && m.ToolCallID == "" {
		return fmtInvalid("tool result without call id")
	}
	if m.Role != RoleTool && m.ToolCallID != "" {
		return fmtInvalid("unexpected tool result id")
	}
	ids := map[string]bool{}
	for _, c := range m.ToolCalls {
		if c.ID == "" || c.Name == "" || ids[c.ID] {
			return fmtInvalid("empty or duplicate tool call id/name")
		}
		ids[c.ID] = true
		if _, err := decodeJSON(c.Arguments); err != nil {
			return fmtInvalid("invalid tool argument JSON: %v", err)
		}
	}
	for _, p := range m.Content {
		if p.Type == "" {
			return fmtInvalid("content part without type")
		}
	}
	if _, err := copyJSON(m); err != nil {
		return fmtInvalid("message is not JSON serializable: %v", err)
	}
	return nil
}
