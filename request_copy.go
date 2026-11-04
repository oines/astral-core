package astral

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/oines/astral-core/internal/base64ascii"
)

// CopyRequest returns a JSON-compatible, independently owned request without
// encoding and decoding its full message history. It preserves the same wire
// normalization and JSON errors as a JSON round trip. It does not validate the
// model's semantic request contract; PrepareRequest and adapters own that step.
func CopyRequest(request Request) (Request, error) {
	if !utf8.ValidString(request.ModelID) || !utf8.ValidString(request.ModelRevision) || !utf8.ValidString(request.SystemPrompt) {
		return copyJSON(request)
	}
	out := request
	var err error
	if out.Messages, err = copyRequestMessages(request.Messages); err != nil {
		return Request{}, err
	}
	if len(request.Tools) == 0 {
		out.Tools = nil
	} else if out.Tools, err = copyJSON(request.Tools); err != nil {
		return Request{}, err
	}
	// These small configuration objects contain numeric pointers and raw JSON
	// maps. Their established encoder also preserves non-finite-number errors,
	// invalid UTF-8 map-key normalization, and required nil JSON values.
	if out.Options, err = copyJSON(request.Options); err != nil {
		return Request{}, err
	}
	return out, nil
}

func copyRequestMessages(messages []Message) ([]Message, error) {
	// Keep future custom Message encoders on their canonical JSON path. The
	// current decoder only rejects the removed tool_calls field, which cannot
	// be represented by a typed Message value.
	if _, ok := any(Message{}).(json.Marshaler); ok {
		return copyJSON(messages)
	}
	if _, ok := any(&Message{}).(json.Marshaler); ok {
		return copyJSON(messages)
	}
	var out []Message
	if messages != nil {
		out = make([]Message, len(messages))
	}
	var err error
	for i, message := range messages {
		if !requestMessageUTF8(message) {
			return copyJSON(messages)
		}
		owned := message
		owned.Content = nil
		if len(message.Content) != 0 {
			owned.Content = make([]Content, len(message.Content))
		}
		for j, part := range message.Content {
			item := part
			if item.Data, err = copyOptionalRequestJSON(part.Data); err != nil {
				return nil, err
			}
			if item.Metadata, err = copyOptionalRequestJSON(part.Metadata); err != nil {
				return nil, err
			}
			if part.ToolCall != nil {
				if item.ToolCall, err = copyJSON(part.ToolCall); err != nil {
					return nil, err
				}
			}
			owned.Content[j] = item
		}
		if owned.Metadata, err = copyOptionalRequestJSON(message.Metadata); err != nil {
			return nil, err
		}
		out[i] = owned
	}
	return out, nil
}

func requestMessageUTF8(message Message) bool {
	for _, value := range []string{string(message.Role), message.ToolCallID, message.Name, message.ContextKind, string(message.FinishReason), message.ErrorMessage} {
		if !utf8.ValidString(value) {
			return false
		}
	}
	for _, part := range message.Content {
		for _, value := range []string{part.Type, part.Text, part.MediaType, part.URL} {
			if !utf8.ValidString(value) {
				return false
			}
		}
	}
	return true
}

func copyOptionalRequestJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// Frozen image pixels commonly arrive as a large, already encoded base64
	// string. This restricted shape is valid JSON and Marshal cannot normalize
	// any byte in it. Avoid running the JSON state machine over every pixel on
	// each budget check; every other shape retains the canonical encoder.
	if simpleRequestJSONString(raw) {
		return bytes.Clone(raw), nil
	}
	// Marshal validates, compacts and HTML-escapes opaque JSON, just as it does
	// inside Request.Marshal. Unmarshalling RawMessage would only copy it again.
	return json.Marshal(raw)
}

func simpleRequestJSONString(raw []byte) bool {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return false
	}
	// A valid Base64 alphabet is also plain ASCII JSON with no escaping.
	// Reuse the same SIMD recognition as image validation for large payloads.
	if len(raw) >= 1024 && base64ascii.Valid(raw[1:len(raw)-1]) {
		return true
	}
	return simpleRequestStringContent(raw[1 : len(raw)-1])
}

func simpleRequestStringContent[T ~string | ~[]byte](raw T) bool {
	// Check eight ASCII bytes at a time. Any control, non-ASCII, JSON escape
	// or HTML-sensitive byte keeps the established encoder fallback below.
	const high = uint64(0x8080808080808080)
	const low = uint64(0x0101010101010101)
	for len(raw) >= 8 {
		x := binary.LittleEndian.Uint64([]byte(raw[:8]))
		if x&high != 0 || (x-0x2020202020202020)&^x&high != 0 {
			return false
		}
		quote, slash := x^0x2222222222222222, x^0x5c5c5c5c5c5c5c5c
		lt, gt, amp := x^0x3c3c3c3c3c3c3c3c, x^0x3e3e3e3e3e3e3e3e, x^0x2626262626262626
		if ((quote-low)&^quote|(slash-low)&^slash|(lt-low)&^lt|(gt-low)&^gt|(amp-low)&^amp)&high != 0 {
			return false
		}
		raw = raw[8:]
	}
	for i := 0; i < len(raw); i++ {
		b := raw[i]
		if b < 0x20 || b >= 0x80 || b == '"' || b == '\\' || b == '<' || b == '>' || b == '&' {
			return false
		}
	}
	return true
}

// As with clone, this helper is for already validated SDK-owned requests.
func cloneRequest(request Request) Request {
	out, err := CopyRequest(request)
	if err != nil {
		panic(fmt.Sprintf("astral: invalid internal JSON: %v", err))
	}
	return out
}
