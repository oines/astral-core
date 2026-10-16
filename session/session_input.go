package session

import (
	"context"
	"encoding/json"
	"errors"
	protocol "github.com/oines/astral-core"
	"sort"
)

type InputKind = protocol.InputKind

const (
	InputSteer    = protocol.InputSteer
	InputFollowUp = protocol.InputFollowUp
	InputContext  = protocol.InputContext
)
const maxPassivePerBoundary = 64

func sessionInputOptions(options []protocol.InputOptions) ([]protocol.Message, error) {
	if len(options) > 1 {
		return nil, fmtInvalid("at most one InputOptions value is allowed")
	}
	if len(options) == 0 {
		return nil, nil
	}
	for _, m := range options[0].Contexts {
		if err := m.ValidateContext(); err != nil {
			return nil, err
		}
	}
	return copyJSON(options[0].Contexts)
}
func (s *Session) Steer(ctx context.Context, message protocol.Message, options ...protocol.InputOptions) (string, error) {
	return s.queue(ctx, InputSteer, message, options)
}
func (s *Session) FollowUp(ctx context.Context, message protocol.Message, options ...protocol.InputOptions) (string, error) {
	return s.queue(ctx, InputFollowUp, message, options)
}
func (s *Session) queue(ctx context.Context, kind InputKind, message protocol.Message, options []protocol.InputOptions) (string, error) {
	contexts, err := sessionInputOptions(options)
	if err != nil {
		return "", err
	}
	return s.enqueue(ctx, InputRecord{Kind: kind, Message: &message, Contexts: contexts})
}
func (s *Session) admitInput(q InputRecord) error {
	if err := q.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(q)
	if err != nil {
		return err
	}
	if len(raw) > s.agent.config.MaxContextBytes {
		return &BudgetError{Input: len(raw), Window: s.agent.config.MaxContextBytes}
	}
	return nil
}
func (s *Session) enqueue(ctx context.Context, q InputRecord) (string, error) {
	if err := s.agent.checkOpen(); err != nil {
		return "", err
	}
	q.ID = newID()
	if err := s.admitInput(q); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", protocol.ErrClosed
	}
	_, err := s.appendLocked(ctx, Entry{Kind: EntryInputQueued, Input: &q})
	if err != nil {
		return "", err
	}
	return q.ID, nil
}
func (s *Session) InjectContext(ctx context.Context, batch []protocol.Message) (string, error) {
	id, err := s.enqueue(ctx, InputRecord{Kind: InputContext, Contexts: batch})
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return id, protocol.ErrClosed
	}
	if !s.busyLocked() && len(s.state.Branch.PendingCalls) == 0 {
		_, err = s.applyInputsLocked(ctx, "", true, false)
	}
	return id, err
}
func (s *Session) pendingRecordsLocked(ctx context.Context, kind InputKind, limit int) ([]Record, error) {
	if s.state.PendingInputs[kind] == 0 {
		return nil, nil
	}
	page, err := s.agent.config.Store.Pending(ctx, s.id, PendingQuery{Kind: EntryInputQueued, InputKind: kind, Limit: limit, MaxBytes: 2 * s.agent.config.MaxContextBytes})
	return page.Records, err
}
func (s *Session) selectedInputsLocked(ctx context.Context, passiveOnly, natural bool) ([]Record, error) {
	var selected []Record
	if !passiveOnly {
		rows, err := s.pendingRecordsLocked(ctx, InputSteer, s.agent.config.MaxSteeringPerTurn)
		if err != nil {
			return nil, err
		}
		selected = append(selected, rows...)
		if len(rows) == 0 && natural {
			rows, err = s.pendingRecordsLocked(ctx, InputFollowUp, 1)
			if err != nil {
				return nil, err
			}
			selected = append(selected, rows...)
		}
	}
	rows, err := s.pendingRecordsLocked(ctx, InputContext, maxPassivePerBoundary)
	if err != nil {
		return nil, err
	}
	selected = append(selected, rows...)
	sort.Slice(selected, func(i, j int) bool { return selected[i].Entry.Sequence < selected[j].Entry.Sequence })
	return selected, nil
}
func contextEntries(contexts []protocol.Message, runID, inputID string) []Entry {
	out := make([]Entry, 0, len(contexts))
	for _, m := range contexts {
		owned := m
		out = append(out, Entry{Kind: EntryContext, RunID: runID, InputID: inputID, Message: &owned})
	}
	return out
}

