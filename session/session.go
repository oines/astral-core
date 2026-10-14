package session

import (
	"context"
	"sync"

	protocol "github.com/oines/astral-core"
	targetenv "github.com/oines/astral-core/environment"
)

// SessionOptions identifies a new session and its optional initial environment.
type SessionOptions struct {
	ID          string
	Environment *targetenv.Snapshot
}

// Session owns one recoverable execution cursor over its Rollout history. The
// session_*.go files implement its storage, history, input, approval and recovery.
type Session struct {
	mu                 sync.Mutex
	agent              *Manager
	rolloutID          string
	id                 string
	state              RuntimeState
	window             []Entry
	windowIndex        map[string]int
	windowBytes        int
	lifetime           context.Context
	stopLifetime       context.CancelFunc
	operations         sync.WaitGroup
	core               *protocol.Agent
	running            bool
	maintenance        bool
	maintenanceCancel  context.CancelFunc
	maintenanceContext context.Context
	maintenanceStop    func() bool
	maintenanceDone    chan struct{}
	closed             bool
	needsEnvironment   bool
	changed            chan struct{}
}

func newSession(a *Manager, id string, state RuntimeState) (*Session, error) {
	lifetime, cancel := context.WithCancel(context.Background())
	s := &Session{agent: a, id: id, rolloutID: state.RolloutID, state: state, windowIndex: map[string]int{}, changed: make(chan struct{}), lifetime: lifetime, stopLifetime: cancel}
	if err := s.initCore(); err != nil {
		cancel()
		return nil, err
	}
	return s, nil
}

func (s *Session) ID() string        { return s.id }
func (s *Session) RolloutID() string { return s.rolloutID }

func (s *Session) busyLocked() bool { return s.running || s.maintenance }

// A maintenance operation reserves the session without holding a lock across
// host callbacks. Close and Cancel can cancel and join the reservation.
func (s *Session) beginMaintenance(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	s.agent.mu.Lock()
	defer s.agent.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent.closed || s.closed {
		return nil, nil, protocol.ErrClosed
	}
	if s.busyLocked() {
		return nil, nil, protocol.ErrBusy
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.maintenance = true
	s.maintenanceCancel = cancel
	s.maintenanceContext = ctx
	s.maintenanceStop = context.AfterFunc(s.lifetime, cancel)
	s.maintenanceDone = done
	return ctx, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.maintenanceDone == done {
			s.endMaintenanceLocked()
		}
	}, nil
}

func (s *Session) endMaintenanceLocked() {
	if s.maintenanceCancel != nil {
		s.maintenanceStop()
		s.maintenanceCancel()
		close(s.maintenanceDone)
	}
	s.maintenance = false
	s.maintenanceCancel = nil
	s.maintenanceContext = nil
	s.maintenanceStop = nil
	s.maintenanceDone = nil
}

func (s *Session) Cancel() {
	s.mu.Lock()
	if s.maintenanceCancel != nil {
		s.maintenanceCancel()
	}
	s.mu.Unlock()
	s.core.Cancel()
}
func (s *Session) Close(ctx context.Context) error {
	s.stopLifetime()
	s.mu.Lock()
	s.closed = true
	var done <-chan struct{}
	if s.maintenanceCancel != nil {
		s.maintenanceCancel()
		done = s.maintenanceDone
	}
	s.mu.Unlock()
	err := s.core.Close(ctx)
	if err != nil {
		return err
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	joined := make(chan struct{})
	go func() { s.operations.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	s.window = nil
	s.windowIndex = nil
	s.windowBytes = 0
	s.state = RuntimeState{}
	s.mu.Unlock()
	s.agent.mu.Lock()
	if s.agent.sessions[s.id] == s {
		delete(s.agent.sessions, s.id)
	}
	s.agent.mu.Unlock()
	return err
}
