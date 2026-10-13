package astral

import (
	"context"
	"errors"
)

// UnresolvedTools returns durable terminal markers that require a host decision.
func (s *Session) UnresolvedTools() []ToolRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.unresolvedLocked())
}

func (s *Session) unresolvedLocked() []ToolRecord {
	latest := map[string]ToolRecord{}
	var order []string
	for _, e := range s.pathLocked() {
		if e.Kind == EntryToolResult || e.Kind == EntryToolResolution {
			id := e.Tool.Invocation.ExecutionID
			if _, ok := latest[id]; !ok {
				order = append(order, id)
			}
			latest[id] = *e.Tool
		}
	}
	var out []ToolRecord
	for _, id := range order {
		if latest[id].NeedsResolution {
			out = append(out, latest[id])
		}
	}
	return out
}

// ResolveTool appends the host's reconciled result. OutcomeUnknown is permitted
// as an explicit acknowledgement to continue while preserving uncertainty.
// It never executes the tool. Historical entries remain readable unchanged.
func (s *Session) ResolveTool(ctx context.Context, executionID string, result ToolResult) error {
	owned, err := copyJSON(result)
	if err != nil {
		return err
	}
	result = owned
	if !validOutcome(result.Outcome) {
		return fmtInvalid("explicit outcome required")
	}
	if result.Environment != nil {
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
	path := clone(s.pathLocked())
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
			result := toolFailure("outcome_unknown", "Execution started before interruption; the operation may have partially or fully completed. The host must reconcile its effects. Do not automatically repeat it.", OutcomeUnknown)
			needsResolution := true
			if state.completed != nil {
				fact := state.completed.Tool.Result
				result = toolFailure("result_unavailable", "Execution finished before interruption, but its final output was not committed. Do not repeat the operation to retrieve output.", fact.Outcome)
				result.Artifact = fact.Artifact
				result.Environment = fact.Environment
				needsResolution = fact.Outcome == OutcomeUnknown
			}
			_, err := s.commitToolResult(ctx, inv, result, needsResolution)
			if result.Environment != nil {
				s.mu.Lock()
				s.needsEnvironment = true
				s.mu.Unlock()
			}
			if err != nil {
				if errors.Is(err, ErrOutcomeUnknown) {
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
	var state toolState
	for _, e := range s.rollout.Entries {
		if e.Tool == nil || e.Tool.Invocation.AssistantID != assistantID || e.Tool.Invocation.Call.ID != callID {
			continue
		}
		v := e
		switch e.Kind {
		case EntryToolPrepared:
			state.prepared = &v
		case EntryToolAuthorized:
			state.authorized = &v
		case EntryToolStarted:
			state.started = &v
		case EntryToolCompleted:
			state.completed = &v
		}
	}
	for _, e := range s.pathLocked() {
		if (e.Kind == EntryToolResult || e.Kind == EntryToolResolution) && e.Tool.Invocation.AssistantID == assistantID && e.Tool.Invocation.Call.ID == callID {
			v := e
			state.result = &v
		}
	}
	return state
}
