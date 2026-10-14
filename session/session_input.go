package session

import (
	"context"

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
	page, err := s.agent.config.Store.Pending(ctx, s.id, PendingQuery{Kind: EntryInputQueued, InputKind: kind, Limit: 1, MaxBytes: s.agent.config.MaxContextBytes})
	if err != nil {
		return nil, err
	}
	out := make([]InputRecord, 0, len(page.Records))
	for _, r := range page.Records {
		out = append(out, *r.Entry.Input)
	}
	return out, nil
}
