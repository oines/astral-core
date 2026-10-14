package astral

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

func (a *Agent) executeBatch(ctx context.Context, run *Run, assistant Frame) (bool, error) {
	state := a.driver.Snapshot()
	definitions := map[string]ToolDefinition{}
	declarations := state.BatchTools
	if declarations == nil {
		declarations = state.Configuration.Tools
	}
	for _, d := range declarations {
		definitions[d.Name] = d
	}
	sequential := false
	for _, c := range assistant.Message.ToolCalls {
		sequential = sequential || definitions[c.Name].Sequential
	}
	results := make([]ToolResult, len(assistant.Message.ToolCalls))
	errs := make([]error, len(results))
	if sequential {
		interrupted := false
		for i, c := range assistant.Message.ToolCalls {
			if interrupted {
				results[i], errs[i] = a.executeTool(ctx, run, assistant, c, definitions[c.Name], true)
			} else {
				results[i], errs[i] = a.executeTool(ctx, run, assistant, c, definitions[c.Name], false)
			}
			interrupted = interrupted || errs[i] != nil
		}
	} else {
		var wg sync.WaitGroup
		jobs := make(chan int)
		for n := 0; n < min(len(results), a.config.MaxParallelTools); n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					c := assistant.Message.ToolCalls[i]
					results[i], errs[i] = a.executeTool(ctx, run, assistant, c, definitions[c.Name], false)
				}
			}()
		}
		for i := range results {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
	}
	if err := errors.Join(append(errs, ctx.Err())...); err != nil {
		return false, err
	}
	terminate := len(results) > 0
	for _, r := range results {
		terminate = terminate && r.Terminate
	}
	return terminate, nil
}
func (a *Agent) executeTool(ctx context.Context, run *Run, assistant Frame, call ToolCall, definition ToolDefinition, skip bool) (result ToolResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("tool pipeline panicked: %v", p)
		}
	}()
	snapshot := a.driver.Snapshot()
	state := snapshot.Calls[call.ID]
	if state.Result != nil {
		result = clone(*state.Result)
		if state.NeedsResolution {
			return result, &UnknownOutcomeError{ExecutionID: state.Invocation.ExecutionID}
		}
		return result, nil
	}
	inv := Invocation{SessionID: snapshot.ID, RunID: run.id, TurnID: assistant.TurnID, AssistantID: assistant.ID, ExecutionID: newID(), Call: clone(call), Binding: snapshot.Binding}
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
	result, executeErr := invokeTool(ctx, registered.tool, inv, func(p ToolProgress) {
		a.Publish(Event{Type: EventToolUpdate, RunID: run.id, TurnID: inv.TurnID, ToolCallID: call.ID, ExecutionID: inv.ExecutionID, Progress: &p})
	})
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
		result = ToolResult{Content: Text("Tool call ended with an error; partial effects may have occurred: " + executeErr.Error()), IsError: true, Code: code, Outcome: outcome, Artifact: result.Artifact, Context: result.Context}
	}
	result = normalizeResult(result)
	if validateToolResult(result) != nil {
		result = toolFailure("invalid_tool_result", "Tool returned an invalid result; effects require host reconciliation.", OutcomeUnknown)
	}
	if result.Context != nil && !definition.Sequential {
		result.Context = nil
		result.IsError = true
		result.Code = "invalid_context_change"
		result.Content = Text("Tool executed, but its context update requires a sequential tool.")
	}
	fact := ToolResult{Outcome: result.Outcome, Artifact: result.Artifact, Context: clone(result.Context)}
	cleanup, cancel := persistContext(ctx)
	err = a.commitTool(cleanup, TransitionCompleted, inv, nil, &fact, false)
	cancel()
	if err != nil {
		return ToolResult{}, err
	}
	actualOutcome := result.Outcome
	actualContext := clone(result.Context)
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
		result.Context = actualContext
	}
	result = normalizeResult(result)
	if validateToolResult(result) != nil {
		result = toolFailure("after_tool_failed", "Tool execution finished, but its processed result is invalid. Do not repeat the operation to retrieve output.", actualOutcome)
		result.Context = actualContext
	}
	return a.commitToolResult(ctx, inv, result, result.Outcome == OutcomeUnknown)
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
