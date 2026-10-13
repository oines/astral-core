package astral

import "sync"

type EventType string

const (
	EventRunStart      EventType = "run_start"
	EventRunEnd        EventType = "run_end"
	EventTurnStart     EventType = "turn_start"
	EventTurnEnd       EventType = "turn_end"
	EventMessageStart  EventType = "message_start"
	EventMessageUpdate EventType = "message_update"
	EventMessageEnd    EventType = "message_end"
	EventAttemptFailed EventType = "attempt_failed"
	EventRetry         EventType = "retry"
	EventToolStart     EventType = "tool_execution_start"
	EventToolUpdate    EventType = "tool_execution_update"
	EventToolEnd       EventType = "tool_execution_end"
	EventCommitted     EventType = "entry_committed"
	EventApproval      EventType = "approval_requested"
)

type Event struct {
	Type        EventType     `json:"type"`
	Sequence    uint64        `json:"sequence"`
	SessionID   string        `json:"session_id"`
	RunID       string        `json:"run_id,omitempty"`
	TurnID      string        `json:"turn_id,omitempty"`
	AttemptID   string        `json:"attempt_id,omitempty"`
	EntryID     string        `json:"entry_id,omitempty"`
	ToolCallID  string        `json:"tool_call_id,omitempty"`
	ExecutionID string        `json:"execution_id,omitempty"`
	ApprovalID  string        `json:"approval_id,omitempty"`
	Model       *ModelEvent   `json:"model,omitempty"`
	Progress    *ToolProgress `json:"progress,omitempty"`
	Error       string        `json:"error,omitempty"`
}

// Subscription closes with ErrSlowSubscriber on overflow. Subscribe before
// reading Rollout, then deduplicate committed entries by ID. Delta events cannot
// be reconstructed from durable history and are never a correctness dependency.
type Subscription struct {
	C       <-chan Event
	channel chan Event
	session *Session
	mu      sync.Mutex
	err     error
	closed  bool
}

func (sub *Subscription) Err() error {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	return sub.err
}
func (sub *Subscription) Close() {
	s := sub.session
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeSubscriberLocked(sub, nil)
}
func (s *Session) Subscribe(buffer int) *Subscription {
	if buffer < 1 {
		buffer = 128
	}
	ch := make(chan Event, buffer)
	sub := &Subscription{C: ch, channel: ch, session: s}
	s.agent.mu.Lock()
	defer s.agent.mu.Unlock()
	s.mu.Lock()
	s.subscribers[sub] = struct{}{}
	if s.agent.closed {
		s.removeSubscriberLocked(sub, ErrClosed)
	}
	s.mu.Unlock()
	return sub
}
func (s *Session) removeSubscriberLocked(sub *Subscription, err error) {
	if _, ok := s.subscribers[sub]; !ok {
		return
	}
	delete(s.subscribers, sub)
	sub.mu.Lock()
	sub.err = err
	sub.closed = true
	close(sub.channel)
	sub.mu.Unlock()
}
func (s *Session) closeSubscribersLocked(err error) {
	for sub := range s.subscribers {
		s.removeSubscriberLocked(sub, err)
	}
}
func (s *Session) publishLocked(event Event) {
	s.sequence++
	event.Sequence = s.sequence
	event.SessionID = s.id
	for sub := range s.subscribers {
		select {
		case sub.channel <- clone(event):
		default:
			s.removeSubscriberLocked(sub, ErrSlowSubscriber)
		}
	}
}
func (s *Session) publish(event Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishLocked(event)
}
