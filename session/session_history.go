package session

import (
	"context"
	"time"

	protocol "github.com/oines/astral-core"
)

type ForkPosition string

const (
	ForkAt     ForkPosition = "at"
	ForkBefore ForkPosition = "before"
)

type HistoryReader struct {
	store     Store
	sessionID string
}
type HistoryPage struct {
	Entries    []Entry
	NextCursor string
}

func (s *Session) History() HistoryReader {
	return HistoryReader{store: s.agent.config.Store, sessionID: s.id}
}
func (h HistoryReader) Get(ctx context.Context, id string) (Entry, error) {
	r, err := h.store.Get(ctx, h.sessionID, id)
	return r.Entry, err
}
func (h HistoryReader) Scan(ctx context.Context, q Query) (HistoryPage, error) {
	p, err := h.store.Scan(ctx, h.sessionID, q)
	if err != nil {
		return HistoryPage{}, err
	}
	out := HistoryPage{NextCursor: p.Next, Entries: make([]Entry, len(p.Records))}
	for i, r := range p.Records {
		out.Entries[i] = r.Entry
	}
	return out, nil
}

// Path pages run from the selected head towards its ancestors. Cursor is opaque.
func (h HistoryReader) Path(ctx context.Context, head string, q Query) (HistoryPage, error) {
	q.PathHead = head
	return h.Scan(ctx, q)
}
func (h HistoryReader) Children(ctx context.Context, parent string, q Query) (HistoryPage, error) {
	q.ParentID = &parent
	return h.Scan(ctx, q)
}
func (s *Session) Head() string { s.mu.Lock(); defer s.mu.Unlock(); return s.state.Branch.HeadID }
func (s *Session) SelectHead(ctx context.Context, id string) error {
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
	if _, err := s.appendLocked(ctx, Entry{Kind: EntryHeadSelected, Head: &HeadRecord{EntryID: id}}); err != nil {
		return err
	}
	s.needsEnvironment = s.state.Branch.EnvironmentID != ""
	return nil
}
func (s *Session) Fork(ctx context.Context, id string, position ForkPosition, options SessionOptions) (*Session, error) {
	if err := s.agent.checkOpen(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, protocol.ErrClosed
	}
	version := s.state.Version
	running := s.running
	s.operations.Add(1)
	s.mu.Unlock()
	defer s.operations.Done()
	forkCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.lifetime, cancel)
	defer func() { stop(); cancel() }()
	record, err := s.agent.config.Store.Get(forkCtx, s.id, id)
	if err != nil {
		return nil, err
	}
	if record.Projection == nil || record.Entry.Version > version {
		return nil, ErrNotFound
	}
	head := id
	switch position {
	case ForkAt:
	case ForkBefore:
		if record.Entry.Kind != EntryMessage || record.Entry.Message.Role != protocol.RoleUser {
			return nil, fmtInvalid("ForkBefore requires a user entry")
		}
		head = record.Entry.ParentID
		record, err = s.agent.config.Store.Get(forkCtx, s.id, head)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmtInvalid("unknown fork position")
	}
	if running && !record.Projection.Complete() {
		return nil, protocol.ErrIncompleteBatch
	}
	if options.ID == "" {
		options.ID = newID()
	}
	created := Entry{ID: newID(), Sequence: 1, Version: 1, Time: time.Now().UTC(), Kind: EntryCreated, Created: &CreationRecord{FormatVersion: FormatVersion, SessionID: options.ID, ForkSessionID: s.id, ForkEntryID: id}}
	request := ForkRequest{SourceID: s.id, TargetID: options.ID, HeadID: head, SourceVersion: version, Creation: created, AllowIncomplete: !running}
	if options.Environment != nil {
		snapshot, err := copyJSON(options.Environment)
		if err != nil {
			return nil, err
		}
		request.Environment = &Entry{ID: newID(), ParentID: head, Sequence: record.Entry.Sequence + 1, Version: 2, Time: time.Now().UTC(), Kind: EntryEnvironment, Environment: snapshot}
	}
	state, err := s.agent.config.Store.Fork(forkCtx, request)
	if err != nil {
		return nil, err
	}
	return s.agent.adopt(forkCtx, options.ID, state, options.Environment == nil && state.Branch.EnvironmentID != "")
}
func (s *Session) pathLocked() []Entry { return s.window }
func (s *Session) configurationLocked() Configuration {
	if i, ok := s.windowIndex[s.state.Branch.ConfigurationID]; ok && s.window[i].Configuration != nil {
		return *s.window[i].Configuration
	}
	panic("missing effective configuration")
}
func (s *Session) boundaryRequiredLocked() bool { return s.state.BoundaryRequired }
