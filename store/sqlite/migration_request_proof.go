package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"

	core "github.com/oines/astral-core"
)

// These format-11 JSON recipe operations are frozen. Recipe selection is an
// editing shortcut, not proof: a same-role original could have been replaced by
// entirely different content. Rebuild the actual messages before claiming it.
func provenRecipeSourcesV11(ctx context.Context, tx *sql.Tx, attempt generationAttemptV11, sources []string) ([]string, error) {
	base := make([]json.RawMessage, len(sources))
	for i, source := range sources {
		message, err := projectedMessageV11(ctx, tx, attempt.Input.RolloutID, source)
		if err != nil {
			return nil, err
		}
		base[i] = message
	}
	selected := append([]json.RawMessage(nil), base...)
	if selection := attempt.Input.Selection; selection != nil {
		selected = make([]json.RawMessage, len(*selection))
		for i, at := range *selection {
			if at < -1 || at >= len(base) {
				return nil, core.ErrCorrupt
			}
			selected[i] = json.RawMessage(`{}`)
			if at >= 0 {
				selected[i] = base[at]
			}
		}
	}
	raw, err := json.Marshal(map[string]any{"messages": selected})
	if err != nil {
		return nil, err
	}
	for _, change := range attempt.Input.Changes {
		if len(change.Path) > 0 && change.Path[0] != "messages" {
			continue
		}
		raw, err = applyRequestChangeV11(raw, change.Path, change)
		if err != nil {
			return nil, err
		}
	}
	var request struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(raw, &request) != nil {
		return nil, core.ErrCorrupt
	}
	exact, content := map[string]string{}, map[string]string{}
	add := func(index map[string]string, key, source string) {
		if old, ok := index[key]; ok && old != source {
			index[key] = ""
		} else if !ok {
			index[key] = source
		}
	}
	for i, message := range base {
		key, err := messageContentKeyV11(message)
		if err != nil {
			return nil, err
		}
		add(exact, canonicalJSONV11(message), sources[i])
		add(content, key, sources[i])
	}
	proven := make([]string, len(request.Messages))
	for i, message := range request.Messages {
		key, err := messageContentKeyV11(message)
		if err != nil {
			return nil, err
		}
		if source, ok := exact[canonicalJSONV11(message)]; ok {
			proven[i] = source
		} else {
			proven[i] = content[key]
		}
	}
	return proven, nil
}

func canonicalJSONV11(raw json.RawMessage) string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func messageContentKeyV11(raw json.RawMessage) (string, error) {
	var message struct {
		Role        string          `json:"role"`
		Name        string          `json:"name"`
		ToolCallID  string          `json:"tool_call_id"`
		ContextKind string          `json:"context_kind"`
		Content     json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &message) != nil {
		return "", core.ErrCorrupt
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return "", err
	}
	return canonicalJSONV11(encoded), nil
}

func projectedMessageV11(ctx context.Context, tx *sql.Tx, rolloutID, source string) (json.RawMessage, error) {
	var raw []byte
	if err := tx.QueryRowContext(ctx, "SELECT payload FROM entries WHERE rollout_id=? AND id=? AND projection IS NOT NULL", rolloutID, source).Scan(&raw); err != nil {
		return nil, err
	}
	var entry struct {
		Kind       string          `json:"kind"`
		Message    json.RawMessage `json:"message"`
		Compaction *struct {
			Summary string `json:"summary"`
		} `json:"compaction"`
		Tool *struct {
			Invocation struct {
				Call struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"call"`
			} `json:"invocation"`
			Result *struct {
				Content json.RawMessage `json:"content"`
				IsError bool            `json:"is_error"`
			} `json:"result"`
		} `json:"tool"`
	}
	if json.Unmarshal(raw, &entry) != nil {
		return nil, core.ErrCorrupt
	}
	switch entry.Kind {
	case "context", "message":
		return entry.Message, nil
	case "compaction":
		if entry.Compaction == nil {
			return nil, core.ErrCorrupt
		}
		return json.Marshal(map[string]any{"role": "user", "context_kind": "summary", "content": []map[string]string{{"type": "text", "text": entry.Compaction.Summary}}})
	case "tool_result", "tool_resolution":
		if entry.Tool == nil || entry.Tool.Result == nil {
			return nil, core.ErrCorrupt
		}
		content := entry.Tool.Result.Content
		if len(content) == 0 || string(content) == "null" || string(content) == "[]" {
			content = json.RawMessage(`[{"type":"text"}]`)
		}
		message := map[string]any{"role": "tool", "content": content, "tool_call_id": entry.Tool.Invocation.Call.ID, "name": entry.Tool.Invocation.Call.Name}
		if entry.Tool.Result.IsError {
			message["is_error"] = true
		}
		return json.Marshal(message)
	}
	return nil, core.ErrCorrupt
}

func applyRequestChangeV11(raw json.RawMessage, path []string, change requestChangeV11) (json.RawMessage, error) {
	invalid := fmt.Errorf("%w: invalid format-11 request change", core.ErrCorrupt)
	if len(path) == 0 {
		if change.Op != "set" || !json.Valid(change.Value) {
			return nil, invalid
		}
		return change.Value, nil
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, invalid
	}
	key := path[0]
	if raw[0] == '{' {
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil {
			return nil, invalid
		}
		if len(path) == 1 && change.Op == "remove" {
			if _, ok := object[key]; !ok {
				return nil, invalid
			}
			delete(object, key)
		} else {
			value, err := applyRequestChangeV11(object[key], path[1:], change)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		return json.Marshal(object)
	}
	if raw[0] == '[' {
		var array []json.RawMessage
		index, err := strconv.Atoi(key)
		if err != nil || json.Unmarshal(raw, &array) != nil || index < 0 || index > len(array) {
			return nil, invalid
		}
		if len(path) == 1 && change.Op == "insert" {
			if !json.Valid(change.Value) {
				return nil, invalid
			}
			array = append(array, nil)
			copy(array[index+1:], array[index:])
			array[index] = change.Value
		} else if index == len(array) {
			return nil, invalid
		} else if len(path) == 1 && change.Op == "remove" {
			array = append(array[:index], array[index+1:]...)
		} else {
			value, err := applyRequestChangeV11(array[index], path[1:], change)
			if err != nil {
				return nil, err
			}
			array[index] = value
		}
		return json.Marshal(array)
	}
	return nil, invalid
}
