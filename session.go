package astral

import (
	"context"
	"sync"
)

// SessionOptions identifies a new session and its optional initial environment.
type SessionOptions struct {
	ID          string
	Environment *EnvironmentSnapshot
}

// Session owns a durable history tree and at most one active execution. The
// session_*.go files implement its storage, history, input, approval and recovery.
type Session struct {
	mu                sync.Mutex
	agent             *Agent
	id                string
	rollout           Rollout
	active            *Run
	maintenance       bool
	maintenanceCancel context.CancelFunc
	maintenanceDone   chan struct{}
	closed            bool
	needsEnvironment  bool
	changed           chan struct{}
	subscribers       map[*Subscription]struct{}
	sequence          uint64
}

func newSession(a *Agent, id string, r Rollout) *Session {
	return &Session{agent: a, id: id, rollout: r, changed: make(chan struct{}), subscribers: map[*Subscription]struct{}{}}
}

func (s *Session) ID() string { return s.id }

func (s *Session) busyLocked() bool { return s.active != nil || s.maintenance }

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
	if s.agent.closed {
		return nil, nil, ErrClosed
	}
	if s.busyLocked() {
		return nil, nil, ErrBusy
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.maintenance = true
	s.maintenanceCancel = cancel
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
		s.maintenanceCancel()
		close(s.maintenanceDone)
	}
	s.maintenance = false
	s.maintenanceCancel = nil
	s.maintenanceDone = nil
}

func (s *Session) Cancel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil {
		s.active.cancel()
	}
	if s.maintenanceCancel != nil {
		s.maintenanceCancel()
	}
}
