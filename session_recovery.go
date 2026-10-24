package astral

import (
	"context"
	"errors"
	"sort"
)

// UnresolvedTools returns durable execution facts that require a host decision.
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

// ResolveTool appends the host's reconciled result or completes an unresolved
// control. OutcomeUnknown is permitted in a Result as an explicit acknowledgement.
// It never executes the tool. Historical entries remain readable unchanged.
func (s *Session) ResolveTool(ctx context.Context, executionID string, execution ToolExecution) error {
	owned, err := copyJSON(execution)
	if err != nil {
		return err
	}
	execution = owned
	if err := execution.Validate(); err != nil {
		return err
	}
	var result ToolResult
	if execution.Result != nil {
		result = *execution.Result
		if !validOutcome(result.Outcome) {
			return fmtInvalid("explicit outcome required")
		}
		result = normalizeResult(result)
		if err := validateToolResult(result); err != nil {
			return err
		}
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
			if execution.EndRun != nil {
				s.mu.Lock()
				defer s.mu.Unlock()
				if record.Result != nil {
					return fmtInvalid("a committed ordinary result cannot become an EndRun control")
				}
				runID := record.Invocation.RunID
				if s.state.LastRun != nil {
					runID = s.state.LastRun.ID
				}
				entries, err := s.controlEntriesLocked(newID(), runID, record.Invocation, *execution.EndRun)
				if err != nil {
					return err
				}
				_, err = s.appendLocked(ctx, entries...)
				return err
			}
			result = normalizeResult(result)
			bounded, budgetErr := s.boundResult(ctx, record.Invocation, result)
			result = bounded
			record.Result = &result
			record.NeedsResolution = false
			_, err := s.append(ctx, Entry{Kind: EntryToolResolution, RunID: record.Invocation.RunID, TurnID: record.Invocation.TurnID, Tool: &record})
			return errors.Join(budgetErr, err)
		}
	}
	return ErrNotFound
}

// recoverInterrupted runs before new execution effects.
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
		if assistant.Kind != EntryMessage || assistant.Message.Failed() {
			continue
		}
		calls := assistant.Message.ToolCalls()
		for _, call := range calls {
			s.mu.Lock()
			state := s.toolStateLocked(assistant.ID, call.ID)
			mayEndRun := s.mayEndRunLocked(call.Name)
			s.mu.Unlock()
			if state.unresolved != nil || state.result != nil && state.result.Tool.NeedsResolution {
				inv := state.unresolved
				if inv == nil {
					inv = state.result
				}
				unknown = &UnknownOutcomeError{ExecutionID: inv.Tool.Invocation.ExecutionID}
				continue
			}
			if state.result != nil || state.control != nil || state.started == nil {
				continue
			}
			inv := clone(state.started.Tool.Invocation)
			if mayEndRun && state.completed == nil {
				if state.unresolved == nil {
					_, err := s.append(ctx, Entry{Kind: EntryToolUnresolved, RunID: inv.RunID, TurnID: inv.TurnID, Tool: &ToolRecord{Invocation: inv, NeedsResolution: true}})
					if err != nil {
						return err
					}
				}
				unknown = &UnknownOutcomeError{ExecutionID: inv.ExecutionID}
				continue
			}
			result := toolFailure("outcome_unknown", "Execution started before interruption; the operation may have partially or fully completed. The host must reconcile its effects. Do not automatically repeat it.", OutcomeUnknown)
			needsResolution := true
			if state.completed != nil {
				fact := state.completed.Tool.Result
				result = toolFailure("result_unavailable", "Execution finished before interruption, but its final output was not committed. Do not repeat the operation to retrieve output.", fact.Outcome)
				result.Artifact = fact.Artifact
				result.Terminate = fact.Terminate
				needsResolution = fact.Outcome == OutcomeUnknown
			}
			_, err := s.commitToolResult(ctx, inv, result, needsResolution)
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
type toolState struct{ prepared, authorized, started, completed, result, control, unresolved *Entry }

func (s *Session) toolStateLocked(assistantID, callID string) toolState {
	if assistantID != s.state.Branch.LastMessageID {
		return toolState{}
	}
	v := s.state.Tools[callID]
	if v.Result == nil && v.Control == nil && v.Unresolved == nil {
		if i, ok := s.windowIndex[s.state.Branch.ToolEntries[callID]]; ok {
			e := s.window[i]
			if e.Tool != nil && e.Tool.Invocation.AssistantID == assistantID && e.Tool.Invocation.Call.ID == callID {
				switch e.Kind {
				case EntryToolControl:
					v.Control = &e
				case EntryToolUnresolved:
					v.Unresolved = &e
				default:
					v.Result = &e
				}
			}
		}
	}
	return toolState{prepared: v.Prepared, authorized: v.Authorized, started: v.Started, completed: v.Completed, result: v.Result, control: v.Control, unresolved: v.Unresolved}
}

func (s *Session) mayEndRunLocked(name string) bool {
	i, ok := s.windowIndex[s.state.Branch.BatchConfigurationID]
	if !ok || s.window[i].Configuration == nil {
		return false
	}
	for _, definition := range s.window[i].Configuration.Tools {
		if definition.Name == name {
			return definition.MayEndRun
		}
	}
	return false
}
func (s *Session) controlEntriesLocked(id, runID string, inv Invocation, end RunEnd) ([]Entry, error) {
	if !s.mayEndRunLocked(inv.Call.Name) {
		return nil, fmtInvalid("tool %q does not declare MayEndRun", inv.Call.Name)
	}
	return []Entry{
		{ID: id, Kind: EntryToolControl, RunID: runID, TurnID: inv.TurnID, Tool: &ToolRecord{Invocation: inv, Control: &end}},
		{Kind: EntryRun, RunID: runID, Run: &RunRecord{ID: runID, Status: "completed", EndReason: end.Reason}},
	}, nil
}
