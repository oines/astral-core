package astral

import (
	"sync"
	"time"
)

type EventType string

const (
	EventRunStart         EventType = "run_start"
	EventRunEnd           EventType = "run_end"
	EventTurnStart        EventType = "turn_start"
	EventTurnEnd          EventType = "turn_end"
	EventMessageStart     EventType = "message_start"
	EventMessageUpdate    EventType = "message_update"
	EventMessageEnd       EventType = "message_end"
	EventAttemptFailed    EventType = "attempt_failed"
	EventAttemptStart     EventType = "attempt_start"
	EventAttemptEnd       EventType = "attempt_end"
	EventContextApplied   EventType = "context_applied"
	EventRetry            EventType = "retry"
	EventToolStart        EventType = "tool_execution_start"
	EventToolUpdate       EventType = "tool_execution_update"
	EventToolEnd          EventType = "tool_execution_end"
	EventCommitted        EventType = "entry_committed"
	EventApproval         EventType = "approval_requested"
	EventMaintenanceStart EventType = "maintenance_start"
	EventMaintenanceEnd   EventType = "maintenance_end"
)

type Event struct {
	Attempt     *AttemptStatus `json:"attempt,omitempty"`
	Type        EventType      `json:"type"`
	Sequence    uint64         `json:"sequence"`
	SessionID   string         `json:"session_id"`
	RunID       string         `json:"run_id,omitempty"`
	TurnID      string         `json:"turn_id,omitempty"`
	AttemptID   string         `json:"attempt_id,omitempty"`
	EntryID     string         `json:"entry_id,omitempty"`
	ToolCallID  string         `json:"tool_call_id,omitempty"`
	ExecutionID string         `json:"execution_id,omitempty"`
	ApprovalID  string         `json:"approval_id,omitempty"`
	Model       *ModelEvent    `json:"model,omitempty"`
	Progress    *ToolProgress  `json:"progress,omitempty"`
	Error       string         `json:"error,omitempty"`
	Message     *Message       `json:"message,omitempty"`
	Status      string         `json:"status,omitempty"`
	EndReason   string         `json:"end_reason,omitempty"`
	InputID     string         `json:"input_id,omitempty"`
}

// Subscription closes on overflow; emitting never blocks the executor.
type Subscription struct {
	C       <-chan Event
	channel chan Event
	hub     *eventHub
	mu      sync.Mutex
	err     error
}

func (s *Subscription) Err() error { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
func (s *Subscription) Close()     { s.hub.mu.Lock(); defer s.hub.mu.Unlock(); s.hub.remove(s, nil) }

type eventHub struct {
	mu          sync.Mutex
	id          string
	sequence    uint64
	closed      bool
	subscribers map[*Subscription]struct{}
}

func (h *eventHub) init(id string) { h.id = id; h.subscribers = map[*Subscription]struct{}{} }
func (h *eventHub) subscribe(buffer int) *Subscription {
	if buffer < 1 {
		buffer = 128
	}
	ch := make(chan Event, buffer)
	s := &Subscription{C: ch, channel: ch, hub: h}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subscribers[s] = struct{}{}
	if h.closed {
		h.remove(s, ErrClosed)
	}
	return s
}
func (h *eventHub) remove(s *Subscription, err error) {
	if _, ok := h.subscribers[s]; !ok {
		return
	}
	delete(h.subscribers, s)
	s.mu.Lock()
	s.err = err
	close(s.channel)
	s.mu.Unlock()
}
func (h *eventHub) publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.sequence++
	e.Sequence = h.sequence
	e.SessionID = h.id
	for s := range h.subscribers {
		select {
		case s.channel <- clone(e):
		default:
			h.remove(s, ErrSlowSubscriber)
		}
	}
}
func (h *eventHub) close(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for s := range h.subscribers {
		h.remove(s, err)
	}
}

// AttemptStatus is operational state. It is never projected into model messages.
type AttemptStatus struct {
	RunID       string    `json:"run_id,omitempty"`
	TurnID      string    `json:"turn_id,omitempty"`
	AttemptID   string    `json:"attempt_id"`
	Purpose     string    `json:"purpose"`
	Attempt     int       `json:"attempt"`
	MaxAttempts int       `json:"max_attempts"`
	State       string    `json:"state"`
	NextRetryAt time.Time `json:"next_retry_at,omitempty"`
	Error       string    `json:"error,omitempty"`
}
type SessionSnapshot struct {
	EventSequence uint64            `json:"event_sequence"`
	State         RuntimeState      `json:"state"`
	Approvals     []PendingApproval `json:"approvals"`
	Unresolved    []ToolRecord      `json:"unresolved"`
	Stopping      bool              `json:"stopping"`
	Error         string            `json:"error,omitempty"`
	Running       bool              `json:"running"`
	Maintenance   bool              `json:"maintenance"`
	CanContinue   bool              `json:"can_continue"`
	RunID         string            `json:"run_id,omitempty"`
	Attempt       *AttemptStatus    `json:"attempt,omitempty"`
}

func (s *Session) Snapshot() SessionSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := SessionSnapshot{State: clone(s.state), Approvals: clone(s.pendingApprovalsLocked()), Unresolved: clone(s.unresolvedLocked()), Running: s.active != nil, Maintenance: s.maintenance, Attempt: clone(s.attempt), Error: s.operationError}
	if !s.busyLocked() {
		execution := s.executionStateLocked()
		snapshot.CanContinue = execution.Continuable(execution.HasQueuedInputs)
	}
	s.events.mu.Lock()
	snapshot.EventSequence = s.events.sequence
	s.events.mu.Unlock()
	snapshot.Stopping = s.active != nil && s.active.ctx.Err() != nil || s.maintenanceContext != nil && s.maintenanceContext.Err() != nil
	if s.active != nil {
		snapshot.RunID = s.active.id
	}
	return snapshot
}
