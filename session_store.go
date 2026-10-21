package astral

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"
)

func (s *Session) appendLocked(ctx context.Context, drafts ...Entry) ([]Entry, error) {
	entries, err := s.prepareEntriesLocked(drafts)
	if err != nil {
		return nil, err
	}
	commit := Commit{ID: newID(), ExpectedVersion: s.state.Version, Entries: entries}
	digest, err := commit.Digest()
	if err != nil {
		return nil, err
	}
	receipt, commitErr := s.config.Store.Commit(ctx, s.id, commit)
	if commitErr != nil {
		check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		receipt, err = s.config.Store.LookupCommit(check, s.id, commit.ID)
		cancel()
		if err != nil {
			return nil, commitErr
		}
	}
	if receipt.CommitID != commit.ID || receipt.Digest != digest || receipt.Version != s.state.Version+1 {
		return nil, fmt.Errorf("%w: commit receipt mismatch", ErrCorrupt)
	}
	if receipt.CurrentVersion != s.state.Version+1 {
		return nil, fmt.Errorf("%w: another owner advanced this session", ErrConflict)
	}
	confirmed, err := commit.Confirmed(receipt)
	if err != nil {
		return nil, err
	}
	next, _, err := Reduce(s.state, confirmed)
	if err != nil {
		return nil, err
	}
	entries = confirmed.Entries
	s.state = next
	if s.rolloutID == "" {
		s.rolloutID = next.RolloutID
	}
	compacting := false
	for _, e := range entries {
		if e.IsHistory() {
			s.windowIndex[e.ID] = len(s.window)
			s.window = append(s.window, e)
		}
		compacting = compacting || e.Kind == EntryCompaction
	}
	if compacting {
		s.pruneWindow()
	}
	// Store-assigned sequence digits change serialized sizes.
	s.measureWindow(0, true)
	close(s.changed)
	s.changed = make(chan struct{})
	for _, e := range entries {
		s.publishLocked(Event{Type: EventCommitted, EntryID: e.ID, RunID: e.RunID, TurnID: e.TurnID})
		if e.Kind == EntryContext {
			s.publishLocked(Event{Type: EventContextApplied, EntryID: e.ID, InputID: e.InputID, RunID: e.RunID, Message: e.Message})
		}
		if e.Kind == EntryToolResult || e.Kind == EntryToolControl || e.Kind == EntryToolUnresolved {
			s.publishLocked(Event{Type: EventToolEnd, EntryID: e.ID, RunID: e.RunID, TurnID: e.TurnID, ToolCallID: e.Tool.Invocation.Call.ID, ExecutionID: e.Tool.Invocation.ExecutionID})
		}
	}
	return clone(entries), nil
}