// applyInputsLocked commits a fitting prefix of indivisible delivery groups.
func (s *Session) applyInputsLocked(ctx context.Context, runID string, passiveOnly, natural bool) (bool, error) {
	if len(s.state.Branch.PendingCalls) > 0 {
		return false, nil
	}
	selected, err := s.selectedInputsLocked(ctx, passiveOnly, natural)
	if err != nil {
		return false, err
	}
	if len(selected) == 0 {
		return false, nil
	}
	view, err := project(s.pathLocked())
	if err != nil {
		return false, err
	}
	request := view.Request
	request.MaxOutputTokens = s.agent.config.Budget.OutputTokens
	if request.MaxOutputTokens == 0 {
		request.MaxOutputTokens = 16384
	}
	var accepted []Entry
	driven := false
	passiveBlocked := false
	for _, record := range selected {
		item := record.Entry.Input
		if item.Kind == InputContext && passiveBlocked {
			continue
		}
		candidate := request
		candidate.Messages = append(append([]protocol.Message(nil), request.Messages...), item.Messages()...)
		raw, err := json.Marshal(candidate)
		if err != nil {
			return false, err
		}
		drafts := append([]Entry(nil), accepted...)
		drafts = append(drafts, contextEntries(item.Contexts, runID, item.ID)...)
		if item.Message != nil {
			drafts = append(drafts, Entry{Kind: EntryMessage, RunID: runID, InputID: item.ID, Message: item.Message})
		}
		drafts = append(drafts, Entry{Kind: EntryInputConsumed, RunID: runID, Input: &InputRecord{ID: item.ID, Kind: item.Kind}})
		var prepared []Entry
		if len(raw) > s.agent.config.MaxContextBytes {
			err = &BudgetError{Input: len(raw), Window: s.agent.config.MaxContextBytes}
		} else {
			prepared, err = s.prepareEntriesLocked(drafts)
		}
		if err != nil {
			if !errors.Is(err, ErrBudget) {
				return false, err
			}
			if item.Kind == InputContext {
				passiveBlocked = true
				continue
			}
			if !driven {
				return false, err
			}
			break
		}
		accepted = prepared
		request = candidate
		driven = driven || item.Kind != InputContext
	}
	if len(accepted) > 0 {
		if _, err = s.appendLocked(ctx, accepted...); err != nil {
			return false, err
		}
	}
	return driven, nil
}
func (s *Session) HasQueuedInputs(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, protocol.ErrClosed
	}
	return s.state.PendingInputs[InputSteer]+s.state.PendingInputs[InputFollowUp] > 0, nil
}

// PeekQueuedInputs returns all driving inputs in durable acceptance order.
func (s *Session) PeekQueuedInputs(ctx context.Context) ([]InputRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, protocol.ErrClosed
	}
	var rows []Record
	for _, kind := range []InputKind{InputSteer, InputFollowUp} {
		cursor := ""
		for {
			page, err := s.agent.config.Store.Pending(ctx, s.id, PendingQuery{Kind: EntryInputQueued, InputKind: kind, Cursor: cursor, MaxBytes: 2 * s.agent.config.MaxContextBytes})
			if err != nil {
				return nil, err
			}
			rows = append(rows, page.Records...)
			if page.Next == "" {
				break
			}
			cursor = page.Next
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Entry.Sequence < rows[j].Entry.Sequence })
	var out []InputRecord
	for _, r := range rows {
		out = append(out, *r.Entry.Input)
	}
	return out, nil
}
func (s *Session) ClearSteering(ctx context.Context) error  { return s.clearInputs(ctx, InputSteer) }
func (s *Session) ClearFollowUps(ctx context.Context) error { return s.clearInputs(ctx, InputFollowUp) }
func (s *Session) ClearQueuedInputs(ctx context.Context) error {
	return s.clearInputs(ctx, InputSteer, InputFollowUp)
}
func (s *Session) clearInputs(ctx context.Context, kinds ...InputKind) error {
	if err := s.agent.checkOpen(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return protocol.ErrClosed
	}
	pending := false
	for _, k := range kinds {
		pending = pending || s.state.PendingInputs[k] > 0
	}
	if !pending {
		return ctx.Err()
	}
	_, err := s.appendLocked(ctx, Entry{Kind: EntryInputCleared, Clear: &InputClear{Kinds: kinds}})
	return err
}
