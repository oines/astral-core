package astral

import (
	"context"
	"errors"
)

type ForkMode uint8

const (
	ForkShared ForkMode = iota
	ForkDetached
)

type ForkOptions struct {
	ID   string
	Mode ForkMode
}

type HistoryReader struct {
	store     Store
	rolloutID string
}
type HistoryPage struct {
	Entries    []Entry
	NextCursor string
}

func (s *Session) History() HistoryReader {
	return HistoryReader{store: s.config.Store, rolloutID: s.rolloutID}
}
func (h HistoryReader) Get(ctx context.Context, id string) (Entry, error) {
	r, err := h.store.Get(ctx, h.rolloutID, id)
	return r.Entry, err
}
func (h HistoryReader) Scan(ctx context.Context, q Query) (HistoryPage, error) {
	p, err := h.store.Scan(ctx, h.rolloutID, q)
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

// Fork creates a new execution cursor at an inclusive, committed history node.
// The stable target ID is required for retries, including after a lost reply.
func (s *Session) Fork(ctx context.Context, id string, options ForkOptions) (*Session, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	request := ForkRequest{SourceID: s.id, SourceRolloutID: s.rolloutID, TargetID: options.ID, HeadID: id, Mode: options.Mode}
	var err error
	if err = ValidateFork(request); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	s.operations.Add(1)
	s.mu.Unlock()
	defer s.operations.Done()
	forkCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.lifetime, cancel)
	defer func() { stop(); cancel() }()
	// Check an existing creation before reading the source or allocating data.
	if receipt, lookupErr := s.config.Store.LookupCommit(forkCtx, options.ID, ForkCommitID); lookupErr == nil {
		digest, digestErr := request.Digest()
		if digestErr != nil {
			return nil, digestErr
		}
		if receipt.Digest != digest {
			return nil, ErrConflict
		}
		return OpenSession(forkCtx, s.config, options.ID)
	} else if !errors.Is(lookupErr, ErrNotFound) {
		return nil, lookupErr
	}
	record, err := s.config.Store.Get(forkCtx, s.rolloutID, id)
	if err != nil {
		return nil, err
	}
	seed, err := ForkState(request, record)
	if err != nil {
		return nil, err
	}
	// Admission is bounded by effective context, before any target is published.
	seed.RolloutID = s.rolloutID
	_, err = s.loadWindow(forkCtx, seed)
	if err != nil {
		return nil, err
	}
	_, forkErr := s.config.Store.Fork(forkCtx, request)
	if forkErr != nil {
		check, done := persistContext(forkCtx)
		receipt, lookupErr := s.config.Store.LookupCommit(check, options.ID, ForkCommitID)
		if lookupErr == nil {
			digest, digestErr := request.Digest()
			if digestErr != nil || receipt.Digest != digest {
				done()
				return nil, ErrConflict
			}
			_, lookupErr = s.config.Store.LoadState(check, options.ID)
		}
		done()
		if lookupErr != nil {
			return nil, forkErr
		}
	}
	return OpenSession(forkCtx, s.config, options.ID)
}
func (s *Session) pathLocked() []Entry { return s.window }
func (s *Session) configurationLocked() Configuration {
	if i, ok := s.windowIndex[s.state.Branch.ConfigurationID]; ok && s.window[i].Configuration != nil {
		return *s.window[i].Configuration
	}
	panic("missing effective configuration")
}
