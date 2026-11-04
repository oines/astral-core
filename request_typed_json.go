package astral

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding"
	"encoding/json"
	"io"
	"strconv"
)

// writeTypedRequest preserves json.Marshal(Request), including RawMessage
// compaction and escaping, without an aggregate history-sized output buffer.
func writeTypedRequest(out io.Writer, request Request, reply *Message) error {
	w := requestDigestWriter{out: bufio.NewWriter(out)}
	w.typedRequestPrefix(request)
	if request.Messages == nil && reply == nil {
		w.literal("null")
	} else {
		w.literal("[")
		w.typedMessages(request.Messages)
		if reply != nil {
			if len(request.Messages) > 0 {
				w.literal(",")
			}
			w.typedMessage(*reply)
		}
		w.literal("]")
	}
	w.typedRequestSuffix(request)
	if w.err != nil {
		return w.err
	}
	return w.out.Flush()
}

func (w *requestDigestWriter) typedRequestPrefix(request Request) {
	o := w.object()
	o.field("model_id")
	w.typedValue(request.ModelID)
	if request.ModelRevision != "" {
		o.field("model_revision")
		w.typedValue(request.ModelRevision)
	}
	o.field("system_prompt")
	w.typedValue(request.SystemPrompt)
	o.field("messages")

}

func (w *requestDigestWriter) typedMessages(messages []Message) {
	for i, message := range messages {
		if i > 0 {
			w.literal(",")
		}
		w.typedMessage(message)
	}
}

func (w *requestDigestWriter) typedRequestSuffix(request Request) {
	o := requestDigestObject{w: w, comma: true}
	if len(request.Tools) > 0 {
		o.field("tools")
		w.typedValue(request.Tools)
	}
	o.field("max_output_tokens")
	w.literal(strconv.Itoa(request.MaxOutputTokens))
	o.field("options")
	w.typedValue(request.Options)
	w.literal("}")
}

func (w *requestDigestWriter) typedValue(value any) {
	if w.err != nil {
		return
	}
	if text, ok := value.(string); ok && w.simpleString(text) {
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		w.err = err
		return
	}
	w.bytes(raw)
}

func (w *requestDigestWriter) typedRaw(value json.RawMessage) {
	if simpleRequestJSONString(value) {
		w.bytes(value)
	} else {
		w.typedValue(value)
	}
}

func (w *requestDigestWriter) typedMessage(m Message) {
	o := w.object()
	o.field("role")
	w.typedValue(m.Role)
	if len(m.Content) > 0 {
		o.field("content")
		w.literal("[")
		for i, p := range m.Content {
			if i > 0 {
				w.literal(",")
			}
			w.typedContent(p)
		}
		w.literal("]")
	}
	if m.ToolCallID != "" {
		o.field("tool_call_id")
		w.typedValue(m.ToolCallID)
	}
	if m.Name != "" {
		o.field("name")
		w.typedValue(m.Name)
	}
	if m.IsError {
		o.field("is_error")
		w.literal("true")
	}
	if m.ContextKind != "" {
		o.field("context_kind")
		w.typedValue(m.ContextKind)
	}
	if len(m.Metadata) > 0 {
		o.field("metadata")
		w.typedRaw(m.Metadata)
	}
	if m.FinishReason != "" {
		o.field("finish_reason")
		w.typedValue(m.FinishReason)
	}
	if m.ErrorMessage != "" {
		o.field("error_message")
		w.typedValue(m.ErrorMessage)
	}
	w.literal("}")
}

func (w *requestDigestWriter) typedContent(p Content) {
	o := w.object()
	o.field("type")
	w.typedValue(p.Type)
	if p.Text != "" {
		o.field("text")
		w.typedValue(p.Text)
	}
	if p.MediaType != "" {
		o.field("media_type")
		w.typedValue(p.MediaType)
	}
	if p.URL != "" {
		o.field("url")
		w.typedValue(p.URL)
	}
	if len(p.Data) > 0 {
		o.field("data")
		w.typedRaw(p.Data)
	}
	if len(p.Metadata) > 0 {
		o.field("metadata")
		w.typedRaw(p.Metadata)
	}
	if p.ToolCall != nil {
		o.field("tool_call")
		w.typedValue(p.ToolCall)
	}
	w.literal("}")
}

