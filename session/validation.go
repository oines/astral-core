package session

import (
	"context"
	"fmt"

	protocol "github.com/oines/astral-core"
)

func stream(ctx context.Context, model protocol.Model, request protocol.Request, emit func(protocol.ModelEvent)) (reply protocol.Reply, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("model stream panicked: %v", p)
		}
	}()
	reply, err = model.Stream(ctx, clone(request), func(event protocol.ModelEvent) {
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

func validateReply(reply protocol.Reply) error {
	if reply.FinishReason != protocol.FinishStop && reply.FinishReason != protocol.FinishToolCalls {
		return fmt.Errorf("%w: finish reason %q", protocol.ErrIncompleteResponse, reply.FinishReason)
	}
	if reply.Message.Role != protocol.RoleAssistant {
		return fmtInvalid("model reply must be an assistant message")
	}
	if reply.FinishReason == protocol.FinishToolCalls && len(reply.Message.ToolCalls) == 0 {
		return fmtInvalid("tool_calls finish reason without calls")
	}
	return validateMessage(reply.Message)
}
func validateModelSpec(spec protocol.ModelSpec) error {
	if spec.ID == "" || spec.ContextWindow < 1 || spec.MaxOutputTokens < 1 {
		return fmtInvalid("model needs a stable ID and positive token limits")
	}
	return nil
}
