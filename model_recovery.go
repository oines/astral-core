package astral

import (
	"context"
	"fmt"
)

// ModelRequestRecoverer may remove provider replay state that the provider
// explicitly rejected. Core owns the single recovery attempt and records its
// actual request, just like every normal attempt. Implementations never retry
// network requests themselves or alter durable conversation messages.
type ModelRequestRecoverer interface {
	RecoverRequest(context.Context, Request, error) (Request, bool, error)
}

func RecoverModelRequest(ctx context.Context, model Model, request Request, cause error) (out Request, recovered bool, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("model request recovery panicked: %v", p)
			recovered = false
		}
	}()
	if ctx.Err() != nil {
		return Request{}, false, ctx.Err()
	}
	recoverer, ok := model.(ModelRequestRecoverer)
	if !ok {
		return Request{}, false, nil
	}
	owned, err := copyJSON(request)
	if err != nil {
		return Request{}, false, err
	}
	out, recovered, err = recoverer.RecoverRequest(ctx, owned, cause)
	if err != nil || !recovered {
		return Request{}, false, err
	}
	if out.ModelID != request.ModelID || out.ModelRevision != request.ModelRevision ||
		out.SystemPrompt != request.SystemPrompt || out.MaxOutputTokens != request.MaxOutputTokens ||
		!sameJSON(out.Tools, request.Tools) || !sameJSON(out.Options, request.Options) {
		return Request{}, false, fmtInvalid("request recovery changed model configuration")
	}
	if sameJSON(out.Messages, request.Messages) {
		return Request{}, false, nil
	}
	for _, message := range out.Messages {
		if err := message.Validate(); err != nil {
			return Request{}, false, err
		}
	}
	out, err = copyJSON(out)
	return out, err == nil, err
}
