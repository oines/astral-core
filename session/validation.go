package session

import (
	"context"
	"errors"
	"fmt"
	"sync"

	protocol "github.com/oines/astral-core"
	"github.com/oines/astral-core/internal/streamcheck"
)

func stream(ctx context.Context, model protocol.Model, request protocol.Request, emit func(protocol.ModelEvent)) (reply protocol.Reply, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("model stream panicked: %v", p)
		}
	}()
	var check streamcheck.Check
	var mu sync.Mutex
	reply, err = model.Stream(ctx, clone(request), func(event protocol.ModelEvent) {
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
	if err == nil {
		reply, err = copyJSON(reply)
	}
	if err == nil && (reply.Message.FinishReason == protocol.FinishStop || reply.Message.FinishReason == protocol.FinishToolCalls) {
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

func validateReply(reply protocol.Reply) error {
	if reply.Message.FinishReason != protocol.FinishStop && reply.Message.FinishReason != protocol.FinishToolCalls {
		return fmt.Errorf("%w: finish reason %q", protocol.ErrIncompleteResponse, reply.Message.FinishReason)
	}
	if reply.Message.Role != protocol.RoleAssistant {
		return fmtInvalid("model reply must be an assistant message")
	}
	if reply.Message.FinishReason == protocol.FinishToolCalls && len(reply.Message.ToolCalls()) == 0 {
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
