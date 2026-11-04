package astral

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// These comparisons preserve encoding/json equality without serializing the
// unchanged multimodal body. The fallback is confined to the changed field.
func sameRequestString(a, b string) bool {
	return a == b || sameJSON(a, b)
}

func sameRequestRaw(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	return bytes.Equal(a, b) || sameJSON(a, b)
}

func sameRequestContent(a, b Content) bool {
	if !sameRequestString(a.Type, b.Type) || !sameRequestString(a.Text, b.Text) ||
		!sameRequestString(a.MediaType, b.MediaType) || !sameRequestString(a.URL, b.URL) ||
		!sameRequestRaw(a.Data, b.Data) || !sameRequestRaw(a.Metadata, b.Metadata) {
		return false
	}
	if a.ToolCall == nil || b.ToolCall == nil {
		return a.ToolCall == b.ToolCall
	}
	return sameRequestString(a.ToolCall.ID, b.ToolCall.ID) && sameRequestString(a.ToolCall.Name, b.ToolCall.Name) && sameRequestRaw(a.ToolCall.Arguments, b.ToolCall.Arguments) && sameRequestRaw(a.ToolCall.Metadata, b.ToolCall.Metadata)
}

func sameRequestMessage(a, b Message) bool {
	if a.IsError != b.IsError || len(a.Content) != len(b.Content) ||
		!sameRequestString(string(a.Role), string(b.Role)) || !sameRequestString(a.ToolCallID, b.ToolCallID) ||
		!sameRequestString(a.Name, b.Name) || !sameRequestString(a.ContextKind, b.ContextKind) ||
		!sameRequestString(string(a.FinishReason), string(b.FinishReason)) || !sameRequestString(a.ErrorMessage, b.ErrorMessage) ||
		!sameRequestRaw(a.Metadata, b.Metadata) {
		return false
	}
	for i := range a.Content {
		if !sameRequestContent(a.Content[i], b.Content[i]) {
			return false
		}
	}
	return true
}

func sameRequestMessages(a, b []Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameRequestMessage(a[i], b[i]) {
			return false
		}
	}
	return true
}

// Request's small envelope and its ordered messages are compared separately.
// Generic JSON diff remains the compatibility implementation for changed
// messages, so array edits and opaque JSON retain their existing semantics.
func diffPreparedRequest(base, actual Request) ([]RequestChange, error) {
	a, b := base, actual
	a.Messages, b.Messages = nil, nil
	before, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	after, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	changes := diffRequestJSON(nil, before, after)
	if len(base.Messages) != len(actual.Messages) || (len(base.Messages) == 0 && (base.Messages == nil) != (actual.Messages == nil)) {
		before, err = json.Marshal(base.Messages)
		if err != nil {
			return nil, err
		}
		after, err = json.Marshal(actual.Messages)
		if err != nil {
			return nil, err
		}
		return append(changes, diffRequestJSON([]string{"messages"}, before, after)...), nil
	}
	for i, message := range actual.Messages {
		if sameRequestMessage(base.Messages[i], message) {
			continue
		}
		before, err = json.Marshal(base.Messages[i])
		if err != nil {
			return nil, err
		}
		after, err = json.Marshal(message)
		if err != nil {
			return nil, err
		}
		changes = append(changes, diffRequestJSON([]string{"messages", strconv.Itoa(i)}, before, after)...)
	}
	return changes, nil
}
