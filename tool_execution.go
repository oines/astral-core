package astral

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type batchOutcome struct {
	Natural bool
	EndRun  *RunEnd
}

func (a *Agent) executeBatch(ctx context.Context, run *Run, assistant Frame) (batchOutcome, error) {
	state := a.driver.Snapshot()
	definitions := map[string]ToolDefinition{}
	declarations := state.BatchTools
	if declarations == nil {
		declarations = state.Configuration.Tools
	}
	if err := validateToolBatch(assistant.Message, declarations); err != nil {
		return batchOutcome{}, err
	}
	for _, d := range declarations {
		definitions[d.Name] = d
	}
	sequential := false
	calls := assistant.Message.ToolCalls()
	for _, c := range calls {
		sequential = sequential || definitions[c.Name].Sequential || definitions[c.Name].MayEndRun
	}
	results := make([]ToolResult, len(calls))
	errs := make([]error, len(results))
	if sequential {
		interrupted := false
		for i, c := range calls {
			if interrupted {
				results[i], errs[i] = a.executeTool(ctx, run, assistant, c, definitions[c.Name], true, false)
			} else {
				results[i], errs[i] = a.executeTool(ctx, run, assistant, c, definitions[c.Name], false, false)
			}
			interrupted = interrupted || errs[i] != nil
		}
	} else {
		// Preflight every call in source order before dispatching any parallel effect.
		for i, c := range calls {
			results[i], errs[i] = a.executeTool(ctx, run, assistant, c, definitions[c.Name], false, true)
		}
		if err := errors.Join(errs...); err != nil {
			return batchOutcome{}, err
		}
		var wg sync.WaitGroup
		jobs := make(chan int)
		for n := 0; n < min(len(results), a.config.MaxParallelTools); n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					c := calls[i]
					results[i], errs[i] = a.executeTool(ctx, run, assistant, c, definitions[c.Name], false, false)
				}
			}()
		}
		for i := range results {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
	}
	// A committed control is the terminal fact. A concurrent cancellation cannot
	// turn that completed Run into a cancelled one.
	if end := a.driver.Snapshot().controlEnd(); end != nil {
		return batchOutcome{Natural: true, EndRun: clone(end)}, nil
	}
	if err := errors.Join(append(errs, ctx.Err())...); err != nil {
		return batchOutcome{}, err
	}
	terminate := len(results) > 0
	for _, r := range results {
		terminate = terminate && r.Terminate
	}
	return batchOutcome{Natural: terminate}, nil
}
func (a *Agent) executeTool(ctx context.Context, run *Run, assistant Frame, call ToolCall, definition ToolDefinition, skip, preflight bool) (result ToolResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("tool pipeline panicked: %v", p)
		}
	}()
	snapshot := a.driver.Snapshot()
	state := snapshot.Calls[call.ID]
	if state.Control != nil {
		return ToolResult{}, nil
	}
	if state.NeedsResolution && state.Result == nil {
		return ToolResult{}, &UnknownOutcomeError{ExecutionID: state.Invocation.ExecutionID}
	}
	if state.Result != nil {
		result = clone(*state.Result)
		if state.NeedsResolution {
			return result, &UnknownOutcomeError{ExecutionID: state.Invocation.ExecutionID}
		}
		return result, nil
	}
	inv := Invocation{SessionID: snapshot.ID, RunID: run.id, TurnID: assistant.TurnID, AssistantID: assistant.ID, ExecutionID: newID(), Call: clone(call)}
	if state.Prepared {
		inv = clone(state.Invocation)
	}
	if state.Started {
		return ToolResult{}, &UnknownOutcomeError{ExecutionID: inv.ExecutionID}
	}
	if skip {
		return a.commitToolResult(ctx, inv, toolFailure("batch_interrupted", "The batch stopped before this call was executed.", OutcomeNotExecuted), false)
	}
	if ctx.Err() != nil {
		return a.commitToolResult(ctx, inv, toolFailure("cancelled", "Cancelled before tool execution.", OutcomeNotExecuted), false)
	}
	if definition.Name == "" {
		return a.commitToolResult(ctx, inv, toolFailure("tool_not_found", "This tool is not declared in the request.", OutcomeNotExecuted), false)
	}
	registered, err := a.toolFor(definition)
	if err != nil {
		return a.commitToolResult(ctx, inv, toolFailure("tool_unavailable", err.Error(), OutcomeNotExecuted), false)
	}
	arguments, err := decodeJSON(inv.Call.Arguments)
	if err == nil {
		err = registered.schema.Validate(arguments)
	}
	if err != nil {
		return a.commitToolResult(ctx, inv, toolFailure("invalid_arguments", err.Error(), OutcomeNotExecuted), false)
	}
	if !state.Prepared {
		if targeted, ok := registered.tool.(TargetedTool); ok {
			binding, bindErr := bindTool(ctx, targeted, inv)
			if bindErr != nil {
				return a.commitToolResult(ctx, inv, toolFailure("target_unavailable", bindErr.Error(), OutcomeNotExecuted), false)
			}
			inv.Binding = binding
		}
		if err = a.commitTool(ctx, TransitionPrepared, inv, nil, nil, false); err != nil {
			return ToolResult{}, err
		}
	}
	decision := ToolDecision{}
	if state.Decision != nil {
		decision = *state.Decision
	} else {
		if a.config.BeforeToolCall != nil {
			decision, err = beforeTool(ctx, a.config.BeforeToolCall, clone(inv))
			if err != nil {
				return a.commitToolResult(ctx, inv, toolFailure("before_tool_failed", "Tool was not executed: "+err.Error(), OutcomeNotExecuted), false)
			}
		}
		if err = a.commitTool(ctx, TransitionAuthorized, inv, &decision, nil, false); err != nil {
			return ToolResult{}, err
		}
	}
	if decision.Block {
		r := toolFailure("blocked", decision.Reason, OutcomeNotExecuted)
		r.Terminate = decision.Terminate
		return a.commitToolResult(ctx, inv, r, false)
	}
	if preflight {
		return ToolResult{}, nil
	}
	if ctx.Err() != nil {
		return a.commitToolResult(ctx, inv, toolFailure("cancelled", "Cancelled before tool execution.", OutcomeNotExecuted), false)
	}
	if err = a.commitTool(ctx, TransitionStarted, inv, nil, nil, false); err != nil {
		return ToolResult{}, err
	}
	if ctx.Err() != nil {
		return a.commitToolResult(ctx, inv, toolFailure("cancelled", "Cancelled before dispatch.", OutcomeNotExecuted), false)
	}
	a.Publish(Event{Type: EventToolStart, RunID: run.id, TurnID: inv.TurnID, ToolCallID: call.ID, ExecutionID: inv.ExecutionID})
	execution, executeErr := invokeTool(ctx, registered.tool, inv, func(p ToolProgress) {
		a.Publish(Event{Type: EventToolUpdate, RunID: run.id, TurnID: inv.TurnID, ToolCallID: call.ID, ExecutionID: inv.ExecutionID, Progress: &p})
	})
	if execution.Result != nil {
		result = *execution.Result
	}
	if executeErr == nil {
		if invalid := execution.Validate(); invalid != nil {
			result = toolFailure("invalid_tool_execution", invalid.Error(), OutcomeUnknown)
		} else if execution.EndRun != nil {
			if !definition.MayEndRun {
				result = toolFailure("invalid_tool_execution", "Tool returned EndRun without declaring MayEndRun.", OutcomeUnknown)
			} else {
				return ToolResult{}, a.commitToolControl(ctx, run.id, inv, *execution.EndRun)
			}
		}
	}
	if executeErr != nil {
		outcome := OutcomeFailed
		code := "tool_error"
		if isCancelled(executeErr) || result.Outcome == OutcomeUnknown {
			outcome = OutcomeUnknown
		}
		var classified *ToolError
		if errors.As(executeErr, &classified) {
			outcome = classified.Outcome
			if classified.Code != "" {
				code = classified.Code
			}
		}
		if result.Outcome != "" {
			outcome = result.Outcome
		}
		result = ToolResult{Content: Text("Tool call ended with an error; partial effects may have occurred: " + executeErr.Error()), IsError: true, Code: code, Outcome: outcome, Artifact: result.Artifact}
	}
	result = normalizeResult(result)
	if validateToolResult(result) != nil {
		result = toolFailure("invalid_tool_result", "Tool returned an invalid result; effects require host reconciliation.", OutcomeUnknown)
	}
	if definition.MayEndRun && result.Outcome == OutcomeUnknown {
		return ToolResult{}, a.commitToolUnresolved(ctx, inv)
	}
	fact := ToolResult{Outcome: result.Outcome, Artifact: result.Artifact, Terminate: result.Terminate}
	cleanup, cancel := persistContext(ctx)
	err = a.commitTool(cleanup, TransitionCompleted, inv, nil, &fact, false)
	cancel()
	if err != nil {
		return ToolResult{}, err
	}
	actualOutcome := result.Outcome
	if a.config.AfterToolCall != nil {
		patch, hookErr := afterTool(ctx, a.config.AfterToolCall, inv, result)
		if hookErr != nil {
			result.IsError = true
			result.Code = "after_tool_failed"
			result.Content = append(result.Content, Content{Type: "text", Text: "Tool execution already finished; result processing failed: " + hookErr.Error()})
		} else {
			result = applyToolPatch(result, patch)
		}
		result.Outcome = actualOutcome
	}
	result = normalizeResult(result)
	if validateToolResult(result) != nil {
		result = toolFailure("after_tool_failed", "Tool execution finished, but its processed result is invalid. Do not repeat the operation to retrieve output.", actualOutcome)
	}
	return a.commitToolResult(ctx, inv, result, result.Outcome == OutcomeUnknown)
}

