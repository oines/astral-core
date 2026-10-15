package session

import (
	"context"
	"encoding/json"
	"errors"

	protocol "github.com/oines/astral-core"
)

type InputKind string

const (
	InputSteer    InputKind = "steer"
	InputFollowUp InputKind = "follow_up"
)

func (s *Session) Steer(ctx context.Context, message protocol.Message) (string, error) {
	return s.queue(ctx, InputSteer, message)
}

func (s *Session) FollowUp(ctx context.Context, message protocol.Message) (string, error) {
	return s.queue(ctx, InputFollowUp, message)
}

func (s *Session) queue(ctx context.Context, kind InputKind, message protocol.Message) (string, error) {
	if err := s.agent.checkOpen(); err != nil {
		return "", err
	}
	if err := validateUser(message); err != nil {
		return "", err
	}
	id := newID()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", protocol.ErrClosed
	}
	if s.maintenance {
		return "", protocol.ErrBusy
	}
	_, err := s.appendLocked(ctx, Entry{Kind: EntryInputQueued, Input: &InputRecord{ID: id, Kind: kind, Message: &message}})
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *Session) pendingInputsLocked(ctx context.Context, kind InputKind) ([]InputRecord, error) {
	if s.state.PendingInputs[kind] == 0 {
		return nil, nil
	}
	limit := 1
	if kind == InputSteer {
		limit = s.agent.config.MaxSteeringPerTurn
	}
	page, err := s.agent.config.Store.Pending(ctx, s.id, PendingQuery{Kind: EntryInputQueued, InputKind: kind, Limit: limit, MaxBytes: s.agent.config.MaxContextBytes})
	if err != nil {
		return nil, err
	}
	out := make([]InputRecord, 0, len(page.Records))
	for _, r := range page.Records {
		out = append(out, *r.Entry.Input)
	}
	return out, nil
}

func (s *Session) steeringEntriesLocked(runID string, pending []InputRecord) ([]Entry, error) {
	view, err := project(s.pathLocked())
	if err != nil {
		return nil, err
	}
	request := view.Request
	// Reserve the uncapped output setting's JSON width, without resolving a
	// model under the queue lock. Prepare retains token checks and compaction.
	request.MaxOutputTokens = s.agent.config.Budget.OutputTokens
	if request.MaxOutputTokens == 0 {
		request.MaxOutputTokens = 16384
	}
	var accepted []Entry
	for _, item := range pending {
		request.Messages = append(request.Messages, *item.Message)
		raw, err := json.Marshal(request)
		if err != nil {
			return nil, err
		}
		var candidate []Entry
		if len(raw) > s.agent.config.MaxContextBytes {
			err = &BudgetError{Input: len(raw), Window: s.agent.config.MaxContextBytes}
		} else {
			drafts := append(accepted, Entry{Kind: EntryMessage, RunID: runID, Message: item.Message}, Entry{Kind: EntryInputConsumed, RunID: runID, Input: &InputRecord{ID: item.ID, Kind: item.Kind}})
			candidate, err = s.prepareEntriesLocked(drafts)
		}
		if err != nil {
			if len(accepted) > 0 && errors.Is(err, ErrBudget) {
				break
			}
			return nil, err
		}
		accepted = candidate
	}
	return accepted, nil
}
