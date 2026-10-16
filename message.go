package astral

import (
	"encoding/json"
	"strings"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
	RoleDeveloper Role = "developer"
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
	ToolCall  *ToolCall       `json:"tool_call,omitempty"`
}

type Message struct {
	Role         Role            `json:"role"`
	Content      []Content       `json:"content,omitempty"`
	ToolCallID   string          `json:"tool_call_id,omitempty"`
	Name         string          `json:"name,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	ContextKind  string          `json:"context_kind,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	FinishReason FinishReason    `json:"finish_reason,omitempty"`
	ErrorMessage string          `json:"error_message,omitempty"`
}

func Text(text string) []Content { return []Content{{Type: "text", Text: text}} }

func User(text string) Message { return Message{Role: RoleUser, Content: Text(text)} }

// ToolCalls derives the current batch from its sole ordered source. Returned
// values share their JSON payloads with this caller-owned message; no batch is
// cached or persisted separately from Content.
func (m Message) ToolCalls() []ToolCall {
	var calls []ToolCall
	for _, part := range m.Content {
		if part.Type == "tool_call" && part.ToolCall != nil {
			calls = append(calls, *part.ToolCall)
		}
	}
	return calls
}

// UnmarshalJSON refuses the removed parallel call list, including an empty or
// null list. Ignoring it would silently turn a tool response into plain text.
func (m *Message) UnmarshalJSON(data []byte) error {
	type plainMessage Message
	var wire struct {
		plainMessage
		LegacyCalls json.RawMessage `json:"tool_calls"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if len(wire.LegacyCalls) > 0 {
		return fmtInvalid("message.tool_calls was removed; use ordered tool_call content blocks")
	}
	*m = Message(wire.plainMessage)
	return nil
}

func validateUser(m Message) error {
	if m.Role != RoleUser {
		return fmtInvalid("input must be a user message")
	}
	return validateMessage(m)
}

func validateMessage(m Message) error { return m.Validate() }

// Validate checks the common message contract used by Core and persistent
// sessions. Provider-defined part types remain opaque and are never executed.
func (m Message) Validate() error {
	if m.Role != RoleUser && m.Role != RoleAssistant && m.Role != RoleTool && m.Role != RoleDeveloper {
		return fmtInvalid("unknown message role %q", m.Role)
	}
	if m.Role != RoleAssistant && (m.FinishReason != "" || m.ErrorMessage != "") {
		return fmtInvalid("only assistant messages may have a terminal status")
	}
	if m.FinishReason != "" && m.FinishReason != FinishStop && m.FinishReason != FinishToolCalls && m.FinishReason != FinishLength && m.FinishReason != FinishError && m.FinishReason != FinishCancelled {
		return fmtInvalid("unknown finish reason %q", m.FinishReason)
	}
	if len(m.Content) == 0 && !m.Failed() {
		return fmtInvalid("empty message")
	}
	if m.Role == RoleTool && m.ToolCallID == "" {
		return fmtInvalid("tool result without call id")
	}
	if m.Role != RoleTool && m.ToolCallID != "" {
		return fmtInvalid("unexpected tool result id")
	}
	ids := map[string]bool{}
	for _, part := range m.Content {
		if strings.TrimSpace(part.Type) == "" {
			return fmtInvalid("content part without type")
		}
		if part.Type != "tool_call" {
			if part.ToolCall != nil {
				return fmtInvalid("tool call payload requires tool_call content type")
			}
			continue
		}
		if m.Role != RoleAssistant {
			return fmtInvalid("only assistant messages may call tools")
		}
		if part.ToolCall == nil {
			return fmtInvalid("tool_call content without a tool call")
		}
		c := part.ToolCall
		if c.ID == "" || c.Name == "" || ids[c.ID] {
			return fmtInvalid("empty or duplicate tool call id/name")
		}
		ids[c.ID] = true
		if _, err := decodeJSON(c.Arguments); err != nil {
			return fmtInvalid("invalid tool argument JSON: %v", err)
		}
	}
	if _, err := json.Marshal(m); err != nil {
		return fmtInvalid("message is not JSON serializable: %v", err)
	}
	return nil
}

// Failed messages remain in history, but are omitted from model requests and
// never introduce executable tool calls.
func (m Message) Failed() bool {
	return m.Role == RoleAssistant && (m.FinishReason == FinishError || m.FinishReason == FinishCancelled)
}

// ValidateContext validates host-supplied passive context without interpreting
// its modalities, source kind or provider metadata.
func (m Message) ValidateContext() error {
	if m.Role != RoleUser && m.Role != RoleDeveloper {
		return fmtInvalid("context role must be user or developer")
	}
	return m.Validate()
}