func (a *Agent) commitToolControl(ctx context.Context, runID string, inv Invocation, end RunEnd) error {
	cleanup, cancel := persistContext(ctx)
	defer cancel()
	id := newID()
	if err := a.driver.Commit(cleanup, Transition{Kind: TransitionToolControl, ID: id, RunID: runID, TurnID: inv.TurnID, Invocation: inv, Control: &end}); err != nil {
		return err
	}
	a.Publish(Event{Type: EventToolEnd, RunID: runID, TurnID: inv.TurnID, ToolCallID: inv.Call.ID, ExecutionID: inv.ExecutionID, EntryID: id, Status: "completed", EndReason: end.Reason})
	return nil
}

func (a *Agent) commitToolUnresolved(ctx context.Context, inv Invocation) error {
	cleanup, cancel := persistContext(ctx)
	defer cancel()
	id := newID()
	if err := a.driver.Commit(cleanup, Transition{Kind: TransitionToolUnresolved, ID: id, RunID: inv.RunID, TurnID: inv.TurnID, Invocation: inv, NeedsResolution: true}); err != nil {
		return err
	}
	err := &UnknownOutcomeError{ExecutionID: inv.ExecutionID}
	a.Publish(Event{Type: EventToolEnd, RunID: inv.RunID, TurnID: inv.TurnID, ToolCallID: inv.Call.ID, ExecutionID: inv.ExecutionID, EntryID: id, Error: err.Error()})
	return err
}
func (a *Agent) commitTool(ctx context.Context, kind TransitionKind, inv Invocation, decision *ToolDecision, result *ToolResult, needs bool) error {
	return a.driver.Commit(ctx, Transition{Kind: kind, ID: newID(), RunID: inv.RunID, TurnID: inv.TurnID, Invocation: inv, Decision: decision, Result: result, NeedsResolution: needs})
}
func (a *Agent) commitToolResult(ctx context.Context, inv Invocation, result ToolResult, needs bool) (ToolResult, error) {
	cleanup, cancel := persistContext(ctx)
	defer cancel()
	result = normalizeResult(result)
	bounded, budgetErr := a.driver.AcceptResult(cleanup, inv, result)
	result = bounded
	id := newID()
	if err := a.driver.Commit(cleanup, Transition{Kind: TransitionResult, ID: id, RunID: inv.RunID, TurnID: inv.TurnID, Invocation: inv, Result: &result, NeedsResolution: needs}); err != nil {
		return result, err
	}
	a.Publish(Event{Type: EventToolEnd, RunID: inv.RunID, TurnID: inv.TurnID, ToolCallID: inv.Call.ID, ExecutionID: inv.ExecutionID, EntryID: id})
	if needs {
		return result, &UnknownOutcomeError{ExecutionID: inv.ExecutionID}
	}
	return result, budgetErr
}
