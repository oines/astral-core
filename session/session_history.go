package session

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	protocol "github.com/oines/astral-core"
	targetenv "github.com/oines/astral-core/environment"
)

type ForkMode uint8

const (
	ForkShared ForkMode = iota
	ForkDetached
)

type ForkOptions struct {
	ID          string
	Mode        ForkMode
	Environment *targetenv.Snapshot
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
	return HistoryReader{store: s.agent.config.Store, rolloutID: s.rolloutID}
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
	if err := s.agent.checkOpen(); err != nil {
		return nil, err
	}
	request := ForkRequest{SourceID: s.id, SourceRolloutID: s.rolloutID, TargetID: options.ID, HeadID: id, Mode: options.Mode}
	var err error
	request.Environment, err = copyJSON(options.Environment)
	if err != nil {
		return nil, err
	}
	if err = ValidateFork(request); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, protocol.ErrClosed
	}
	s.operations.Add(1)
	s.mu.Unlock()
	defer s.operations.Done()
	forkCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.lifetime, cancel)
	defer func() { stop(); cancel() }()
	// Check an existing creation before reading the source or allocating data.
	if receipt, lookupErr := s.agent.config.Store.LookupCommit(forkCtx, options.ID, ForkCommitID); lookupErr == nil {
		digest, digestErr := request.Digest()
		if digestErr != nil {
			return nil, digestErr
		}
		if receipt.Digest != digest {
			return nil, ErrConflict
		}
		return s.agent.OpenSession(forkCtx, options.ID)
	} else if !errors.Is(lookupErr, ErrNotFound) {
		return nil, lookupErr
	}
	record, err := s.agent.config.Store.Get(forkCtx, s.rolloutID, id)
	if err != nil {
		return nil, err
	}
	seed, err := ForkState(request, record)
	if err != nil {
		return nil, err
	}
	// Admission is bounded by effective context, before any target is published.
	seed.RolloutID = s.rolloutID
	window, err := s.loadWindow(forkCtx, seed)
	if err != nil {
		return nil, err
	}
	if request.Environment != nil {
		// The Store will assign the actual sequence; reserve its maximum width for
		// admission so an explicit environment cannot publish an oversized child.
		draft := Entry{ID: newID(), SessionID: request.TargetID, Sequence: math.MaxInt64, Version: 1, ParentID: id, Time: time.Now().UTC(), Kind: EntryEnvironment, Environment: request.Environment}
		encoded, err := json.Marshal(draft)
		if err != nil {
			return nil, err
		}
		size := len(encoded) + 16 // reserve timestamp fractional-width differences
		for _, entry := range window {
			raw, err := json.Marshal(entry)
			if err != nil {
				return nil, err
			}
			size += len(raw)
		}
		if size > 2*s.agent.config.MaxContextBytes {
			return nil, &BudgetError{Input: size, Window: 2 * s.agent.config.MaxContextBytes}
		}
	}
	state, forkErr := s.agent.config.Store.Fork(forkCtx, request)
	if forkErr != nil {
		check, done := persistContext(forkCtx)
		receipt, lookupErr := s.agent.config.Store.LookupCommit(check, options.ID, ForkCommitID)
		if lookupErr == nil {
			digest, digestErr := request.Digest()
			if digestErr != nil || receipt.Digest != digest {
				done()
				return nil, ErrConflict
			}
			state, lookupErr = s.agent.config.Store.LoadState(check, options.ID)
		}
		done()
		if lookupErr != nil {
			return nil, forkErr
		}
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
