package astral

import (
	"context"
)

type InputKind string

const (
	InputSteer    InputKind = "steer"
	InputFollowUp InputKind = "follow_up"
)

func (s *Session) Steer(ctx context.Context, message Message) (string, error) {
	return s.queue(ctx, InputSteer, message)
}

func (s *Session) FollowUp(ctx context.Context, message Message) (string, error) {
	return s.queue(ctx, InputFollowUp, message)
}

func (s *Session) queue(ctx context.Context, kind InputKind, message Message) (string, error) {
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
		return "", ErrClosed
	}
	if s.maintenance {
		return "", ErrBusy
	}
	_, err := s.appendLocked(ctx, Entry{Kind: EntryInputQueued, Input: &InputRecord{ID: id, Kind: kind, Message: &message}})
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *Session) pendingInputsLocked(kind InputKind) []InputRecord {
	consumed := map[string]bool{}
	for _, e := range s.rollout.Entries {
		if e.Kind == EntryInputConsumed {
			consumed[e.Input.ID] = true
		}
	}
	var out []InputRecord
	for _, e := range s.rollout.Entries {
		if e.Kind == EntryInputQueued && e.Input.Kind == kind && !consumed[e.Input.ID] {
			out = append(out, *e.Input)
		}
	}
	return out
}
