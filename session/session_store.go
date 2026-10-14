package session

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	protocol "github.com/oines/astral-core"
)

func (s *Session) appendLocked(ctx context.Context, drafts ...Entry) ([]Entry, error) {
	if len(drafts) == 0 {
		return nil, fmtInvalid("empty commit")
	}
	entries, err := copyJSON(drafts)
	if err != nil {
		return nil, err
	}
	head := s.state.Branch.HeadID
	sequence := s.state.Sequence
	var selection *Projection
	for i := range entries {
		e := &entries[i]
		if e.ID == "" {
			e.ID = newID()
		}
		if e.Time.IsZero() {
			e.Time = time.Now().UTC()
		}
		sequence++
		e.Sequence = sequence
		e.Version = s.state.Version + 1
		if e.IsHistory() {
			e.ParentID = head
			head = e.ID
		} else {
			e.ParentID = ""
		}
		if e.Kind == EntryHeadSelected {
			r, err := s.agent.config.Store.Get(ctx, s.id, e.Head.EntryID)
			if err != nil {
				return nil, err
			}
			if r.Projection == nil {
				return nil, ErrNotFound
			}
			selection = r.Projection
			head = e.Head.EntryID
		}
	}
	commit := Commit{ID: newID(), ExpectedVersion: s.state.Version, Entries: entries, Selection: selection}
	next, _, err := Reduce(s.state, commit)
	if err != nil {
		return nil, err
	}
	var selectedWindow []Entry
	if selection != nil {
		selectedWindow, err = s.loadWindow(ctx, next)
		if err != nil {
			return nil, err
		}
	}
	// Context memory is independent of archived history. Reject oversized new
	// messages before model-declared tools can execute; bounded result admission
	// handles delivery after an actual tool execution separately.
	added := 0
	compacting := false
	for _, e := range entries {
		if e.IsHistory() {
			b, _ := json.Marshal(e)
			added += len(b)
		}
		compacting = compacting || e.Kind == EntryCompaction
	}
	if !compacting && selection == nil && s.windowBytes+added > 2*s.agent.config.MaxContextBytes {
		return nil, &BudgetError{Input: s.windowBytes + added, Window: 2 * s.agent.config.MaxContextBytes}
	}
	digest, err := commit.Digest()
	if err != nil {
		return nil, err
	}
	receipt, commitErr := s.agent.config.Store.Commit(ctx, s.id, commit)
	if commitErr != nil {
		check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		receipt, err = s.agent.config.Store.LookupCommit(check, s.id, commit.ID)
		cancel()
		if err != nil {
			return nil, commitErr
		}
	}
	if receipt.CommitID != commit.ID || receipt.Digest != digest || receipt.Version != next.Version {
		return nil, fmt.Errorf("%w: commit receipt mismatch", ErrCorrupt)
	}
	if receipt.CurrentVersion != next.Version {
		return nil, fmt.Errorf("%w: another owner advanced this session", ErrConflict)
	}
	s.state = next
	if selection != nil {
		s.setWindow(selectedWindow)
	} else {
		for _, e := range entries {
			if e.IsHistory() {
				s.windowIndex[e.ID] = len(s.window)
				s.window = append(s.window, e)
			}
		}
		if compacting {
			s.pruneWindow()
		}
		s.measureWindow(added, compacting)
	}
	close(s.changed)
	s.changed = make(chan struct{})
	for _, e := range entries {
		s.publishLocked(protocol.Event{Type: protocol.EventCommitted, EntryID: e.ID, RunID: e.RunID, TurnID: e.TurnID})
	}
	return clone(entries), nil
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
	keep := map[string]bool{p.ConfigurationID: true, p.EnvironmentID: true, p.CompactionID: true, p.ThroughID: true, p.LastMessageID: true, p.BatchConfigurationID: true}
	out := make([]Entry, 0, len(s.window)-through+len(keep))
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
		page, err := s.agent.config.Store.Scan(ctx, s.id, Query{PathHead: p.HeadID, StopID: p.ThroughID, Cursor: cursor, Limit: DefaultPageSize, MaxBytes: 2 * s.agent.config.MaxContextBytes})
		if err != nil {
			return nil, err
		}
		for _, r := range page.Records {
			b, _ := json.Marshal(r.Entry)
			size += len(b)
			if size > 2*s.agent.config.MaxContextBytes {
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
	for _, id := range []string{p.ConfigurationID, p.EnvironmentID, p.CompactionID, p.ThroughID, p.LastMessageID, p.BatchConfigurationID} {
		if id == "" || seen[id] {
			continue
		}
		r, err := s.agent.config.Store.Get(ctx, s.id, id)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(r.Entry)
		size += len(b)
		if size > 2*s.agent.config.MaxContextBytes {
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
	if err := s.agent.checkOpen(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return protocol.ErrClosed
	}
	if s.busyLocked() {
		return protocol.ErrBusy
	}
	state, err := s.agent.config.Store.LoadState(ctx, s.id)
	if err != nil {
		return err
	}
	window, err := s.loadWindow(ctx, state)
	if err != nil {
		return err
	}
	s.state = state
	s.setWindow(window)
	s.needsEnvironment = state.Branch.EnvironmentID != ""
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}
