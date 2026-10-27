package astral

import (
	"context"
	"time"
)

type AttemptHistory struct {
	store     Store
	sessionID string
	maxBytes  int
}

func (s *Session) Attempts() AttemptHistory {
	return AttemptHistory{store: s.config.Store, sessionID: s.id, maxBytes: s.config.MaxContextBytes}
}

func (h AttemptHistory) Get(ctx context.Context, id string) (ModelAttempt, error) {
	return h.store.GetAttempt(ctx, h.sessionID, id)
}

func (h AttemptHistory) Scan(ctx context.Context, query AttemptQuery) (AttemptPage, error) {
	return h.store.ScanAttempts(ctx, h.sessionID, query)
}

func (h AttemptHistory) Request(ctx context.Context, id string) (Request, error) {
	attempt, err := h.Get(ctx, id)
	if err != nil {
		return Request{}, err
	}
	return RebuildRequest(ctx, h.store, attempt.Input, h.maxBytes)
}

func (s *Session) newAttempt(input RequestInput, call modelCall, number int, retryOf string) ModelAttempt {
	return ModelAttempt{ID: newID(), SessionID: s.id, RunID: call.runID, TurnID: call.turnID, Purpose: call.purpose, Number: number, RetryOf: retryOf, StartedAt: time.Now().UTC(), Input: input}
}

func (s *Session) createAttempt(ctx context.Context, input RequestInput, call modelCall, number int, previous *ModelAttempt) (ModelAttempt, error) {
	retryOf := ""
	var mutations []AttemptMutation
	if previous != nil {
		retryOf = previous.ID
		if previous.Outcome == nil {
			mutations = append(mutations, AttemptMutation{ID: previous.ID, Finish: &AttemptOutcome{Status: "interrupted", FinishedAt: time.Now().UTC(), Error: "process interrupted before a durable result"}})
		}
	}
	attempt := s.newAttempt(input, call, number, retryOf)
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt.Version = s.state.Version + 1
	mutations = append(mutations, AttemptMutation{ID: attempt.ID, Create: &attempt})
	_, err := s.commitLocked(ctx, nil, mutations)
	return attempt, err
}

// resumeGeneration never re-prepares a committed input boundary. Newly queued
// input remains queued until the frozen generation and its tools complete.
func (s *Session) resumeGeneration(ctx context.Context, id string, options prepareOptions) (preparedStep, error) {
	previous, err := s.config.Store.GetAttempt(ctx, s.id, id)
	if err != nil {
		return preparedStep{}, err
	}
	base, err := ReadContext(ctx, s.config.Store, previous.Input.RolloutID, previous.Input.HeadID, s.config.MaxContextBytes)
	if err != nil {
		return preparedStep{}, err
	}
	request, err := previous.Input.Apply(base.Request)
	if err != nil {
		return preparedStep{}, err
	}
	model, err := s.restoreModel(ctx, ModelSnapshot{Spec: previous.Input.Model, Data: previous.Input.Snapshot})
	if err != nil {
		return preparedStep{}, err
	}
	call := modelCall{runID: options.runID, turnID: options.turnID, purpose: "generation", base: base}
	attempt, err := s.createAttempt(ctx, previous.Input, call, 1, &previous)
	if err != nil {
		return preparedStep{}, err
	}
	view := base
	view.Request = request
	return preparedStep{model: model, view: view, base: base, attempt: &attempt}, nil
}
