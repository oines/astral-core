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

// Anchors are conservative: a size change is a replacement. This filter avoids
// repeatedly JSON-normalizing a large new part against unrelated small parts.
func sameRequestContentAnchor(a, b Content) bool {
	if a.Type != b.Type || len(a.Text) != len(b.Text) || len(a.URL) != len(b.URL) ||
		len(a.Data) != len(b.Data) || len(a.Metadata) != len(b.Metadata) {
		return false
	}
	if a.ToolCall == nil || b.ToolCall == nil {
		if a.ToolCall != b.ToolCall {
			return false
		}
	} else if len(a.ToolCall.Arguments) != len(b.ToolCall.Arguments) || len(a.ToolCall.Metadata) != len(b.ToolCall.Metadata) {
		return false
	}
	return sameRequestContent(a, b)
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
		old, next := base.Messages[i], message
		old.Content, next.Content = nil, nil
		before, err = json.Marshal(old)
		if err != nil {
			return nil, err
		}
		after, err = json.Marshal(next)
		if err != nil {
			return nil, err
		}
		changes = append(changes, diffRequestJSON([]string{"messages", strconv.Itoa(i)}, before, after)...)
		contentChanges, err := diffPreparedContent([]string{"messages", strconv.Itoa(i), "content"}, base.Messages[i].Content, message.Content)
		if err != nil {
			return nil, err
		}
		changes = append(changes, contentChanges...)
	}
	return changes, nil
}

// Walk the typed content containers before encoding. Removed images need only
// a remove operation; replacing an image with text encodes only the new text.
// Unchanged payloads never pass through generic JSON comparison.
func diffPreparedContent(path []string, before, after []Content) ([]RequestChange, error) {
	var changes []RequestChange
	if len(before) == 0 && len(after) > 0 {
		raw, err := json.Marshal(after)
		return []RequestChange{{Path: path, Op: "set", Value: raw}}, err
	}
	if len(before) > 0 && len(after) == 0 {
		return []RequestChange{{Path: path, Op: "remove"}}, nil
	}
	if len(before) != len(after) {
		// Retain unchanged parts between several changed spans too, such as
		// two collapsed image groups with a large text/opaque part between them.
		oldStart, newStart := 0, 0
		replaceSpan := func(oldEnd, newEnd int) error {
			for i := oldEnd - oldStart - 1; i >= 0; i-- {
				changes = append(changes, RequestChange{Path: jsonPath(path, strconv.Itoa(newStart+i)), Op: "remove"})
			}
			for i := newStart; i < newEnd; i++ {
				raw, err := json.Marshal(after[i])
				if err != nil {
					return err
				}
				changes = append(changes, RequestChange{Path: jsonPath(path, strconv.Itoa(i)), Op: "insert", Value: raw})
			}
			return nil
		}
		for i, part := range after {
			if !largeRequestContent(part) {
				continue
			}
			at := oldStart
			for at < len(before) && !sameRequestContentAnchor(before[at], part) {
				at++
			}
			if at == len(before) {
				break
			}
			if err := replaceSpan(at, i); err != nil {
				return nil, err
			}
			oldStart, newStart = at+1, i+1
		}
		err := replaceSpan(len(before), len(after))
		return changes, err
	}
	for i, part := range after {
		if sameRequestContent(before[i], part) {
			continue
		}
		raw, err := json.Marshal(part)
		if err != nil {
			return nil, err
		}
		at := jsonPath(path, strconv.Itoa(i))
		if before[i].Type != part.Type && !largeRequestContent(part) {
			changes = append(changes, RequestChange{Path: at, Op: "set", Value: raw})
			continue
		}
		old, err := json.Marshal(before[i])
		if err != nil {
			return nil, err
		}
		changes = append(changes, diffRequestJSON(at, old, raw)...)
	}
	return changes, nil
}
