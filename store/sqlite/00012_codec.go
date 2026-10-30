package sqlite

// Schema 12's wire codec is also used by its frozen migration. Keep this layout
// and field grammar immutable; a future storage layout gets its own codec.
// It walks declared Core fields, never the contents of opaque provider JSON.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	core "github.com/oines/astral-core"
)

const inlineBytes12 = 4096

type field12 struct {
	leaf   byte // s: UTF-8 string; j: opaque JSON bytes
	fields map[string]*field12
	items  *field12
	values *field12
}

func object12(fields map[string]*field12) *field12 { return &field12{fields: fields} }
func array12(item *field12) *field12               { return &field12{items: item} }
func map12(value *field12) *field12                { return &field12{values: value} }

var text12 = &field12{leaf: 's'}
var json12 = &field12{leaf: 'j'}
var call12 = object12(map[string]*field12{"arguments": json12, "metadata": json12})
var content12 = object12(map[string]*field12{"text": text12, "url": text12, "data": json12, "metadata": json12, "tool_call": call12})
var message12 = object12(map[string]*field12{"content": array12(content12), "metadata": json12, "error_message": text12})
var tool12 = object12(map[string]*field12{
	"invocation": object12(map[string]*field12{"call": call12, "binding": json12}),
	"result":     object12(map[string]*field12{"content": array12(content12), "details": json12, "artifact": text12}),
	"decision":   object12(map[string]*field12{"reason": text12}),
	"control":    object12(map[string]*field12{"reason": text12}),
})
var approval12 = object12(map[string]*field12{
	"request":  object12(map[string]*field12{"prompt": text12, "details": json12}),
	"decision": object12(map[string]*field12{"reason": text12}),
})
var projection12 = object12(map[string]*field12{"pending_calls": map12(call12), "unresolved": map12(tool12)})
var creation12 = object12(map[string]*field12{"projection": projection12})
var run12 = object12(map[string]*field12{"error": text12, "end_reason": text12})
var options12 = object12(map[string]*field12{
	"stop": array12(text12), "provider_options": map12(json12),
	"response_format": object12(map[string]*field12{"schema": json12}),
})
var entry12 = object12(map[string]*field12{
	"created": creation12,
	"configuration": object12(map[string]*field12{
		"system_prompt": text12,
		"selection":     object12(map[string]*field12{"options": options12}),
		"tools":         array12(object12(map[string]*field12{"description": text12, "parameters": json12})),
	}),
	"message": message12, "tool": tool12, "approval": approval12,
	"input":      object12(map[string]*field12{"message": message12, "contexts": array12(message12)}),
	"compaction": object12(map[string]*field12{"summary": text12, "instructions": text12, "options": options12}),
	"run":        run12,
})
var state12 = object12(map[string]*field12{
	"creation": creation12, "branch": projection12, "approvals": map12(approval12), "last_run": run12,
	"tools": map12(object12(map[string]*field12{
		"prepared": entry12, "authorized": entry12, "started": entry12, "completed": entry12,
		"result": entry12, "control": entry12, "unresolved": entry12,
	})),
})
var attempt12 = object12(map[string]*field12{
	"input":   object12(map[string]*field12{"snapshot": json12, "changes": array12(object12(map[string]*field12{"value": json12}))}),
	"outcome": object12(map[string]*field12{"error": text12, "reply": object12(map[string]*field12{"message": message12})}),
})

type reference12 struct {
	Path []string `json:"path"`
	Hash string   `json:"hash"`
}
type packed12 struct {
	Body json.RawMessage `json:"body"`
	Refs []reference12   `json:"refs,omitempty"`
}

func hash12(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func path12(path []string) string { b, _ := json.Marshal(path); return string(b) }

// walk12 preserves unknown envelope fields and treats declared JSON leaves as
// indivisible bytes. Map keys (execution IDs) are path components, not syntax.
func walk12(raw json.RawMessage, shape *field12, path []string, leaf func([]string, byte, json.RawMessage) (json.RawMessage, error)) (json.RawMessage, error) {
	if shape.leaf != 0 {
		return leaf(path, shape.leaf, raw)
	}
	if string(raw) == "null" {
		return raw, nil
	}
	if shape.items != nil {
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
		for i := range list {
			value, err := walk12(list[i], shape.items, append(path, strconv.Itoa(i)), leaf)
			if err != nil {
				return nil, err
			}
			list[i] = value
		}
		return json.Marshal(list)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		child := shape.fields[key]
		if shape.values != nil {
			child = shape.values
		}
		if child == nil {
			continue
		}
		value, err := walk12(fields[key], child, append(path, key), leaf)
		if err != nil {
			return nil, err
		}
		fields[key] = value
	}
	return json.Marshal(fields)
}

func encode12(raw []byte, shape *field12) ([]byte, map[string][]byte, error) {
	pack := packed12{}
	blobs := map[string][]byte{}
	if len(raw) < inlineBytes12 {
		encoded, err := json.Marshal(packed12{Body: raw})
		return encoded, blobs, err
	}
	body, err := walk12(raw, shape, nil, func(path []string, kind byte, value json.RawMessage) (json.RawMessage, error) {
		if len(value) < inlineBytes12 {
			return value, nil
		}
		bytes := []byte(value)
		if kind == 's' {
			var text string
			if err := json.Unmarshal(value, &text); err != nil {
				return nil, err
			}
			bytes = []byte(text)
		}
		if len(bytes) < inlineBytes12 {
			return value, nil
		}
		hash := hash12(bytes)
		blobs[hash] = bytes
		pack.Refs = append(pack.Refs, reference12{Path: append([]string(nil), path...), Hash: hash})
		return json.RawMessage("null"), nil
	})
	if err != nil {
		return nil, nil, err
	}
	pack.Body = body
	if len(pack.Refs) == 0 {
		pack.Body = raw
	}
	encoded, err := json.Marshal(pack)
	return encoded, blobs, err
}

func decode12(pack packed12, shape *field12, blobs map[string][]byte) ([]byte, error) {
	if len(pack.Refs) == 0 {
		return pack.Body, nil
	}
	refs := make(map[string]string, len(pack.Refs))
	for _, ref := range pack.Refs {
		key := path12(ref.Path)
		if len(ref.Hash) != 64 || len(ref.Path) == 0 || refs[key] != "" {
			return nil, core.ErrCorrupt
		}
		refs[key] = ref.Hash
	}
	decoded, err := walk12(pack.Body, shape, nil, func(path []string, kind byte, value json.RawMessage) (json.RawMessage, error) {
		key := path12(path)
		hash := refs[key]
		if hash == "" {
			return value, nil
		}
		delete(refs, key)
		bytes, ok := blobs[hash]
		if !ok || string(value) != "null" {
			return nil, core.ErrCorrupt
		}
		if kind == 's' {
			return json.Marshal(string(bytes))
		}
		if !json.Valid(bytes) {
			return nil, core.ErrCorrupt
		}
		return json.RawMessage(bytes), nil
	})
	if err != nil || len(refs) != 0 {
		return nil, fmt.Errorf("%w: invalid payload envelope", core.ErrCorrupt)
	}
	return decoded, nil
}
