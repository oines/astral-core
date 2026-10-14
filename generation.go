package astral

import (
	"context"
	"errors"

	"github.com/oines/astral-core/internal/retry"
)

func (a *Agent) generate(ctx context.Context, run *Run, turnID string) (Frame, error) {
	p := a.config.Retry
	override := ""
	seen := map[string]bool{}
	attempts := 0
	var lastErr error
	var lastModel string
	for round := 0; round <= p.MaxFallbacks; round++ {
		preferred, attemptID := "", ""
		fallbackAllowed := false
		frame, err := retry.Do(ctx, retry.Policy{Attempts: p.MaxAttempts, Initial: p.InitialBackoff, Maximum: p.MaxBackoff}, func(attempt int) (Frame, retry.Decision, error) {
			fallbackAllowed = false
			current := a.driver.Snapshot().Configuration.ModelID
			if attempt > 0 && current != preferred {
				override = ""
			}
			preferred = current
			model, request, err := a.driver.Prepare(ctx, override)
			if err != nil {
				return Frame{}, retry.Decision{}, err
			}
			lastModel = model.Spec().ID
			attemptID = newID()
			a.Publish(Event{Type: EventMessageStart, RunID: run.id, TurnID: turnID, AttemptID: attemptID})
			requestCtx := ctx
			cancel := func() {}
			if p.RequestTimeout > 0 {
				requestCtx, cancel = context.WithTimeout(ctx, p.RequestTimeout)
			}
			reply, streamErr := stream(requestCtx, model, request, func(e ModelEvent) {
				a.Publish(Event{Type: EventMessageUpdate, RunID: run.id, TurnID: turnID, AttemptID: attemptID, Model: &e})
			})
			streamErr = requestError(ctx, requestCtx, streamErr)
			cancel()
			attempts++
			if streamErr == nil {
				streamErr = validateReply(reply)
			}
			if streamErr == nil {
				streamErr = ctx.Err()
			}
			if streamErr == nil {
				frame := Frame{ID: newID(), TurnID: turnID, Message: reply.Message}
				if err = a.driver.Commit(ctx, Transition{Kind: TransitionMessage, ID: frame.ID, RunID: run.id, TurnID: turnID, Message: &frame.Message, Usage: reply.Usage}); err != nil {
					return Frame{}, retry.Decision{}, err
				}
				a.Publish(Event{Type: EventMessageEnd, RunID: run.id, TurnID: turnID, AttemptID: attemptID, EntryID: frame.ID})
				return frame, retry.Decision{}, nil
			}
			fallbackAllowed = true
			a.Publish(Event{Type: EventAttemptFailed, RunID: run.id, TurnID: turnID, AttemptID: attemptID, Error: streamErr.Error()})
			var classified *ModelError
			decision := retry.Decision{}
			if errors.As(streamErr, &classified) {
				decision = retry.Decision{Retry: classified.Transient, After: classified.RetryAfter}
			}
			return Frame{}, decision, streamErr
		}, func(err error) {
			a.Publish(Event{Type: EventRetry, RunID: run.id, TurnID: turnID, AttemptID: attemptID, Error: err.Error()})
		})
		if err == nil {
			return frame, nil
		}
		lastErr = err
		if !fallbackAllowed {
			return Frame{}, err
		}
		if ctx.Err() != nil {
			return Frame{}, ctx.Err()
		}
		if a.config.Fallback == nil || round == p.MaxFallbacks {
			return Frame{}, lastErr
		}
		fallback, err := a.config.Fallback(ctx, ModelFailure{SessionID: a.driver.Snapshot().ID, ModelID: lastModel, Attempts: attempts, Err: lastErr})
		if err != nil {
			return Frame{}, err
		}
		seen[lastModel] = true
		if fallback.ModelID == "" || seen[fallback.ModelID] {
			return Frame{}, lastErr
		}
		if fallback.Persistent {
			if err = a.driver.Commit(ctx, Transition{Kind: TransitionSelectModel, ID: newID(), ModelID: fallback.ModelID}); err != nil {
				return Frame{}, err
			}
			override = ""
		} else {
			override = fallback.ModelID
		}
	}
	return Frame{}, lastErr
}
func requestError(parent, request context.Context, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(request.Err(), context.DeadlineExceeded) {
		return &ModelError{Code: "request_timeout", Transient: true, Cause: context.DeadlineExceeded}
	}
	return err
}
