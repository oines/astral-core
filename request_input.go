package astral

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// RequestInput pins an immutable branch boundary and the small differences
// between its normal projection and the actual model input. Conversation bodies
// remain in their history nodes. Selection and Changes also represent summary
// inputs and provider repairs without copying the unchanged conversation.
type RequestInput struct {
	RolloutID       string          `json:"rollout_id"`
	HeadID          string          `json:"head_id"`
	ConfigurationID string          `json:"configuration_id"`
	Model           ModelSpec       `json:"model"`
	Selection       *[]int          `json:"selection,omitempty"`
	Changes         []RequestChange `json:"changes,omitempty"`
	Digest          string          `json:"digest"`
}

// RequestChange is a structural edit of the projected Request JSON. Paths are
// individual object keys / array indices, not a provider-specific wire format.
// Untouched values, including images and opaque reasoning, are never repeated.
type RequestChange struct {
	Path  []string        `json:"path"`
	Op    string          `json:"op"`
	Value json.RawMessage `json:"value,omitempty"`
}

// FreezeRequest does no I/O. base must be the projection of the nodes being
// atomically committed with the attempt, or an existing immutable boundary.
func FreezeRequest(rolloutID string, base Context, request Request, spec ModelSpec) (RequestInput, error) {
	input := RequestInput{RolloutID: rolloutID, HeadID: base.HeadID, ConfigurationID: base.ConfigurationID, Model: spec}
	if rolloutID == "" || base.HeadID == "" || base.ConfigurationID == "" || request.ModelID != spec.ID {
		return input, fmtInvalid("request input needs an immutable context boundary and model")
	}
	selection := selectRequestMessages(base.Request.Messages, request.Messages)
	identity := len(selection) == len(base.Request.Messages)
	for i, source := range selection {
		identity = identity && source == i
	}
	projection := clone(base.Request)
	if !identity {
		input.Selection = &selection
		projection.Messages = selectedMessages(projection.Messages, selection)
	}
	before, err := json.Marshal(projection)
	if err != nil {
		return input, err
	}
	after, err := json.Marshal(request)
	if err != nil {
		return input, err
	}
	input.Changes = diffRequestJSON(nil, before, after)
	input.Digest = requestDigest(request)
	if input.Digest == "" {
		return input, ErrInvalid
	}
	return input, nil
}

func selectRequestMessages(base, actual []Message) []int {
	byDigest := make(map[string]int, len(base))
	parts := make([]map[string]int, len(base))
	for i, message := range base {
		byDigest[canonicalDigest(message)] = i
	}
	out := make([]int, len(actual))
	for i, message := range actual {
		if i < len(base) && sameJSON(base[i], message) {
			out[i] = i
			continue
		}
		if at, ok := byDigest[canonicalDigest(message)]; ok {
			out[i] = at
			continue
		}
		out[i] = -1
		// Repairs and summary selection may both shift a message and change it.
		// Prefer the same-role source retaining the most unchanged content bytes,
		// so removing reasoning never duplicates a surviving image or tool output.
		best := -1
		actualParts := make([]string, len(message.Content))
		for k, part := range message.Content {
			actualParts[k] = canonicalDigest(part)
		}
		for j, old := range base {
			if !sameMessageIdentity(old, message) {
				continue
			}
			if parts[j] == nil {
				parts[j] = make(map[string]int, len(old.Content))
				for _, part := range old.Content {
					raw, _ := json.Marshal(part)
					parts[j][canonicalDigest(part)] = len(raw)
				}
			}
			score := 0
			if i == j {
				score = 1
			}
			for _, digest := range actualParts {
				score += 2 * parts[j][digest]
			}
			if score > best {
				best, out[i] = score, j
			}
		}
	}
	return out
}

func sameMessageIdentity(a, b Message) bool {
	return a.Role == b.Role && a.ToolCallID == b.ToolCallID && a.Name == b.Name && a.ContextKind == b.ContextKind
}

func selectedMessages(base []Message, selection []int) []Message {
	out := make([]Message, len(selection))
	for i, at := range selection {
		if at >= 0 && at < len(base) {
			out[i] = base[at]
		}
	}
	return out
}

// RebuildRequest reads only the pinned active context window. It never invokes
// host preparation, environment collection, token estimation or model I/O.
func RebuildRequest(ctx context.Context, store Store, input RequestInput, maxBytes int) (Request, error) {
	base, err := ReadContext(ctx, store, input.RolloutID, input.HeadID, maxBytes)
	if err != nil {
		return Request{}, err
	}
	if base.ConfigurationID != input.ConfigurationID {
		return Request{}, fmt.Errorf("%w: request configuration boundary changed", ErrCorrupt)
	}
	return input.Apply(base.Request)
}

