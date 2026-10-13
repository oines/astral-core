package astral

import (
	"context"
	"errors"
	"time"
)

func (s *Session) generate(ctx context.Context, run *Run, turnID string) (Entry, error) {
	policy := s.agent.config.Retry
	override := ""
	seenFallbacks := map[string]bool{}
	var lastErr error
	var lastModelID string
	attempts := 0
	for fallbackRound := 0; fallbackRound <= policy.MaxFallbacks; fallbackRound++ {
		for attempt := 0; attempt < policy.MaxAttempts; attempt++ {
			if err := ctx.Err(); err != nil {
				return Entry{}, err
			}
			s.mu.Lock()
			preferred := s.configurationLocked().ModelID
			s.mu.Unlock()
			id := preferred
			if override != "" {
				id = override
			}
			model, err := s.agent.model(ctx, id)
			if err != nil {
				return Entry{}, err
			}
			view, err := s.prepareRequest(ctx, model)
			if err != nil {
				return Entry{}, err
			}
			// Compaction may await a separate model for a long time. Honor a
			// selection made during that preparation, with a fresh budget check.
			// Do not compact again for this attempt if the new window is smaller.
			s.mu.Lock()
			current := s.configurationLocked().ModelID
			s.mu.Unlock()
			if current != preferred {
				override = ""
				id = current
				model, err = s.agent.model(ctx, id)
				if err == nil {
					view, err = s.requestContext(ctx, model)
				}
				if err != nil {
					return Entry{}, err
				}
			}
			lastModelID = id
			attemptID := newID()
			record := AttemptRecord{ID: attemptID, ModelID: id, ConfigurationID: view.ConfigurationID, HeadID: view.HeadID, Status: "started", Purpose: "generation"}
			if _, err = s.append(ctx, Entry{Kind: EntryAttempt, RunID: run.id, TurnID: turnID, Attempt: &record}); err != nil {
				return Entry{}, err
			}
			s.publish(Event{Type: EventMessageStart, RunID: run.id, TurnID: turnID, AttemptID: attemptID})
			requestCtx := ctx
			cancel := func() {}
			if policy.RequestTimeout > 0 {
				requestCtx, cancel = context.WithTimeout(ctx, policy.RequestTimeout)
			}
			reply, streamErr := stream(requestCtx, model, view.Request, func(delta ModelEvent) {
				s.publish(Event{Type: EventMessageUpdate, RunID: run.id, TurnID: turnID, AttemptID: attemptID, Model: &delta})
			})
			streamErr = requestError(ctx, requestCtx, streamErr)
			cancel()
			attempts++
			if streamErr == nil {
				streamErr = validateReply(reply)
			}
			if streamErr == nil && ctx.Err() != nil {
				streamErr = ctx.Err()
			}
			if streamErr == nil {
				record.Status = "succeeded"
				record.Usage = reply.Usage
				committed, err := s.append(ctx, Entry{Kind: EntryAttempt, RunID: run.id, TurnID: turnID, Attempt: &record}, Entry{Kind: EntryMessage, RunID: run.id, TurnID: turnID, Message: &reply.Message})
				if err != nil {
					return Entry{}, err
				}
				assistant := committed[1]
				s.publish(Event{Type: EventMessageEnd, RunID: run.id, TurnID: turnID, AttemptID: attemptID, EntryID: assistant.ID})
				return assistant, nil
			}
			lastErr = streamErr
			record.Status = "failed"
			record.Error = streamErr.Error()
			cleanup, done := persistContext(ctx)
			_, err = s.append(cleanup, Entry{Kind: EntryAttempt, RunID: run.id, TurnID: turnID, Attempt: &record})
			done()
			if err != nil {
				return Entry{}, errors.Join(streamErr, err)
			}
			s.publish(Event{Type: EventAttemptFailed, RunID: run.id, TurnID: turnID, AttemptID: attemptID, Error: streamErr.Error()})
			if ctx.Err() != nil {
				return Entry{}, ctx.Err()
			}
			var classified *ModelError
			retryable := errors.As(streamErr, &classified) && classified.Transient
			if !retryable || attempt+1 == policy.MaxAttempts {
				break
			}
			delay := policy.InitialBackoff
			for n := 0; n < attempt && delay < policy.MaxBackoff; n++ {
				if delay > policy.MaxBackoff/2 {
					delay = policy.MaxBackoff
				} else {
					delay *= 2
				}
			}
			if classified.RetryAfter > delay {
				delay = classified.RetryAfter
			}
			s.publish(Event{Type: EventRetry, RunID: run.id, TurnID: turnID, AttemptID: attemptID, Error: streamErr.Error()})
			if err = waitDelay(ctx, delay); err != nil {
				return Entry{}, err
			}
			// A user's explicit selection during a failed request takes precedence
			// over a temporary fallback on the next retry.
			s.mu.Lock()
			changed := s.configurationLocked().ModelID != preferred
			s.mu.Unlock()
			if changed {
				override = ""
			}
		}
		if ctx.Err() != nil {
			return Entry{}, ctx.Err()
		}
		if s.agent.config.Fallback == nil || fallbackRound == policy.MaxFallbacks {
			return Entry{}, lastErr
		}
		alternative, err := s.agent.config.Fallback(ctx, ModelFailure{SessionID: s.id, ModelID: lastModelID, Attempts: attempts, Err: lastErr})
		if err != nil {
			return Entry{}, err
		}
		seenFallbacks[lastModelID] = true
		if alternative.ModelID == "" || seenFallbacks[alternative.ModelID] {
			return Entry{}, lastErr
		}
		if alternative.Persistent {
			if err = s.SelectModel(ctx, alternative.ModelID); err != nil {
				return Entry{}, err
			}
			override = ""
		} else {
			override = alternative.ModelID
		}
	}
	return Entry{}, lastErr
}

func (s *Session) prepareRequest(ctx context.Context, model Model) (Context, error) {
	view, err := s.requestContext(ctx, model)
	near := err == nil && s.agent.config.Budget.AutoCompactAt > 0 && view.EstimatedInput > model.Spec().ContextWindow-view.Request.MaxOutputTokens-s.agent.config.Budget.SafetyMargin-s.agent.config.Budget.AutoCompactAt
	if (errors.Is(err, ErrBudget) || near) && !s.agent.config.Compaction.Disabled {
		_, compactErr := s.compact(ctx, "", model)
		if compactErr != nil {
			if near && errors.Is(compactErr, ErrCompactionNoProgress) {
				return view, nil
			}
			return Context{}, errors.Join(err, compactErr)
		}
		// At most one compaction for this request preparation. This prevents a
		// summary which makes no useful progress from causing an unbounded loop.
		return s.requestContext(ctx, model)
	}
	return view, err
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

func waitDelay(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
