package astral

import (
	"context"
	"encoding/json"
)

type ApprovalRequest struct {
	Prompt  string          `json:"prompt"`
	Details json.RawMessage `json:"details,omitempty"`
}

type ApprovalDecision struct {
	Approved bool   `json:"approved"`
	Reason   string `json:"reason,omitempty"`
}

type PendingApproval struct {
	ID         string
	Invocation Invocation
	Request    ApprovalRequest
}

// AwaitApproval may be used in BeforeToolCall. Re-entering that hook after a
// restart returns the persisted decision for the same invocation. Hook code
// outside this gate must be safe to call again. One approval gate per invocation.
func (i Invocation) AwaitApproval(ctx context.Context, request ApprovalRequest) (ApprovalDecision, error) {
	if i.awaitApproval == nil {
		return ApprovalDecision{}, fmtInvalid("approval is only available in BeforeToolCall")
	}
	return i.awaitApproval(ctx, request)
}

func (s *Session) PendingApprovals() []PendingApproval {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.pendingApprovalsLocked())
}

func (s *Session) pendingApprovalsLocked() []PendingApproval {
	decided := map[string]bool{}
	for _, e := range s.rollout.Entries {
		if e.Kind == EntryApprovalDecided {
			decided[e.Approval.ID] = true
		}
	}
	active := map[string]Entry{}
	for _, e := range s.pathLocked() {
		if e.Kind == EntryMessage && e.Message.Role == RoleAssistant {
			active[e.ID] = e
		}
	}
	var out []PendingApproval
	for _, e := range s.rollout.Entries {
		if e.Kind != EntryApprovalRequested || decided[e.Approval.ID] {
			continue
		}
		for _, p := range s.rollout.Entries {
			if p.Kind != EntryToolPrepared || p.Tool.Invocation.ExecutionID != e.Approval.ExecutionID {
				continue
			}
			inv := p.Tool.Invocation
			if _, ok := active[inv.AssistantID]; !ok {
				continue
			}
			if s.toolStateLocked(inv.AssistantID, inv.Call.ID).result != nil {
				continue
			}
			out = append(out, PendingApproval{ID: e.Approval.ID, Invocation: inv, Request: e.Approval.Request})
		}
	}
	return out
}

func (s *Session) DecideApproval(ctx context.Context, id string, decision ApprovalDecision) error {
	if err := s.agent.checkOpen(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	for _, e := range s.rollout.Entries {
		if e.Kind == EntryApprovalDecided && e.Approval.ID == id {
			if sameJSON(*e.Approval.Decision, decision) {
				return nil
			}
			return ErrAlreadyResolved
		}
	}
	for _, p := range s.pendingApprovalsLocked() {
		if p.ID == id {
			_, err := s.appendLocked(ctx, Entry{Kind: EntryApprovalDecided, Approval: &ApprovalRecord{ID: id, ExecutionID: p.Invocation.ExecutionID, Request: p.Request, Decision: &decision}})
			return err
		}
	}
	return ErrNotFound
}

func (s *Session) awaitApproval(ctx context.Context, inv Invocation, request ApprovalRequest) (ApprovalDecision, error) {
	for {
		if err := ctx.Err(); err != nil {
			return ApprovalDecision{}, err
		}
		s.mu.Lock()
		id := ""
		for _, e := range s.rollout.Entries {
			if e.Approval != nil && e.Approval.ExecutionID == inv.ExecutionID {
				if e.Kind == EntryApprovalDecided {
					decision := *e.Approval.Decision
					s.mu.Unlock()
					return decision, nil
				}
				if e.Kind == EntryApprovalRequested {
					id = e.Approval.ID
					if !sameJSON(e.Approval.Request, request) {
						s.mu.Unlock()
						return ApprovalDecision{}, fmtInvalid("approval request changed after persistence")
					}
				}
			}
		}
		if id == "" {
			id = newID()
			_, err := s.appendLocked(ctx, Entry{Kind: EntryApprovalRequested, RunID: inv.RunID, TurnID: inv.TurnID, Approval: &ApprovalRecord{ID: id, ExecutionID: inv.ExecutionID, Request: request}})
			if err != nil {
				s.mu.Unlock()
				return ApprovalDecision{}, err
			}
			s.publishLocked(Event{Type: EventApproval, ApprovalID: id, RunID: inv.RunID, TurnID: inv.TurnID, ExecutionID: inv.ExecutionID, ToolCallID: inv.Call.ID})
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ApprovalDecision{}, ctx.Err()
		case <-changed:
		}
	}
}
