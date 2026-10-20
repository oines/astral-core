package astral

import (
	"context"
	"errors"

	"github.com/oines/astral-core/internal/retry"
)

func (a *Agent) generate(ctx context.Context, run *Run, turnID string) (adopted Frame, finalErr error) {
	messageID := newID()
	started := false
	lastAttempt := ""
	var lastReply Reply
	defer func() {
		if !started {
			return
		}
		if finalErr != nil && adopted.ID == "" {
			m := lastReply.Message
			m.Role = RoleAssistant
			m.FinishReason = FinishError
			if m.ErrorMessage == "" {
				m.ErrorMessage = finalErr.Error()
			}
			if ctx.Err() != nil || errors.Is(finalErr, context.Canceled) || lastReply.Message.FinishReason == FinishCancelled {
				m.FinishReason = FinishCancelled
			}
			if m.Validate() != nil {
				m = Message{Role: RoleAssistant, FinishReason: m.FinishReason, ErrorMessage: m.ErrorMessage}
			}
			frame := Frame{ID: messageID, TurnID: turnID, Message: m}
			cleanup, cancel := persistContext(ctx)
			err := a.driver.Commit(cleanup, Transition{Kind: TransitionMessage, ID: frame.ID, RunID: run.id, TurnID: turnID, Message: &frame.Message, Usage: lastReply.Usage})
			cancel()
			finalErr = errors.Join(finalErr, err)
			if err == nil {
				adopted = frame
			}
		}
		event := Event{Type: EventMessageEnd, RunID: run.id, TurnID: turnID, AttemptID: lastAttempt, EntryID: adopted.ID}
		if adopted.ID != "" {
			event.Message = &adopted.Message
			event.Status = string(adopted.Message.FinishReason)
		}
		if finalErr != nil {
			event.Error = finalErr.Error()
		}
		a.Publish(event)
	}()
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
			// A prepare failure has not requested the next model response yet. Keep
			// a pending FinishTurn continuation available for explicit recovery.
			if !started {
				if err := a.driver.Commit(ctx, Transition{Kind: TransitionTurnStart, ID: newID(), RunID: run.id, TurnID: turnID}); err != nil {
					return Frame{}, retry.Decision{}, err
				}
			}
			lastModel = model.Spec().ID
			attemptID = newID()
			lastAttempt = attemptID
			record := ModelRequestRecord{AttemptID: attemptID, Purpose: "generation", Request: request}
			if err := a.driver.Commit(ctx, Transition{Kind: TransitionModelRequest, ID: newID(), RunID: run.id, TurnID: turnID, ModelRequest: &record}); err != nil {
				return Frame{}, retry.Decision{}, err
			}
			if !started {
				a.Publish(Event{Type: EventMessageStart, RunID: run.id, TurnID: turnID})
				started = true
			}
			a.Publish(Event{Type: EventAttemptStart, RunID: run.id, TurnID: turnID, AttemptID: attemptID})
			requestCtx := ctx
			cancel := func() {}
			if p.RequestTimeout > 0 {
				requestCtx, cancel = context.WithTimeout(ctx, p.RequestTimeout)
			}
			reply, streamErr := stream(requestCtx, model, request, func(e ModelEvent) {
				a.Publish(Event{Type: EventMessageUpdate, RunID: run.id, TurnID: turnID, AttemptID: attemptID, Model: &e})
			})
			lastReply = reply
			streamErr = requestError(ctx, requestCtx, streamErr)
			if reply.Message.FinishReason == FinishCancelled {
				streamErr = errors.Join(streamErr, context.Canceled)
			}
			cancel()
			attempts++
			if streamErr == nil {
				streamErr = validateReply(reply)
			}
			if streamErr == nil {
				streamErr = validateToolBatch(reply.Message, request.Tools)
			}
			if streamErr == nil {
				streamErr = ctx.Err()
			}
			response := ModelResponseRecord{AttemptID: attemptID, Purpose: "generation", Reply: reply}
			if streamErr != nil {
				response.Error = streamErr.Error()
			}
			cleanup, finish := persistContext(ctx)
			recordErr := a.driver.Commit(cleanup, Transition{Kind: TransitionModelResponse, ID: newID(), RunID: run.id, TurnID: turnID, ModelResponse: &response})
			finish()
			if recordErr != nil {
				return Frame{}, retry.Decision{}, errors.Join(streamErr, recordErr)
			}
			if streamErr == nil {
				frame := Frame{ID: messageID, TurnID: turnID, Message: reply.Message}
				var synthetic []ToolCompletion
				if frame.Message.FinishReason == FinishLength {
					for _, call := range frame.Message.ToolCalls() {
						synthetic = append(synthetic, ToolCompletion{ID: newID(), Invocation: Invocation{SessionID: a.driver.Snapshot().ID, RunID: run.id, TurnID: turnID, AssistantID: frame.ID, ExecutionID: newID(), Call: clone(call)}, Result: toolFailure("response_length", "Tool was not executed because the model response was truncated.", OutcomeNotExecuted)})
					}
				}
				if err = a.driver.Commit(ctx, Transition{Kind: TransitionMessage, ID: frame.ID, RunID: run.id, TurnID: turnID, Message: &frame.Message, Usage: reply.Usage, SyntheticResults: synthetic}); err != nil {
					return Frame{}, retry.Decision{}, err
				}
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
		if errors.Is(err, context.Canceled) {
			return Frame{}, err
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
