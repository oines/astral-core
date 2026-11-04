package astral

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strconv"
	"unicode/utf8"
)

// canonicalRequestDigest preserves canonicalDigest's JSON representation while
// streaming the message body into SHA-256 instead of materializing it twice.
func canonicalRequestDigest(request Request) string {
	hash := sha256.New()
	if err := writeCanonicalRequest(hash, request); err != nil {
		return ""
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writeCanonicalRequest(out io.Writer, request Request) error {
	w := requestDigestWriter{out: bufio.NewWriter(out)}
	object := w.object()
	object.field("max_output_tokens")
	w.literal(strconv.Itoa(request.MaxOutputTokens))
	object.field("messages")
	if request.Messages == nil {
		w.literal("null")
	} else {
		w.literal("[")
		for i, message := range request.Messages {
			if i > 0 {
				w.literal(",")
			}
			w.message(message)
		}
		w.literal("]")
	}
	object.field("model_id")
	w.string(request.ModelID)
	if request.ModelRevision != "" {
		object.field("model_revision")
		w.string(request.ModelRevision)
	}
	object.field("options") // A struct is not omitted by encoding/json.
	w.canonical(request.Options)
	object.field("system_prompt")
	w.string(request.SystemPrompt)
	if len(request.Tools) > 0 {
		object.field("tools")
		w.canonical(request.Tools)
	}
	w.literal("}")
	if w.err != nil {
		return w.err
	}
	return w.out.Flush()
}

type requestDigestWriter struct {
	out     *bufio.Writer
	encoder *json.Encoder
	err     error
}

func (w *requestDigestWriter) literal(value string) {
	if w.err == nil {
		_, w.err = w.out.WriteString(value)
	}
}

func (w *requestDigestWriter) bytes(value []byte) {
	if w.err == nil {
		_, w.err = w.out.Write(value)
	}
}

func (w *requestDigestWriter) string(value string) {
	if w.simpleString(value) {
		return
	}
	if !utf8.ValidString(value) {
		// The old first encoding replaces invalid bytes with escaped U+FFFD;
		// decoding and re-encoding produces the literal valid replacement rune.
		w.canonical(value)
		return
	}
	w.encode(value)
}

// encode lets the standard encoder write directly into this traversal's output
// without json.Marshal's additional owned result slice. Encode appends one LF;
// the adapter consumes only that terminator, retaining every JSON content byte.
func (w *requestDigestWriter) encode(value any) {
	if w.err != nil {
		return
	}
	if w.encoder == nil {
		w.encoder = json.NewEncoder(requestJSONWriter{out: w.out})
	}
	w.err = w.encoder.Encode(value)
}

type requestJSONWriter struct{ out io.Writer }

func (w requestJSONWriter) Write(p []byte) (int, error) {
	size := len(p)
	if size > 0 && p[size-1] == '\n' {
		p = p[:size-1]
	}
	n, err := w.out.Write(p)
	if n == len(p) {
		return size, err
	}
	if err == nil {
		err = io.ErrShortWrite
	}
	return n, err
}

// Plain ASCII has the same bytes in typed and canonical JSON. Read it directly
// from its immutable string instead of allocating an encoded copy to hash it.
func (w *requestDigestWriter) simpleString(value string) bool {
	if !simpleRequestStringContent(value) {
		return false
	}
	w.literal(`"`)
	w.literal(value)
	w.literal(`"`)
	return true
}

// canonical is reserved for the small configuration objects and opaque JSON
// that needs normalization. UseNumber and the second marshal retain the old
// duplicate-key, number spelling, HTML escaping and map-key ordering rules.
func (w *requestDigestWriter) canonical(value any) {
	if w.err != nil {
		return
	}
	raw, err := json.Marshal(value)
	if err == nil {
		var decoded any
		decoded, err = decodeJSON(raw)
		if err == nil {
			raw, err = json.Marshal(decoded)
		}
	}
	if err != nil {
		w.err = err
		return
	}
	w.bytes(raw)
}

func (w *requestDigestWriter) raw(value json.RawMessage) {
	if simpleRequestJSONString(value) {
		w.bytes(value)
	} else {
		w.canonical(value)
	}
}

type requestDigestObject struct {
	w     *requestDigestWriter
	comma bool
}

func (w *requestDigestWriter) object() requestDigestObject {
	w.literal("{")
	return requestDigestObject{w: w}
}

// Callers supply the same lexicographic field order as canonical map encoding.
func (o *requestDigestObject) field(key string) {
	if o.comma {
		o.w.literal(",")
	}
	o.comma = true
	o.w.literal(`"`)
	o.w.literal(key)
	o.w.literal(`":`)
}

func (w *requestDigestWriter) message(message Message) {
	object := w.object()
	if len(message.Content) > 0 {
		object.field("content")
		w.literal("[")
		for i, part := range message.Content {
			if i > 0 {
				w.literal(",")
			}
			w.content(part)
		}
		w.literal("]")
	}
	if message.ContextKind != "" {
		object.field("context_kind")
		w.string(message.ContextKind)
	}
	if message.ErrorMessage != "" {
		object.field("error_message")
		w.string(message.ErrorMessage)
	}
	if message.FinishReason != "" {
		object.field("finish_reason")
		w.string(string(message.FinishReason))
	}
	if message.IsError {
		object.field("is_error")
		w.literal("true")
	}
	if len(message.Metadata) > 0 {
		object.field("metadata")
		w.raw(message.Metadata)
	}
	if message.Name != "" {
		object.field("name")
		w.string(message.Name)
	}
	object.field("role")
	w.string(string(message.Role))
	if message.ToolCallID != "" {
		object.field("tool_call_id")
		w.string(message.ToolCallID)
	}
	w.literal("}")
}

func (w *requestDigestWriter) content(part Content) {
	object := w.object()
	if len(part.Data) > 0 {
		object.field("data")
		w.raw(part.Data)
	}
	if part.MediaType != "" {
		object.field("media_type")
		w.string(part.MediaType)
	}
	if len(part.Metadata) > 0 {
		object.field("metadata")
		w.raw(part.Metadata)
	}
	if part.Text != "" {
		object.field("text")
		w.string(part.Text)
	}
	if part.ToolCall != nil {
		object.field("tool_call")
		w.toolCall(*part.ToolCall)
	}
	object.field("type")
	w.string(part.Type)
	if part.URL != "" {
		object.field("url")
		w.string(part.URL)
	}
	w.literal("}")
}

func (w *requestDigestWriter) toolCall(call ToolCall) {
	object := w.object()
	object.field("arguments")
	w.raw(call.Arguments)
	object.field("id")
	w.string(call.ID)
	if len(call.Metadata) > 0 {
		object.field("metadata")
		w.raw(call.Metadata)
	}
	object.field("name")
	w.string(call.Name)
	w.literal("}")
}
