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
	"unicode/utf8"
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
	Snapshot        json.RawMessage `json:"snapshot,omitempty"`
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
	return freezeRequest(rolloutID, base, request, spec, nil)
}

func freezeRequest(rolloutID string, base Context, request Request, spec ModelSpec, facts *requestFacts) (RequestInput, error) {
	input := RequestInput{RolloutID: rolloutID, HeadID: base.HeadID, ConfigurationID: base.ConfigurationID, Model: spec}
	if rolloutID == "" || base.HeadID == "" || base.ConfigurationID == "" || request.ModelID != spec.ID {
		return input, fmtInvalid("request input needs an immutable context boundary and model")
	}
	selection := selectRequestMessages(base.Request.Messages, request.Messages)
	identity := len(selection) == len(base.Request.Messages)
	for i, source := range selection {
		identity = identity && source == i
	}
	// Projection is borrowed for comparison only. No value in the immutable
	// branch is changed while recording this request's small recipe.
	projection := base.Request
	if !identity {
		input.Selection = &selection
		projection.Messages = selectedMessages(projection.Messages, selection)
	}
	var err error
	input.Changes, err = diffPreparedRequest(projection, request)
	if err != nil {
		return input, err
	}
	input.Digest = facts.frozenDigest(request)
	if input.Digest == "" {
		input.Digest = requestDigest(request)
	}
	if input.Digest == "" {
		return input, ErrInvalid
	}
	return input, nil
}