// prepareEntriesLocked admits a draft without mutating state or writing it.
// Steering uses the same envelope sizing as the eventual atomic append.
func (s *Session) prepareEntriesLocked(drafts []Entry) ([]Entry, error) {
	if len(drafts) == 0 {
		return nil, fmtInvalid("empty commit")
	}
	entries, err := copyJSON(drafts)
	if err != nil {
		return nil, err
	}
	head := s.state.Branch.HeadID
	for i := range entries {
		e := &entries[i]
		if e.ID == "" {
			e.ID = newID()
		}
		if e.Time.IsZero() {
			e.Time = time.Now().UTC()
		}
		e.Sequence = 0
		e.SessionID = s.id
		e.Version = s.state.Version + 1
		if e.IsHistory() {
			e.ParentID = head
			head = e.ID
		} else {
			e.ParentID = ""
		}
	}
	// Context memory is independent of archived history. Reject oversized new
	// messages before model-declared tools can execute; bounded result admission
	// handles delivery after an actual tool execution separately.
	added := 0
	compacting := false
	for _, e := range entries {
		if e.IsHistory() {
			e.Sequence = math.MaxInt64 // conservative size before the Store assigns order
			b, _ := json.Marshal(e)
			added += len(b)
		}
		compacting = compacting || e.Kind == EntryCompaction
	}
	if !compacting && s.windowBytes+added > 2*s.config.MaxContextBytes {
		return nil, &BudgetError{Input: s.windowBytes + added, Window: 2 * s.config.MaxContextBytes}
	}
	return entries, nil
}
func (s *Session) append(ctx context.Context, entries ...Entry) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(ctx, entries...)
}
func (s *Session) measureWindow(added int, recount bool) {
	if !recount {
		s.windowBytes += added
		return
	}
	s.windowBytes = 0
	for _, e := range s.window {
		b, _ := json.Marshal(e)
		s.windowBytes += len(b)
	}
}
func (s *Session) setWindow(entries []Entry) {
	s.window = entries
	s.windowIndex = map[string]int{}
	for i, e := range entries {
		s.windowIndex[e.ID] = i
	}
	s.measureWindow(0, true)
}
func (s *Session) pruneWindow() {
	p := s.state.Branch
	if p.ThroughID == "" {
		return
	}
	through, ok := s.windowIndex[p.ThroughID]
	if !ok {
		panic("missing committed context boundary")
	}
	keep := map[string]bool{p.ConfigurationID: true, p.CompactionID: true, p.ThroughID: true, p.LastMessageID: true, p.BatchConfigurationID: true}
	out := make([]Entry, 0, len(s.window)-through+len(keep))
	for _, id := range p.ToolEntries {
		keep[id] = true
	}
	for i, e := range s.window {
		if i > through || keep[e.ID] {
			out = append(out, e)
		}
	}
	s.setWindow(out)
}
func (s *Session) loadWindow(ctx context.Context, state RuntimeState) ([]Entry, error) {
	p := state.Branch
	if p.HeadID == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	var entries []Entry
	size := 0
	cursor := ""
	for {
		page, err := s.config.Store.Scan(ctx, state.RolloutID, Query{PathHead: p.HeadID, StopID: p.ThroughID, Cursor: cursor, Limit: DefaultPageSize, MaxBytes: 2 * s.config.MaxContextBytes})
		if err != nil {
			return nil, err
		}
		for _, r := range page.Records {
			b, _ := json.Marshal(r.Entry)
			size += len(b)
			if size > 2*s.config.MaxContextBytes {
				return nil, ErrBudget
			}
			entries = append(entries, r.Entry)
			seen[r.Entry.ID] = true
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	references := []string{p.ConfigurationID, p.CompactionID, p.ThroughID, p.LastMessageID, p.BatchConfigurationID}
	for _, id := range p.ToolEntries {
		references = append(references, id)
	}
	for _, id := range references {
		if id == "" || seen[id] {
			continue
		}
		r, err := s.config.Store.Get(ctx, state.RolloutID, id)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(r.Entry)
		size += len(b)
		if size > 2*s.config.MaxContextBytes {
			return nil, ErrBudget
		}
		entries = append(entries, r.Entry)
		seen[id] = true
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Sequence < entries[j].Sequence })
	return entries, nil
}

// State returns the exact bounded checkpoint, not a model-generated summary.
func (s *Session) State() RuntimeState { s.mu.Lock(); defer s.mu.Unlock(); return clone(s.state) }

// Statistics reports retained working data without reading archived history.
type Statistics struct {
	ContextEntries int
	ContextBytes   int
	ToolCalls      int
	Approvals      int
}

func (s *Session) Stats() Statistics {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Statistics{ContextEntries: len(s.window), ContextBytes: s.windowBytes, ToolCalls: len(s.state.Tools), Approvals: len(s.state.Approvals)}
}

// Reload requires the host to re-establish exclusive execution ownership first.
func (s *Session) Reload(ctx context.Context) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.busyLocked() {
		return ErrBusy
	}
	state, err := s.config.Store.LoadState(ctx, s.id)
	if err != nil {
		return err
	}
	window, err := s.loadWindow(ctx, state)
	if err != nil {
		return err
	}
	s.state = state
	s.setWindow(window)
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}
