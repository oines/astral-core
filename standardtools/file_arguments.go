package standardtools

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// File tools follow Pi's permissive object schemas. Unlike the command tools,
// unrelated properties are allowed, but missing or mistyped required fields are
// still rejected for callers that invoke Execute without Core's schema check.
func decodeFileArguments(data json.RawMessage, target any, required ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return notExecuted("invalid arguments: expected an object")
	}
	for _, name := range required {
		var value string
		raw, ok := fields[name]
		if !ok || string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
			return notExecuted(fmt.Sprintf("invalid arguments: %s must be a string", name))
		}
	}
	if err := json.Unmarshal(data, target); err != nil {
		return notExecuted("invalid arguments: " + err.Error())
	}
	return nil
}

func singleEdit(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return false
	}
	return singleEditFields(fields)
}

func singleEditFields(fields map[string]json.RawMessage) bool {
	for _, name := range []string{"oldText", "newText"} {
		var value string
		if string(fields[name]) == "null" || json.Unmarshal(fields[name], &value) != nil {
			return false
		}
	}
	return true
}

func editArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	raw = bytes.TrimSpace(raw)
	var list []json.RawMessage
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &list) != nil {
		return nil, false
	}
	return list, true
}

// Compatibility preparation only needs the top-level value kinds. Inspecting
// them through encoding/json validates syntax and preserves last-key-wins
// behavior without copying large oldText/newText strings or edits arrays.
type argumentKind byte

func (k *argumentKind) UnmarshalJSON(raw []byte) error {
	*k = argumentKind(raw[0])
	return nil
}

func prepareEditArguments(input json.RawMessage) (json.RawMessage, error) {
	// Use exact map keys: struct decoding would also accept "EDITS", unlike Pi.
	var shape map[string]argumentKind
	if err := json.Unmarshal(input, &shape); err != nil {
		return nil, notExecuted("invalid arguments: " + err.Error())
	}
	if shape["edits"] == '[' && (shape["oldText"] != '"' || shape["newText"] != '"') {
		// Already canonical: Core's schema check (or direct Execute's decoder)
		// validates the elements. No compatibility conversion or ownership copy
		// is needed here; callers retain their existing ownership boundaries.
		return input, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input, &fields); err != nil {
		return nil, notExecuted("invalid arguments: " + err.Error())
	}
	if fields == nil {
		return append(json.RawMessage(nil), input...), nil
	}
	edits := fields["edits"]
	var encoded string
	if json.Unmarshal(edits, &encoded) == nil {
		parsed := json.RawMessage(encoded)
		_, array := editArray(parsed)
		if array || singleEdit(parsed) {
			edits = parsed
		}
	}
	if singleEdit(edits) {
		edits = append(append(json.RawMessage{'['}, edits...), ']')
	}
	if singleEditFields(fields) {
		// Pi gives a valid legacy pair priority over a malformed edits value.
		list, _ := editArray(edits)
		legacy, _ := json.Marshal(map[string]json.RawMessage{"oldText": fields["oldText"], "newText": fields["newText"]})
		list = append(list, legacy)
		edits, _ = json.Marshal(list)
		delete(fields, "oldText")
		delete(fields, "newText")
	}
	if edits != nil {
		fields["edits"] = edits
	}
	return json.Marshal(fields)
}
