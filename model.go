package astral

import (
	"context"
	"encoding/json"
	"fmt"
)

// Model must count the complete request input, including system prompt, tools
// and provider protocol overhead. Output reservation is accounted for by Core.
// A Stream implementation must stop calling emit before returning. Opaque
// reasoning/signature/part metadata must round-trip through Request and Reply.
type Model interface {
	Spec() ModelSpec
	EstimateTokens(context.Context, Request) (int, error)
	Stream(context.Context, Request, func(ModelEvent)) (Reply, error)
}

// ModelFunc adapts host functions without a provider-specific SDK dependency.
type ModelFunc struct {
	Specification ModelSpec
	Estimate      func(context.Context, Request) (int, error)
	Generate      func(context.Context, Request, func(ModelEvent)) (Reply, error)
}

type ModelSpec struct {
	ID              string `json:"id"`
	ContextWindow   int    `json:"context_window"`
	MaxOutputTokens int    `json:"max_output_tokens"`
}

type Request struct {
	ModelID         string           `json:"model_id"`
	SystemPrompt    string           `json:"system_prompt"`
	Messages        []Message        `json:"messages"`
	Tools           []ToolDefinition `json:"tools,omitempty"`
	MaxOutputTokens int              `json:"max_output_tokens"`
}

type Reply struct {
	Message      Message      `json:"message"`
	FinishReason FinishReason `json:"finish_reason"`
	Usage        Usage        `json:"usage,omitempty"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishToolCalls FinishReason = "tool_calls"
	FinishLength    FinishReason = "length"
	FinishError     FinishReason = "error"
	FinishCancelled FinishReason = "cancelled"
)

type ModelEvent struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

type ModelFailure struct {
	SessionID, ModelID string
	Attempts           int
	Err                error
}

type Fallback struct {
	ModelID    string
	Persistent bool
}

func (m ModelFunc) Spec() ModelSpec { return m.Specification }

func (m ModelFunc) EstimateTokens(ctx context.Context, request Request) (int, error) {
	if m.Estimate == nil {
		return 0, fmtInvalid("missing model token estimator")
	}
	return m.Estimate(ctx, request)
}

func (m ModelFunc) Stream(ctx context.Context, request Request, emit func(ModelEvent)) (Reply, error) {
	if m.Generate == nil {
		return Reply{}, fmtInvalid("missing model stream implementation")
	}
	return m.Generate(ctx, request, emit)
}

func validateModelSpec(spec ModelSpec) error {
	if spec.ID == "" || spec.ContextWindow < 1 || spec.MaxOutputTokens < 1 {
		return fmtInvalid("model needs a stable ID and positive token limits")
	}
	return nil
}

func (a *Agent) model(ctx context.Context, id string) (Model, error) {
	var m Model
	var err error
	if id == a.modelID {
		m = a.config.Model
	} else if a.config.ResolveModel != nil {
		m, err = a.config.ResolveModel(ctx, id)
	} else {
		return nil, fmtInvalid("model %q has no host resolver", id)
	}
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmtInvalid("model resolver returned nil for %q", id)
	}
	spec := m.Spec()
	if err = validateModelSpec(spec); err != nil {
		return nil, err
	}
	if spec.ID != id {
		return nil, fmtInvalid("model resolver returned %q for %q", spec.ID, id)
	}
	return m, nil
}

func stream(ctx context.Context, model Model, request Request, emit func(ModelEvent)) (reply Reply, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("model stream panicked: %v", p)
		}
	}()
	reply, err = model.Stream(ctx, clone(request), func(event ModelEvent) {
		owned, copyErr := copyJSON(event)
		if copyErr == nil {
			emit(owned)
		}
	})
	if err == nil {
		reply, err = copyJSON(reply)
	}
	return
}

func validateReply(reply Reply) error {
	if reply.FinishReason != FinishStop && reply.FinishReason != FinishToolCalls {
		return fmt.Errorf("%w: finish reason %q", ErrIncompleteResponse, reply.FinishReason)
	}
	if reply.Message.Role != RoleAssistant {
		return fmtInvalid("model reply must be an assistant message")
	}
	if reply.FinishReason == FinishToolCalls && len(reply.Message.ToolCalls) == 0 {
		return fmtInvalid("tool_calls finish reason without calls")
	}
	return validateMessage(reply.Message)
}
