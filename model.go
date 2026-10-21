package astral

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/oines/astral-core/internal/streamcheck"
)

// Model supplies protocol-neutral generation. Token estimation is a separate
// policy dependency in the session package.
// A Stream implementation must stop calling emit before returning. Opaque
// reasoning/signature/part metadata must round-trip through Request and Reply.
type Model interface {
	Spec() ModelSpec
	Stream(context.Context, Request, func(ModelEvent)) (Reply, error)
}

// RequestPreparer freezes host context before estimation, persistence and
// generation. Stream must use that snapshot, without resolving mutable files
// or appending live environment data. Preparation may run for inspection or
// budgeting without a subsequent model call and must have no external effects.
type RequestPreparer interface {
	PrepareRequest(context.Context, Request) (Request, error)
}

// PrepareRequest owns both sides of the optional preparation boundary.
func PrepareRequest(ctx context.Context, model Model, request Request) (out Request, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("model request preparation panicked: %v", p)
		}
	}()
	out, err = copyJSON(request)
	if err != nil {
		return Request{}, err
	}
	if preparer, ok := model.(RequestPreparer); ok {
		out, err = preparer.PrepareRequest(ctx, out)
		if err != nil {
			return Request{}, err
		}
	}
	if out.ModelID != request.ModelID || out.ModelRevision != request.ModelRevision ||
		out.MaxOutputTokens != request.MaxOutputTokens || !sameJSON(out.Tools, request.Tools) ||
		!sameJSON(out.Options, request.Options) {
		return Request{}, fmtInvalid("request preparation changed model configuration")
	}
	if err = out.Options.Validate(); err != nil {
		return Request{}, err
	}
	for _, message := range out.Messages {
		if err = validateMessage(message); err != nil {
			return Request{}, err
		}
	}
	return copyJSON(out)
}

// ModelRequestRecord and ModelResponseRecord are operational rollout facts.
// They are never added to the conversation or replayed as additional messages.
type ModelRequestRecord struct {
	AttemptID string  `json:"attempt_id"`
	Purpose   string  `json:"purpose"`
	Request   Request `json:"request"`
}

type ModelResponseRecord struct {
	AttemptID string `json:"attempt_id"`
	Purpose   string `json:"purpose"`
	Reply     Reply  `json:"reply"`
	Error     string `json:"error,omitempty"`
}

// ModelFunc adapts host functions without a provider-specific SDK dependency.
type ModelFunc struct {
	Specification ModelSpec
	Generate      func(context.Context, Request, func(ModelEvent)) (Reply, error)
}

type ModelSpec struct {
	ID              string `json:"id"`
	Revision        string `json:"revision,omitempty"`
	ContextWindow   int    `json:"context_window"`
	MaxInputTokens  int    `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens"`
}

type Request struct {
	ModelID         string            `json:"model_id"`
	ModelRevision   string            `json:"model_revision,omitempty"`
	SystemPrompt    string            `json:"system_prompt"`
	Messages        []Message         `json:"messages"`
	Tools           []ToolDefinition  `json:"tools,omitempty"`
	MaxOutputTokens int               `json:"max_output_tokens"`
	Options         GenerationOptions `json:"options,omitempty"`
}

type Reply struct {
	Message Message `json:"message"`
	Usage   Usage   `json:"usage,omitempty"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	// Available distinguishes a provider's reported zero usage from no report.
	Available         bool `json:"available,omitempty"`
	CachedInputTokens *int `json:"cached_input_tokens,omitempty"`
	CacheWriteTokens  *int `json:"cache_write_tokens,omitempty"`
	ReasoningTokens   *int `json:"reasoning_tokens,omitempty"`
}

type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishToolCalls FinishReason = "tool_calls"
	FinishLength    FinishReason = "length"
	FinishError     FinishReason = "error"
	FinishCancelled FinishReason = "cancelled"
)

// ModelEvent is an observation, never a partial durable message. Standard
// text_delta, reasoning_delta and tool_call_delta events require ContentIndex,
// an index in this attempt's final Reply.Message.Content. Text may contain an
// incomplete tool-argument JSON fragment; Data and Metadata remain opaque.
type ModelEvent struct {
	Type         string          `json:"type"`
	ContentIndex *int            `json:"content_index,omitempty"`
	Text         string          `json:"text,omitempty"`
	ToolCallID   string          `json:"tool_call_id,omitempty"`
	Data         json.RawMessage `json:"data,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
}

func (m ModelFunc) Spec() ModelSpec { return m.Specification }

func (m ModelFunc) Stream(ctx context.Context, request Request, emit func(ModelEvent)) (Reply, error) {
	if m.Generate == nil {
		return Reply{}, fmtInvalid("missing model stream implementation")
	}
	return m.Generate(ctx, request, emit)
}

func validateModelSpec(spec ModelSpec) error {
	if spec.ID == "" || spec.ContextWindow < 1 || spec.MaxOutputTokens < 1 || spec.MaxInputTokens < 0 {
		return fmtInvalid("model needs a stable ID and positive token limits")
	}
	return nil
}

func stream(ctx context.Context, model Model, request Request, emit func(ModelEvent)) (reply Reply, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("model stream panicked: %v", p)
		}
	}()
	var check streamcheck.Check
	var mu sync.Mutex
	reply, err = model.Stream(ctx, clone(request), func(event ModelEvent) {
		mu.Lock()
		defer mu.Unlock()
		owned, copyErr := copyJSON(event)
		if copyErr != nil {
			check.Fail(copyErr)
			return
		}
		if check.Observe(owned.Type, owned.ContentIndex, owned.ToolCallID) != nil {
			return
		}
		emit(owned)
	})
	if check.Err() != nil {
		err = errors.Join(err, fmtInvalid("model event: %v", check.Err()))
	}
	owned, copyErr := copyJSON(reply)
	if copyErr != nil {
		reply = Reply{}
		err = errors.Join(err, copyErr)
	} else {
		reply = owned
	}
	if err == nil && (reply.Message.FinishReason == FinishStop || reply.Message.FinishReason == FinishToolCalls || reply.Message.FinishReason == FinishLength) {
		if checkErr := check.Finish(len(reply.Message.Content), func(i int) (string, string) {
			part := reply.Message.Content[i]
			callID := ""
			if part.ToolCall != nil {
				callID = part.ToolCall.ID
			}
			return part.Type, callID
		}); checkErr != nil {
			err = fmtInvalid("model event: %v", checkErr)
		}
	}
	return
}

func validateReply(reply Reply) error {
	if reply.Message.FinishReason == FinishCancelled {
		return context.Canceled
	}
	if reply.Message.FinishReason != FinishStop && reply.Message.FinishReason != FinishToolCalls {
		return fmt.Errorf("%w: finish reason %q: %s", ErrIncompleteResponse, reply.Message.FinishReason, reply.Message.ErrorMessage)
	}
	if reply.Message.Role != RoleAssistant {
		return fmtInvalid("model reply must be an assistant message")
	}
	if reply.Message.FinishReason == FinishToolCalls && len(reply.Message.ToolCalls()) == 0 {
		return fmtInvalid("tool_calls finish reason without calls")
	}
	return validateMessage(reply.Message)
}