func selectRequestMessages(base, actual []Message) []int {
	var byDigest map[string]int
	parts := make([]map[string]int, len(base))
	out := make([]int, len(actual))
	for i, message := range actual {
		if i < len(base) && sameRequestMessage(base[i], message) {
			out[i] = i
			continue
		}
		// Most projections change content in place. Prefer that source before
		// building an index over the entire history, provided this cannot copy a
		// large field moved from a different message into the request recipe.
		if len(base) == len(actual) && sameMessageIdentity(base[i], message) && retainsLargeRequestFields(base[i], message) {
			out[i] = i
			continue
		}
		if byDigest == nil {
			byDigest = make(map[string]int, len(base))
			for j, old := range base {
				byDigest[canonicalDigest(old)] = j
			}
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

func retainsLargeRequestFields(before, after Message) bool {
	const large = 4096
	if len(after.Metadata) >= large && !bytes.Equal(before.Metadata, after.Metadata) ||
		len(after.ErrorMessage) >= large && before.ErrorMessage != after.ErrorMessage {
		return false
	}
	for _, part := range after.Content {
		retained := false
		for _, old := range before.Content {
			if len(part.Text) >= large && old.Text != part.Text ||
				len(part.URL) >= large && old.URL != part.URL ||
				len(part.Data) >= large && !bytes.Equal(old.Data, part.Data) ||
				len(part.Metadata) >= large && !bytes.Equal(old.Metadata, part.Metadata) {
				continue
			}
			if call := part.ToolCall; call != nil && (len(call.Arguments) >= large || len(call.Metadata) >= large) {
				if old.ToolCall == nil || len(call.Arguments) >= large && !bytes.Equal(old.ToolCall.Arguments, call.Arguments) ||
					len(call.Metadata) >= large && !bytes.Equal(old.ToolCall.Metadata, call.Metadata) {
					continue
				}
			}
			retained = true
			break
		}
		if !retained && (len(part.Text) >= large || len(part.URL) >= large || len(part.Data) >= large || len(part.Metadata) >= large ||
			part.ToolCall != nil && (len(part.ToolCall.Arguments) >= large || len(part.ToolCall.Metadata) >= large)) {
			return false
		}
	}
	return true
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
	raw, err = applyRequestChanges(raw, input.Changes)
	if err != nil {
		return Request{}, err
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

func requestDigest(request Request) string { return canonicalRequestDigest(request) }

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

// requestJSON expands only containers reached by a patch. Untouched leaves stay
// raw: opaque JSON keeps its number spelling and duplicate fields, and large
// images are not decoded and re-encoded for every sibling change.
type requestJSON struct {
	raw    json.RawMessage
	object map[string]*requestJSON
	array  []*requestJSON
}

func applyRequestChanges(raw json.RawMessage, changes []RequestChange) (json.RawMessage, error) {
	for _, change := range changes {
		for _, key := range change.Path {
			if utf8.ValidString(key) {
				continue
			}
			// Direct Go callers can supply invalid UTF-8 map keys. The old
			// per-change JSON round trip normalized them before the next
			// operation, possibly merging them with an existing U+FFFD key.
			// Persisted recipes already have valid keys; keep their batch path.
			var err error
			for _, item := range changes {
				raw, err = applyRequestChange(raw, item.Path, item)
				if err != nil {
					return nil, err
				}
			}
			return raw, nil
		}
	}
	return applyRequestChangeBatch(raw, changes)
}

func applyRequestChangeBatch(raw json.RawMessage, changes []RequestChange) (json.RawMessage, error) {
	if len(changes) == 0 {
		return raw, nil
	}
	root := &requestJSON{raw: raw}
	for _, change := range changes {
		if err := root.apply(change.Path, change); err != nil {
			return nil, err
		}
	}
	if root.raw != nil {
		return bytes.Clone(root.raw), nil
	}
	var output bytes.Buffer
	if err := root.write(&output); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func applyRequestChange(raw json.RawMessage, path []string, change RequestChange) (json.RawMessage, error) {
	change.Path = path
	return applyRequestChangeBatch(raw, []RequestChange{change})
}

func (n *requestJSON) expand() error {
	invalid := fmt.Errorf("%w: invalid frozen request change", ErrCorrupt)
	if n.object != nil || n.array != nil {
		return nil
	}
	if len(n.raw) == 0 {
		return invalid
	}
	switch n.raw[0] {
	case '{':
		var object map[string]json.RawMessage
		if json.Unmarshal(n.raw, &object) != nil {
			return invalid
		}
		n.object = make(map[string]*requestJSON, len(object))
		for key, value := range object {
			n.object[key] = &requestJSON{raw: value}
		}
	case '[':
		var array []json.RawMessage
		if json.Unmarshal(n.raw, &array) != nil {
			return invalid
		}
		n.array = make([]*requestJSON, len(array))
		for index, value := range array {
			n.array[index] = &requestJSON{raw: value}
		}
	default:
		return invalid
	}
	n.raw = nil
	return nil
}

func (n *requestJSON) apply(path []string, change RequestChange) error {
	invalid := fmt.Errorf("%w: invalid frozen request change", ErrCorrupt)
	if len(path) == 0 {
		if change.Op != "set" || !json.Valid(change.Value) {
			return invalid
		}
		*n = requestJSON{raw: change.Value}
		return nil
	}
	if err := n.expand(); err != nil {
		return err
	}
	key := path[0]
	if n.object != nil {
		if len(path) == 1 && change.Op == "remove" {
			if _, exists := n.object[key]; !exists {
				return invalid
			}
			delete(n.object, key)
			return nil
		}
		child := n.object[key]
		if child == nil {
			child = &requestJSON{}
		}
		if err := child.apply(path[1:], change); err != nil {
			return err
		}
		// The old per-change parent marshal compacted child values before the
		// next patch. Retain that path behavior without serializing the parent.
		child.raw = bytes.TrimSpace(child.raw)
		n.object[key] = child
		return nil
	}
	index, err := strconv.Atoi(key)
	if err != nil || index < 0 || index > len(n.array) {
		return invalid
	}
	if len(path) == 1 && change.Op == "insert" {
		if !json.Valid(change.Value) {
			return invalid
		}
		n.array = append(n.array, nil)
		copy(n.array[index+1:], n.array[index:])
		n.array[index] = &requestJSON{raw: bytes.TrimSpace(change.Value)}
	} else if index == len(n.array) {
		return invalid
	} else if len(path) == 1 && change.Op == "remove" {
		copy(n.array[index:], n.array[index+1:])
		n.array[len(n.array)-1] = nil
		n.array = n.array[:len(n.array)-1]
	} else {
		child := n.array[index]
		if err := child.apply(path[1:], change); err != nil {
			return err
		}
		child.raw = bytes.TrimSpace(child.raw)
	}
	return nil
}

// Serialize expanded containers once. Marshal each raw leaf with the standard
// encoder so its compaction and HTML escaping match the old RawMessage path,
// without repeatedly copying the whole request at each ancestor.
func (n *requestJSON) write(output *bytes.Buffer) error {
	if n.raw != nil {
		raw, err := json.Marshal(n.raw)
		if err != nil {
			return err
		}
		output.Write(raw)
		return nil
	}
	if n.object != nil {
		keys := make([]string, 0, len(n.object))
		for key := range n.object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		output.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				output.WriteByte(',')
			}
			encoded, _ := json.Marshal(key)
			output.Write(encoded)
			output.WriteByte(':')
			if err := n.object[key].write(output); err != nil {
				return err
			}
		}
		output.WriteByte('}')
		return nil
	}
	output.WriteByte('[')
	for index, child := range n.array {
		if index > 0 {
			output.WriteByte(',')
		}
		if err := child.write(output); err != nil {
			return err
		}
	}
	output.WriteByte(']')
	return nil
}
