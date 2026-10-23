package astral

import (
	"context"
	"errors"
	"time"
)

func (s *Session) generate(ctx context.Context, run *Run, turnID string, step preparedStep) (Frame, error) {
	s.publish(Event{Type: EventMessageStart, RunID: run.id, TurnID: turnID})
	reply, err := s.callModel(ctx, step.model, step.view.Request, modelCall{runID: run.id, turnID: turnID, purpose: "generation", configurationID: step.view.ConfigurationID})
	frame := Frame{}
	if err == nil {
		frame = Frame{ID: newID(), TurnID: turnID, Message: reply.Message}
		_, err = s.append(ctx, Entry{Kind: EntryMessage, ID: frame.ID, RunID: run.id, TurnID: turnID, Message: &frame.Message, Usage: reply.Usage, Generation: reply.generation})
	}
	event := Event{Type: EventMessageEnd, RunID: run.id, TurnID: turnID, Error: errorText(err)}
	if err == nil {
		event.EntryID, event.Message, event.Status = frame.ID, &frame.Message, string(frame.Message.FinishReason)
	}
	s.publish(event)
	return frame, err
}

type modelCall struct{ runID, turnID, purpose, configurationID string }

type modelResult struct {
	Reply
	generation *GenerationRecord
}

// callModel owns the complete attempt policy for both generation and summaries.
// Each ordinary retry uses this frozen request. A single bounded repair may
// replace only a private request copy, within the same total attempt budget.
func (s *Session) callModel(ctx context.Context, model Model, request Request, call modelCall) (modelResult, error) {
	policy := s.config.Retry
	frozen := clone(request)
	repaired := false
	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return modelResult{}, err
		}
		attemptID := newID()
		status := AttemptStatus{RunID: call.runID, TurnID: call.turnID, AttemptID: attemptID, Purpose: call.purpose, Attempt: attempt, MaxAttempts: policy.MaxAttempts, State: "running"}
		record := ModelRequestRecord{AttemptID: attemptID, Purpose: call.purpose, Request: frozen}
		requestEntries, err := s.append(ctx, Entry{Kind: EntryModelRequest, RunID: call.runID, TurnID: call.turnID, ModelRequest: &record})
		if err != nil {
			return modelResult{}, err
		}
		s.publishAttempt(EventAttemptStart, status)
		requestCtx, cancel := context.WithTimeout(ctx, policy.RequestTimeout)
		reply, err := stream(requestCtx, model, frozen, func(event ModelEvent) {
			if call.purpose == "generation" {
				s.publish(Event{Type: EventMessageUpdate, RunID: call.runID, TurnID: call.turnID, AttemptID: attemptID, Model: &event})
			}
		})
		err = requestError(ctx, requestCtx, err)
		if reply.Message.FinishReason == FinishCancelled {
			err = errors.Join(err, context.Canceled)
		}
		cancel()
		if err == nil {
			err = validateReply(reply)
		}
		if err == nil {
			err = validateToolBatch(reply.Message, frozen.Tools)
		}
		if err == nil && call.purpose == "compaction" && len(reply.Message.ToolCalls()) != 0 {
			err = fmtInvalid("summarizer returned tool calls")
		}
		if err == nil {
			err = ctx.Err()
		}
		response := ModelResponseRecord{AttemptID: attemptID, Purpose: call.purpose, Reply: reply, Error: errorText(err)}
		cleanup, finish := persistContext(ctx)
		_, recordErr := s.append(cleanup, Entry{Kind: EntryModelResponse, RunID: call.runID, TurnID: call.turnID, ModelResponse: &response})
		finish()
		if recordErr != nil {
			return modelResult{}, errors.Join(err, recordErr)
		}
		if err == nil {
			status.State = "completed"
			s.publishAttempt(EventAttemptEnd, status)
			result := modelResult{Reply: reply}
			if call.purpose == "generation" {
				covered := clone(frozen)
				covered.Messages = append(covered.Messages, reply.Message)
				result.generation = &GenerationRecord{AttemptID: attemptID, RequestID: requestEntries[0].ID, ConfigurationID: call.configurationID, ContextDigest: contextDigest(covered)}
			}
			return result, nil
		}
		status.State, status.Error = "failed", safeModelError(err)
		s.publishAttempt(EventAttemptFailed, status)
		if ctx.Err() != nil {
			return modelResult{}, ctx.Err()
		}
		if errors.Is(err, context.Canceled) || attempt == policy.MaxAttempts {
			return modelResult{}, err
		}
		var classified *ModelError
		retryable := errors.As(err, &classified) && classified.Transient
		if classified != nil && classified.ContextOverflow {
			return modelResult{}, err
		}
		var retryAfter time.Duration
		if classified != nil {
			retryAfter = classified.RetryAfter
		}
		if !repaired {
			candidate, ok, repairErr := RecoverModelRequest(ctx, model, frozen, err)
			if repairErr != nil {
				return modelResult{}, errors.Join(err, repairErr)
			}
			if ok {
				// No preparation callbacks, input selection, compaction or tool execution.
				// Validate the repaired wire request without changing conversation history.
				if _, budgetErr := s.checkRequestBudget(ctx, model, candidate); budgetErr != nil {
					return modelResult{}, budgetErr
				}
				if protocolErr := validateProtocol(candidate.Messages); protocolErr != nil {
					return modelResult{}, protocolErr
				}
				frozen, repaired, retryable = candidate, true, true
			}
		}
		if !retryable {
			return modelResult{}, err
		}
		delay := policy.InitialBackoff
		for n := 1; n < attempt && delay < policy.MaxBackoff; n++ {
			if delay > policy.MaxBackoff/2 {
				delay = policy.MaxBackoff
			} else {
				delay *= 2
			}
		}
		if delay > policy.MaxBackoff {
			delay = policy.MaxBackoff
		}
		if retryAfter > delay {
			delay = retryAfter
		}
		status.State, status.NextRetryAt = "retrying", time.Now().UTC().Add(delay)
		s.publishAttempt(EventRetry, status)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return modelResult{}, ctx.Err()
		}
	}
	return modelResult{}, ErrIncompleteResponse
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
func safeModelError(err error) string {
	var modelError *ModelError
	if errors.As(err, &modelError) && modelError.Code != "" {
		code := modelError.Code
		if len(code) <= 64 {
			valid := true
			for _, c := range code {
				valid = valid && (c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.')
			}
			if valid {
				return code
			}
		}
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, ErrIncompleteResponse) {
		return "incomplete_response"
	}
	return "model_request_failed"
}
func (s *Session) publishAttempt(kind EventType, status AttemptStatus) {
	s.mu.Lock()
	s.attempt = &status
	s.publishLocked(Event{Type: kind, RunID: status.RunID, TurnID: status.TurnID, AttemptID: status.AttemptID, Attempt: &status, Error: status.Error})
	s.mu.Unlock()
}

// Recovery belongs to the complete turn boundary, outside the HTTP retry loop.
// The failed generation has committed no assistant/tool calls. Already executed
// tools and consumed inputs remain committed and are never executed again.
func (s *Session) generateWithRecovery(ctx context.Context, run *Run, turnID string, step preparedStep) (Frame, error) {
	frame, err := s.generate(ctx, run, turnID, step)
	var failure *ModelError
	if s.config.Compaction.Disabled || !errors.As(err, &failure) || !failure.ContextOverflow || ctx.Err() != nil {
		return frame, err
	}
	recovered, _, compactErr := s.prepareStep(ctx, prepareOptions{runID: run.id, turnID: turnID, compact: true, noInputs: true, overflow: true, reason: "context_overflow"})
	if compactErr != nil {
		return Frame{}, errors.Join(err, compactErr)
	}
	return s.generate(ctx, run, turnID, recovered)
}