// Apply reconstructs the frozen semantic Request from its normal projection.
// Digest verification detects a damaged reference, recipe or projection.
func (input RequestInput) Apply(base Request) (Request, error) {
	if input.Selection != nil {
		for _, index := range *input.Selection {
			if index < -1 || index >= len(base.Messages) {
				return Request{}, fmt.Errorf("%w: invalid request message selection", ErrCorrupt)
			}
		}
		base.Messages = selectedMessages(base.Messages, *input.Selection)
	}
	raw, err := json.Marshal(base)
	if err != nil {
		return Request{}, err
	}
	for _, change := range input.Changes {
		raw, err = applyRequestChange(raw, change.Path, change)
		if err != nil {
			return Request{}, err
		}
	}
	var request Request
	if err = json.Unmarshal(raw, &request); err != nil {
		return Request{}, err
	}
	if request.ModelID != input.Model.ID || requestDigest(request) != input.Digest {
		return Request{}, fmt.Errorf("%w: frozen request digest mismatch", ErrCorrupt)
	}
	return request, nil
}

func requestDigest(request Request) string { return canonicalDigest(request) }

func canonicalDigest(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var canonical any
	if decoder.Decode(&canonical) != nil {
		return ""
	}
	raw, err = json.Marshal(canonical)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func jsonPath(parent []string, key string) []string {
	return append(append([]string(nil), parent...), key)
}

func equalRequestJSON(a, b json.RawMessage) bool {
	return bytes.Equal(a, b) || canonicalDigest(a) == canonicalDigest(b)
}

func diffRequestJSON(path []string, before, after json.RawMessage) []RequestChange {
	if equalRequestJSON(before, after) {
		return nil
	}
	var oldObject, newObject map[string]json.RawMessage
	if len(before) > 0 && before[0] == '{' && len(after) > 0 && after[0] == '{' && json.Unmarshal(before, &oldObject) == nil && json.Unmarshal(after, &newObject) == nil {
		keys := make([]string, 0, len(oldObject)+len(newObject))
		for key := range oldObject {
			keys = append(keys, key)
		}
		for key := range newObject {
			if _, exists := oldObject[key]; !exists {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		var changes []RequestChange
		for _, key := range keys {
			oldValue, oldExists := oldObject[key]
			newValue, newExists := newObject[key]
			p := jsonPath(path, key)
			if !newExists {
				changes = append(changes, RequestChange{Path: p, Op: "remove"})
			} else if !oldExists {
				changes = append(changes, RequestChange{Path: p, Op: "set", Value: newValue})
			} else {
				changes = append(changes, diffRequestJSON(p, oldValue, newValue)...)
			}
		}
		return changes
	}
	var oldArray, newArray []json.RawMessage
	if len(before) > 0 && before[0] == '[' && len(after) > 0 && after[0] == '[' && json.Unmarshal(before, &oldArray) == nil && json.Unmarshal(after, &newArray) == nil {
		var changes []RequestChange
		if len(oldArray) == len(newArray) {
			for i := range oldArray {
				changes = append(changes, diffRequestJSON(jsonPath(path, strconv.Itoa(i)), oldArray[i], newArray[i])...)
			}
			return changes
		}
		// Preserve identical prefixes/suffixes when a repair removes a reasoning
		// item. An unchanged image elsewhere in the content array stays a reference.
		prefix, suffix := 0, 0
		for prefix < min(len(oldArray), len(newArray)) && equalRequestJSON(oldArray[prefix], newArray[prefix]) {
			prefix++
		}
		for suffix < min(len(oldArray), len(newArray))-prefix && equalRequestJSON(oldArray[len(oldArray)-1-suffix], newArray[len(newArray)-1-suffix]) {
			suffix++
		}
		for i := len(oldArray) - suffix - 1; i >= prefix; i-- {
			changes = append(changes, RequestChange{Path: jsonPath(path, strconv.Itoa(i)), Op: "remove"})
		}
		for i := prefix; i < len(newArray)-suffix; i++ {
			changes = append(changes, RequestChange{Path: jsonPath(path, strconv.Itoa(i)), Op: "insert", Value: newArray[i]})
		}
		return changes
	}
	return []RequestChange{{Path: path, Op: "set", Value: after}}
}

func applyRequestChange(raw json.RawMessage, path []string, change RequestChange) (json.RawMessage, error) {
	invalid := fmt.Errorf("%w: invalid frozen request change", ErrCorrupt)
	if len(path) == 0 {
		if change.Op != "set" || !json.Valid(change.Value) {
			return nil, invalid
		}
		return change.Value, nil
	}
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
			if _, exists := object[key]; !exists {
				return nil, invalid
			}
			delete(object, key)
		} else {
			value, err := applyRequestChange(object[key], path[1:], change)
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
			value, err := applyRequestChange(array[index], path[1:], change)
			if err != nil {
				return nil, err
			}
			array[index] = value
		}
		return json.Marshal(array)
	}
	return nil, invalid
}
