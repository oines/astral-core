package astral

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

type InputKind string

const (
	InputSteer    InputKind = "steer"
	InputFollowUp InputKind = "follow_up"
	InputContext  InputKind = "context"
)

// Input is a stable submission. Digest may identify the caller's original
// logical request before timestamps or attachment bytes are frozen. Callers
// check LookupInput before repeating expensive payload construction.
type Input struct {
	ID       string    `json:"id"`
	Digest   string    `json:"digest,omitempty"`
	Message  Message   `json:"message,omitempty"`
	Contexts []Message `json:"contexts,omitempty"`
}
type InputState string

const (
	InputPending InputState = "pending"
	InputApplied InputState = "applied"
	InputCleared InputState = "cleared"
)

type InputReceipt struct {
	InputRecord
	AcceptedAt time.Time  `json:"accepted_at"`
	State      InputState `json:"state"`
	EntryID    string     `json:"entry_id,omitempty"`
}
type QueuedInput struct {
	ID       string    `json:"id"`
	Digest   string    `json:"digest"`
	Kind     InputKind `json:"kind"`
	Message  *Message  `json:"message,omitempty"`
	Contexts []Message `json:"contexts,omitempty"`
}

func (q QueuedInput) Validate() error {
	if q.Kind != InputSteer && q.Kind != InputFollowUp && q.Kind != InputContext {
		return fmtInvalid("unknown input kind")
	}
	if q.Kind == InputContext {
		if q.Message != nil || len(q.Contexts) == 0 {
			return fmtInvalid("passive delivery needs context only")
		}
	} else if q.Message == nil {
		return fmtInvalid("missing input message")
	} else if err := validateUser(*q.Message); err != nil {
		return err
	}
	for _, m := range q.Contexts {
		if err := m.ValidateContext(); err != nil {
			return err
		}
	}
	return nil
}
func (q QueuedInput) Messages() []Message {
	out := append([]Message(nil), q.Contexts...)
	if q.Message != nil {
		out = append(out, *q.Message)
	}
	return out
}
func inputRecord(kind InputKind, input Input) (InputRecord, error) {
	if input.ID == "" {
		return InputRecord{}, fmtInvalid("stable input ID is required")
	}
	q := InputRecord{ID: input.ID, Digest: input.Digest, Kind: kind, Contexts: input.Contexts}
	if kind != InputContext {
		q.Message = &input.Message
	}
	if q.Digest == "" {
		raw, err := json.Marshal(struct {
			Message  *Message
			Contexts []Message
		}{q.Message, q.Contexts})
		if err != nil {
			return InputRecord{}, err
		}
		hash := sha256.Sum256(raw)
		q.Digest = hex.EncodeToString(hash[:])
	}
	return q, nil
}
func (s *Session) LookupInput(ctx context.Context, id string) (InputReceipt, error) {
	if err := s.checkOpen(); err != nil {
		return InputReceipt{}, err
	}
	return s.config.Store.LookupInput(ctx, s.id, id)
}
func (s *Session) existingInputLocked(ctx context.Context, q InputRecord) (InputReceipt, bool, error) {
	old, err := s.config.Store.LookupInput(ctx, s.id, q.ID)
	if err == nil {
		if old.Digest != q.Digest {
			return InputReceipt{}, false, ErrConflict
		}
		return old, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return InputReceipt{}, false, err
	}
	return InputReceipt{}, false, nil
}
func (s *Session) admitInput(q InputRecord) error {
	if err := q.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(q)
	if err != nil {
		return err
	}
	if len(raw) > s.config.MaxContextBytes {
		return &BudgetError{Input: len(raw), Window: s.config.MaxContextBytes}
	}
	return nil
}
func (s *Session) enqueue(ctx context.Context, kind InputKind, input Input) (InputReceipt, bool, error) {
	q, err := inputRecord(kind, input)
	if err != nil {
		return InputReceipt{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return InputReceipt{}, false, ErrClosed
	}
	if old, exists, err := s.existingInputLocked(ctx, q); err != nil || exists {
		return old, false, err
	}
	if err := s.admitInput(q); err != nil {
		return InputReceipt{}, false, err
	}
	entries, err := s.appendLocked(ctx, Entry{Kind: EntryInputQueued, Input: &q})
	if err != nil {
		return InputReceipt{}, false, err
	}
	return InputReceipt{InputRecord: *entries[0].Input, AcceptedAt: entries[0].Time, State: InputPending}, true, nil
}
func (s *Session) Steer(ctx context.Context, input Input) (InputReceipt, error) {
	receipt, _, err := s.enqueue(ctx, InputSteer, input)
	return receipt, err
}
func (s *Session) FollowUp(ctx context.Context, input Input) (InputReceipt, error) {
	receipt, _, err := s.enqueue(ctx, InputFollowUp, input)
	return receipt, err
}
func (s *Session) InjectContext(ctx context.Context, input Input) (InputReceipt, error) {
	receipt, _, err := s.enqueue(ctx, InputContext, input)
	if err != nil {
		return receipt, err
	}
	// Re-admission also resumes accepted passive input after recovery. Maintenance
	// may summarize history, but never starts a driving model turn.
	for {
		s.mu.Lock()
		// A crashed generation already owns its input boundary. Keep newly
		// received observations queued until that generation has a durable result.
		pending := s.state.PendingInputs[InputContext] > 0 && s.state.CurrentAttemptID == ""
		s.mu.Unlock()
		if !pending {
			break
		}
		operation, finish, reserveErr := s.beginMaintenance(ctx)
		if reserveErr != nil {
			break
		}
		_, _, flushErr := s.prepareStep(operation, prepareOptions{passiveOnly: true, reason: "passive", skipCollect: true})
		finish()
		if flushErr != nil {
			if !errors.Is(flushErr, ErrBudget) && !errors.Is(flushErr, ErrIncompleteBatch) {
				return receipt, flushErr
			}
			break
		}
	}
	return s.LookupInput(ctx, input.ID)
}

const maxPassivePerBoundary = 64

func (s *Session) pendingRecordsLocked(ctx context.Context, kind InputKind, limit int) ([]Record, error) {
	if s.state.PendingInputs[kind] == 0 {
		return nil, nil
	}
	page, err := s.config.Store.Pending(ctx, s.id, PendingQuery{Kind: EntryInputQueued, InputKind: kind, Limit: limit, MaxBytes: 2 * s.config.MaxContextBytes})
	return page.Records, err
}
func (s *Session) selectedInputsLocked(ctx context.Context, passiveOnly, natural bool) ([]Record, error) {
	passive, err := s.pendingRecordsLocked(ctx, InputContext, maxPassivePerBoundary)
	if err != nil || passiveOnly {
		return passive, err
	}
	driving, err := s.pendingRecordsLocked(ctx, InputSteer, s.config.MaxSteeringPerTurn)
	if err != nil {
		return nil, err
	}
	if len(driving) == 0 && natural {
		driving, err = s.pendingRecordsLocked(ctx, InputFollowUp, 1)
	}
	return append(passive, driving...), err
}
func contextEntries(contexts []Message, runID, inputID string) []Entry {
	out := make([]Entry, 0, len(contexts))
	for _, m := range contexts {
		owned := m
		out = append(out, Entry{Kind: EntryContext, RunID: runID, InputID: inputID, Message: &owned})
	}
	return out
}
func (s *Session) HasQueuedInputs(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, ErrClosed
	}
	return s.state.PendingInputs[InputSteer]+s.state.PendingInputs[InputFollowUp] > 0, nil
}
func (s *Session) PeekQueuedInputs(ctx context.Context) ([]InputRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queuedInputsLocked(ctx)
}
func (s *Session) queuedInputsLocked(ctx context.Context) ([]InputRecord, error) {
	if s.closed {
		return nil, ErrClosed
	}
	var rows []Record
	for _, kind := range []InputKind{InputSteer, InputFollowUp} {
		cursor := ""
		for {
			page, err := s.config.Store.Pending(ctx, s.id, PendingQuery{Kind: EntryInputQueued, InputKind: kind, Cursor: cursor, MaxBytes: 2 * s.config.MaxContextBytes})
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
	out := make([]InputRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, *row.Entry.Input)
	}
	return out, nil
}

// pendingInputLocked selects the current queue item while holding the same lock
// used by consumption. A UI action cannot revive an already consumed input.
func (s *Session) pendingInputLocked(ctx context.Context, id string) (InputRecord, error) {
	if s.closed {
		return InputRecord{}, ErrClosed
	}
	receipt, err := s.config.Store.LookupInput(ctx, s.id, id)
	if err != nil {
		return InputRecord{}, err
	}
	if receipt.State != InputPending || receipt.Kind == InputContext {
		return InputRecord{}, ErrNotFound
	}
	return receipt.InputRecord, nil
}

// ClearQueuedInput removes one driving input without affecting the other items.
func (s *Session) ClearQueuedInput(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	input, err := s.pendingInputLocked(ctx, id)
	if err != nil {
		return err
	}
	_, err = s.appendLocked(ctx, Entry{Kind: EntryInputCleared, Input: &input})
	return err
}

// UpdateQueuedInput edits the existing input and/or changes when it is applied.
// Its ID, acceptance digest, attached context and frozen attachments survive.
// Clearing and requeuing share a commit; no intermediate queue is observable.
func (s *Session) UpdateQueuedInput(ctx context.Context, id string, kind InputKind, message *Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	queued, err := s.queuedInputsLocked(ctx)
	if err != nil {
		return err
	}
	next := append([]InputRecord(nil), queued...)
	for i, input := range queued {
		if input.ID != id {
			continue
		}
		if kind != "" {
			next[i].Kind = kind
		}
		if message != nil {
			next[i].Message = message
		}
		if err = s.admitInput(next[i]); err != nil {
			return err
		}
		if sameJSON(input, next[i]) {
			return nil
		}
		return s.requeueLocked(ctx, queued, next)
	}
	return ErrNotFound
}

// MoveQueuedInput moves one item before another (empty beforeID means the end).
// The order is resolved from the current queue, so later arrivals are retained.
func (s *Session) MoveQueuedInput(ctx context.Context, id, beforeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	queued, err := s.queuedInputsLocked(ctx)
	if err != nil {
		return err
	}
	index, target := -1, len(queued)
	for i, input := range queued {
		if input.ID == id {
			index = i
		}
		if input.ID == beforeID {
			target = i
		}
	}
	if index < 0 || beforeID != "" && target == len(queued) {
		return ErrNotFound
	}
	if index == target || index+1 == target {
		return nil
	}
	previous := append([]InputRecord(nil), queued...)
	item := queued[index]
	queued = append(queued[:index], queued[index+1:]...)
	if index < target {
		target--
	}
	queued = append(queued, InputRecord{})
	copy(queued[target+1:], queued[target:])
	queued[target] = item
	return s.requeueLocked(ctx, previous, queued)
}

// Replacement uses existing input identities, not a parallel queue or new
// submissions. Every removal and reinsertion is in one durable commit.
func (s *Session) requeueLocked(ctx context.Context, previous, next []InputRecord) error {
	drafts := make([]Entry, 0, len(previous)+len(next))
	for _, input := range previous {
		drafts = append(drafts, Entry{Kind: EntryInputCleared, Input: &input})
	}
	for _, input := range next {
		drafts = append(drafts, Entry{Kind: EntryInputRequeued, Input: &input})
	}
	_, err := s.appendLocked(ctx, drafts...)
	return err
}

func (s *Session) ClearSteering(ctx context.Context) error  { return s.clearInputs(ctx, InputSteer) }
func (s *Session) ClearFollowUps(ctx context.Context) error { return s.clearInputs(ctx, InputFollowUp) }
func (s *Session) ClearQueuedInputs(ctx context.Context) error {
	return s.clearInputs(ctx, InputSteer, InputFollowUp)
}
func (s *Session) clearInputs(ctx context.Context, kinds ...InputKind) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	pending := false
	for _, kind := range kinds {
		pending = pending || s.state.PendingInputs[kind] > 0
	}
	if !pending {
		return ctx.Err()
	}
	_, err := s.appendLocked(ctx, Entry{Kind: EntryInputCleared, Clear: &InputClear{Kinds: kinds}})
	return err
}
