package astral

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/oines/astral-core/internal/base64ascii"
	"github.com/oines/astral-core/internal/jsonstring"
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
	return jsonstring.UnescapedASCII(raw)
}

// As with clone, this helper is for already validated SDK-owned requests.
func cloneRequest(request Request) Request {
	out, err := CopyRequest(request)
	if err != nil {
		panic(fmt.Sprintf("astral: invalid internal JSON: %v", err))
	}
	return out
}
