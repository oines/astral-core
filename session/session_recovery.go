package session

import (
	"context"
	"errors"
	"sort"

	protocol "github.com/oines/astral-core"
)

// UnresolvedTools returns durable terminal markers that require a host decision.
func (s *Session) UnresolvedTools() []ToolRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.unresolvedLocked())
}

func (s *Session) unresolvedLocked() []ToolRecord {
	var out []ToolRecord
	for _, r := range s.state.Branch.Unresolved {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Invocation.ExecutionID < out[j].Invocation.ExecutionID })
	return out
}

// ResolveTool appends the host's reconciled result. OutcomeUnknown is permitted
// as an explicit acknowledgement to continue while preserving uncertainty.
// It never executes the tool. Historical entries remain readable unchanged.
func (s *Session) ResolveTool(ctx context.Context, executionID string, result protocol.ToolResult) error {
	owned, err := copyJSON(result)
	if err != nil {
		return err
	}
	result = owned
	if !validOutcome(result.Outcome) {
		return fmtInvalid("explicit outcome required")
	}
	if result.Context != nil {
		return fmtInvalid("refresh environment separately while reconciling a tool")
	}
	result = normalizeResult(result)
	if err := validateToolResult(result); err != nil {
		return err
	}
	ctx, finish, err := s.beginMaintenance(ctx)
	if err != nil {
		return err
	}
	defer finish()
	s.mu.Lock()
	unresolved := clone(s.unresolvedLocked())
	s.mu.Unlock()
	for _, record := range unresolved {
		if record.Invocation.ExecutionID == executionID {
			result = normalizeResult(result)
			bounded, budgetErr := s.boundResult(ctx, record.Invocation, result)
			result = bounded
			record.Result = &result
			record.NeedsResolution = false
			_, err := s.append(ctx, Entry{Kind: EntryToolResolution, Tool: &record})
			return errors.Join(budgetErr, err)
		}
	}
	return ErrNotFound
}

// recoverInterrupted runs before environment rediscovery and before new effects.
// Prepared/authorized calls may still execute; started calls are never replayed.
func (s *Session) recoverInterrupted(ctx context.Context) error {
	s.mu.Lock()
	var path []Entry
	if last := s.lastMessageLocked(); last != nil {
		path = []Entry{clone(*last)}
	}
	s.mu.Unlock()
	var unknown error
	for _, assistant := range path {
		if assistant.Kind != EntryMessage || len(assistant.Message.ToolCalls) == 0 {
			continue
		}
		for _, call := range assistant.Message.ToolCalls {
			s.mu.Lock()
			state := s.toolStateLocked(assistant.ID, call.ID)
			s.mu.Unlock()
			if state.result != nil || state.started == nil {
				continue
			}
			inv := clone(state.started.Tool.Invocation)
			result := toolFailure("outcome_unknown", "Execution started before interruption; the operation may have partially or fully completed. The host must reconcile its effects. Do not automatically repeat it.", protocol.OutcomeUnknown)
			needsResolution := true
			if state.completed != nil {
				fact := state.completed.Tool.Result
				result = toolFailure("result_unavailable", "Execution finished before interruption, but its final output was not committed. Do not repeat the operation to retrieve output.", fact.Outcome)
				result.Artifact = fact.Artifact
				result.Context = fact.Context
				needsResolution = fact.Outcome == protocol.OutcomeUnknown
			}
			_, err := s.commitToolResult(ctx, inv, result, needsResolution)
			if result.Context != nil {
				s.mu.Lock()
				s.needsEnvironment = true
				s.mu.Unlock()
			}
			if err != nil {
				if errors.Is(err, protocol.ErrOutcomeUnknown) {
					unknown = err
				} else {
					return err
				}
			}
		}
	}
	return unknown
}

// toolState includes only execution facts belonging to assistant entries on the
// selected branch. A decision from a sibling branch must never authorize a call.
type toolState struct{ prepared, authorized, started, completed, result *Entry }

func (s *Session) toolStateLocked(assistantID, callID string) toolState {
	if assistantID != s.state.Branch.LastMessageID {
		return toolState{}
	}
	v := s.state.Tools[callID]
	if v.Result == nil {
		if i, ok := s.windowIndex[s.state.Branch.Results[callID]]; ok {
			e := s.window[i]
			if e.Tool != nil && e.Tool.Invocation.AssistantID == assistantID && e.Tool.Invocation.Call.ID == callID {
				v.Result = &e
			}
		}
	}
	return toolState{prepared: v.Prepared, authorized: v.Authorized, started: v.Started, completed: v.Completed, result: v.Result}
}