type requestByteCounter int

func (c *requestByteCounter) Write(p []byte) (int, error) {
	*c += requestByteCounter(len(p))
	return len(p), nil
}

func typedRequestSize(request Request) (int, error) {
	var n requestByteCounter
	err := writeTypedRequest(&n, request, nil)
	return int(n), err
}

// requestFacts belongs to one immutable prepared request. It is never stored
// or shared between sessions. Only the small SHA state and JSON suffix are
// owned here; source borrows the prepared request for the validity check.
type requestFacts struct {
	size          int
	source        Request
	contextPrefix []byte
	contextSuffix []byte
	usageIndex    int
	usageDigest   string
}

// measurePreparedRequest counts the actual typed JSON and captures the
// provenance SHA prefix during that same traversal of the message body.
func measurePreparedRequest(request Request) (*requestFacts, error) {
	return measureRequestFacts(request, -1)
}

func measureRequestFacts(request Request, usageIndex int) (*requestFacts, error) {
	facts := &requestFacts{source: request, usageIndex: -1}
	if !requestStringsUTF8(request) {
		// GenerationProvenance normalizes these uncommon inputs before hashing.
		// Count their original JSON exactly, and leave provenance on that path.
		n, err := typedRequestSize(request)
		facts.size = n
		return facts, err
	}
	covered := request
	covered.MaxOutputTokens = 0
	var suffix bytes.Buffer
	tail := requestDigestWriter{out: bufio.NewWriter(&suffix)}
	tail.typedRequestSuffix(covered)
	if tail.err != nil {
		return nil, tail.err
	}
	if err := tail.out.Flush(); err != nil {
		return nil, err
	}
	facts.contextSuffix = suffix.Bytes()
	var size requestByteCounter
	hash := sha256.New()
	w := requestDigestWriter{out: bufio.NewWriter(io.MultiWriter(&size, hash))}
	w.typedRequestPrefix(covered)
	// Appending a reply always uses an array, even when the input was nil.
	w.literal("[")
	var usagePrefix []byte
	for i, message := range covered.Messages {
		if i > 0 {
			w.literal(",")
		}
		w.typedMessage(message)
		if i == usageIndex {
			if w.err != nil {
				return nil, w.err
			}
			if err := w.out.Flush(); err != nil {
				return nil, err
			}
			var err error
			usagePrefix, err = hash.(encoding.BinaryMarshaler).MarshalBinary()
			if err != nil {
				return nil, err
			}
		}
	}
	if w.err != nil {
		return nil, w.err
	}
	if err := w.out.Flush(); err != nil {
		return nil, err
	}
	var err error
	facts.contextPrefix, err = hash.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return nil, err
	}
	if len(usagePrefix) > 0 {
		facts.usageIndex = usageIndex
		facts.usageDigest, _ = finishContextDigest(usagePrefix, facts.contextSuffix, nil, false)
	}
	// The remaining bytes only affect size; provenance appends the reply here.
	w.out.Reset(&size)
	w.literal("]")
	w.bytes(facts.contextSuffix)
	if w.err != nil {
		return nil, w.err
	}
	if err := w.out.Flush(); err != nil {
		return nil, err
	}
	facts.size = int(size) + len(strconv.Itoa(request.MaxOutputTokens)) - 1
	if request.Messages == nil {
		facts.size += len("null") - len("[]")
	}
	return facts, nil
}

func (facts *requestFacts) sizeForOutput(output int) int {
	return facts.size + len(strconv.Itoa(output)) - len(strconv.Itoa(facts.source.MaxOutputTokens))
}
