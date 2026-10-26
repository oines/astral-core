package astral

import (
	"context"
	"encoding/json"
	"sort"
)

// ToolContext exposes one Session approval gate while retaining the Core invocation.
type ToolContext struct {
	Invocation
	awaitApproval func(context.Context, ApprovalRequest) (ApprovalDecision, error)
}
type BeforeToolCall func(context.Context, ToolContext) (ToolDecision, error)

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
func (i ToolContext) AwaitApproval(ctx context.Context, request ApprovalRequest) (ApprovalDecision, error) {
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
	var out []PendingApproval
	for execution, record := range s.state.Approvals {
		if record.Decision != nil {
			continue
		}
		for _, work := range s.state.Tools {
			if work.Prepared == nil || work.Prepared.Tool.Invocation.ExecutionID != execution || work.Result != nil || work.Control != nil || work.Unresolved != nil {
				continue
			}
			out = append(out, PendingApproval{ID: record.ID, Invocation: work.Prepared.Tool.Invocation, Request: record.Request})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (s *Session) DecideApproval(ctx context.Context, id string, decision ApprovalDecision) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	page, err := s.config.Store.Scan(ctx, s.rolloutID, Query{SessionID: s.id, Kind: EntryApprovalDecided, Reference: id, Reverse: true, Limit: 1})
	if err != nil {
		return err
	}
	if len(page.Records) > 0 {
		if sameJSON(*page.Records[0].Entry.Approval.Decision, decision) {
			return nil
		}
		return ErrAlreadyResolved
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
		record, exists := s.state.Approvals[inv.ExecutionID]
		if exists {
			if !sameJSON(record.Request, request) {
				s.mu.Unlock()
				return ApprovalDecision{}, fmtInvalid("approval request changed after persistence")
			}
			if record.Decision != nil {
				decision := *record.Decision
				s.mu.Unlock()
				return decision, nil
			}
		}
		if !exists {
			id := newID()
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
