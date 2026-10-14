package session

import (
	protocol "github.com/oines/astral-core"
)

func validateMessage(m protocol.Message) error {
	if m.Role != protocol.RoleUser && m.Role != protocol.RoleAssistant && m.Role != protocol.RoleTool && m.Role != protocol.RoleContext {
		return fmtInvalid("unknown message role %q", m.Role)
	}
	if len(m.Content) == 0 && len(m.ToolCalls) == 0 {
		return fmtInvalid("empty message")
	}
	if m.Role != protocol.RoleAssistant && len(m.ToolCalls) > 0 {
		return fmtInvalid("only assistant messages may call tools")
	}
	if m.Role == protocol.RoleTool && m.ToolCallID == "" {
		return fmtInvalid("tool result without call id")
	}
	if m.Role != protocol.RoleTool && m.ToolCallID != "" {
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

func validateUser(m protocol.Message) error {
	if m.Role != protocol.RoleUser {
		return fmtInvalid("input must be a user message")
	}
	return validateMessage(m)
}
