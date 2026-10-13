package astral

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

func (s *Session) batchConfiguration(assistantID string) (Configuration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.pathLocked()
	for i, e := range path {
		if e.ID == assistantID {
			c, _, err := effectiveConfiguration(path[:i+1])
			return clone(c), err
		}
	}
	return Configuration{}, ErrNotFound
}

func (s *Session) executeBatch(ctx context.Context, run *Run, assistant Entry) (bool, error) {
	configuration, err := s.batchConfiguration(assistant.ID)
	if err != nil {
		return false, err
	}
	definitions := map[string]ToolDefinition{}
	for _, d := range configuration.Tools {
		definitions[d.Name] = d
	}
	sequential := false
	for _, call := range assistant.Message.ToolCalls {
		if definitions[call.Name].Sequential {
			sequential = true
		}
	}
	results := make([]ToolResult, len(assistant.Message.ToolCalls))
	errs := make([]error, len(results))
	if sequential {
		interrupted := false
		for i, call := range assistant.Message.ToolCalls {
			if interrupted {
				results[i], errs[i] = s.skipTool(ctx, run, assistant, call, "batch_interrupted", "The batch stopped before this call was executed.")
			} else {
				results[i], errs[i] = s.executeTool(ctx, run, assistant, call, definitions[call.Name])
			}
			if errs[i] != nil {
				interrupted = true
			}
		}
	} else {
		var wg sync.WaitGroup
		jobs := make(chan int)
		workers := min(len(results), s.agent.config.MaxParallelTools)
		for worker := 0; worker < workers; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					call := assistant.Message.ToolCalls[i]
					results[i], errs[i] = s.executeTool(ctx, run, assistant, call, definitions[call.Name])
				}
			}()
		}
		for i := range results {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
	}
	if err = errors.Join(append(errs, ctx.Err())...); err != nil {
		return false, err
	}
	terminate := len(results) > 0
	for _, result := range results {
		terminate = terminate && result.Terminate
	}
	return terminate, nil
}

func (s *Session) invocationLocked(run *Run, assistant Entry, call ToolCall) Invocation {
	environment, binding := effectiveEnvironment(s.pathLocked())
	return Invocation{SessionID: s.id, RunID: run.id, TurnID: assistant.TurnID, AssistantID: assistant.ID, ExecutionID: newID(), Call: clone(call), Binding: binding, Environment: clone(environment)}
}

func (s *Session) skipTool(ctx context.Context, run *Run, assistant Entry, call ToolCall, code, message string) (ToolResult, error) {
	s.mu.Lock()
	state := s.toolStateLocked(assistant.ID, call.ID)
	if state.result != nil {
		result := clone(*state.result.Tool.Result)
		needs := state.result.Tool.NeedsResolution
		s.mu.Unlock()
		if needs {
			return result, &UnknownOutcomeError{ExecutionID: state.result.Tool.Invocation.ExecutionID}
		}
		return result, nil
	}
	inv := s.invocationLocked(run, assistant, call)
	if state.prepared != nil {
		inv = clone(state.prepared.Tool.Invocation)
	}
	s.mu.Unlock()
	if state.started != nil {
		return ToolResult{}, &UnknownOutcomeError{ExecutionID: inv.ExecutionID}
	}
	return s.commitToolResult(ctx, inv, toolFailure(code, message, OutcomeNotExecuted), false)
}

func (s *Session) executeTool(ctx context.Context, run *Run, assistant Entry, call ToolCall, definition ToolDefinition) (result ToolResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("tool pipeline panicked: %v", p)
		}
	}()
	s.mu.Lock()
	state := s.toolStateLocked(assistant.ID, call.ID)
	if state.result != nil {
		result := clone(*state.result.Tool.Result)
		needs := state.result.Tool.NeedsResolution
		s.mu.Unlock()
		if needs {
			return result, &UnknownOutcomeError{ExecutionID: state.result.Tool.Invocation.ExecutionID}
		}
		return result, nil
	}
	inv := s.invocationLocked(run, assistant, call)
	if state.prepared != nil {
		inv = clone(state.prepared.Tool.Invocation)
	}
	s.mu.Unlock()
	if state.started != nil {
		return ToolResult{}, &UnknownOutcomeError{ExecutionID: inv.ExecutionID}
	}
	if err := ctx.Err(); err != nil {
		return s.commitToolResult(ctx, inv, toolFailure("cancelled", "Cancelled before tool execution.", OutcomeNotExecuted), false)
	}
	if definition.Name == "" {
		return s.commitToolResult(ctx, inv, toolFailure("tool_not_found", "This tool is not declared in the request.", OutcomeNotExecuted), false)
	}
	registered, err := s.agent.toolFor(definition)
	if err != nil {
		return s.commitToolResult(ctx, inv, toolFailure("tool_unavailable", err.Error(), OutcomeNotExecuted), false)
	}
	arguments, err := decodeJSON(inv.Call.Arguments)
	if err == nil {
		err = registered.schema.Validate(arguments)
	}
	if err != nil {
		return s.commitToolResult(ctx, inv, toolFailure("invalid_arguments", err.Error(), OutcomeNotExecuted), false)
	}
	if state.prepared == nil {
		if targeted, ok := registered.tool.(TargetedTool); ok {
			binding, environment, bindErr := bindTool(ctx, targeted, inv)
			if bindErr != nil {
				return s.commitToolResult(ctx, inv, toolFailure("target_unavailable", bindErr.Error(), OutcomeNotExecuted), false)
			}
			inv.Binding = binding
			inv.Environment = environment
		}
		if _, err = s.append(ctx, Entry{Kind: EntryToolPrepared, RunID: run.id, TurnID: inv.TurnID, Tool: &ToolRecord{Invocation: inv}}); err != nil {
			return ToolResult{}, err
		}
	}
	decision := ToolDecision{}
	if state.authorized != nil {
		decision = *state.authorized.Tool.Decision
	} else {
		if s.agent.config.BeforeToolCall != nil {
			hookInvocation := clone(inv)
			hookInvocation.awaitApproval = func(waitCtx context.Context, request ApprovalRequest) (ApprovalDecision, error) {
				return s.awaitApproval(waitCtx, inv, request)
			}
			decision, err = beforeTool(ctx, s.agent.config.BeforeToolCall, hookInvocation)
			if err != nil {
				return s.commitToolResult(ctx, inv, toolFailure("before_tool_failed", "Tool was not executed: "+err.Error(), OutcomeNotExecuted), false)
			}
		}
		if _, err = s.append(ctx, Entry{Kind: EntryToolAuthorized, RunID: run.id, TurnID: inv.TurnID, Tool: &ToolRecord{Invocation: inv, Decision: &decision}}); err != nil {
			return ToolResult{}, err
		}
	}
	if decision.Block {
		result := toolFailure("blocked", decision.Reason, OutcomeNotExecuted)
		result.Terminate = decision.Terminate
		return s.commitToolResult(ctx, inv, result, false)
	}
	if err = ctx.Err(); err != nil {
		return s.commitToolResult(ctx, inv, toolFailure("cancelled", "Cancelled before tool execution.", OutcomeNotExecuted), false)
	}
	if _, err = s.append(ctx, Entry{Kind: EntryToolStarted, RunID: run.id, TurnID: inv.TurnID, Tool: &ToolRecord{Invocation: inv}}); err != nil {
		return ToolResult{}, err
	}
	// If cancellation wins after the intent commit but before the call, we know
	// this invocation was not dispatched and can record that fact explicitly.
	if err = ctx.Err(); err != nil {
		return s.commitToolResult(ctx, inv, toolFailure("cancelled", "Cancelled before dispatch.", OutcomeNotExecuted), false)
	}
	s.publish(Event{Type: EventToolStart, RunID: run.id, TurnID: inv.TurnID, ToolCallID: call.ID, ExecutionID: inv.ExecutionID})
	result, executeErr := invokeTool(ctx, registered.tool, inv, func(progress ToolProgress) {
		s.publish(Event{Type: EventToolUpdate, RunID: run.id, TurnID: inv.TurnID, ToolCallID: call.ID, ExecutionID: inv.ExecutionID, Progress: &progress})
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
		result = ToolResult{Content: Text("Tool call ended with an error; partial effects may have occurred: " + executeErr.Error()), IsError: true, Code: code, Outcome: outcome, Artifact: result.Artifact, Environment: result.Environment}
	}
	result = normalizeResult(result)
	if !validOutcome(result.Outcome) {
		result = toolFailure("invalid_tool_result", "Tool returned an invalid execution outcome; effects require host reconciliation.", OutcomeUnknown)
	}
	if resultErr := validateToolResult(result); resultErr != nil {
		result = toolFailure("invalid_tool_result", "Tool returned an invalid result; its effects require host reconciliation.", OutcomeUnknown)
	}
	if result.Environment != nil && !definition.Sequential {
		result.Environment = nil
		result.IsError = true
		result.Code = "invalid_environment_change"
		result.Content = Text("Tool executed, but its environment update was rejected: environment-changing tools must be sequential.")
	}
	// Persist only the execution fact and optional artifact/environment, not an
	// unbounded pre-hook result. A crash after this point loses delivery, not the
	// knowledge that the tool already ran.
	fact := ToolResult{Outcome: result.Outcome, Artifact: result.Artifact, Environment: result.Environment}
	cleanup, cancel := persistContext(ctx)
	_, err = s.append(cleanup, Entry{Kind: EntryToolCompleted, RunID: run.id, TurnID: inv.TurnID, Tool: &ToolRecord{Invocation: inv, Result: &fact}})
	cancel()
	if err != nil {
		return ToolResult{}, err
	}
	actualOutcome := result.Outcome
	actualEnvironment := clone(result.Environment)
	if s.agent.config.AfterToolCall != nil {
		changed, hookErr := afterTool(ctx, s.agent.config.AfterToolCall, inv, result)
		if hookErr != nil {
			result.IsError = true
			result.Code = "after_tool_failed"
			result.Content = append(result.Content, Content{Type: "text", Text: "Tool execution already finished; result processing failed: " + hookErr.Error()})
		} else {
			result = applyToolPatch(result, changed)
		}
		result.Outcome = actualOutcome // hooks change presentation, not execution facts
		result.Environment = actualEnvironment
	}
	if result.Environment != nil && !definition.Sequential {
		result.Environment = nil
		result.IsError = true
		result.Code = "invalid_environment_change"
	}
	result = normalizeResult(result)
	if resultErr := validateToolResult(result); resultErr != nil {
		result = toolFailure("after_tool_failed", "Tool execution finished, but its processed result is not valid JSON. Do not repeat the operation to retrieve its output.", actualOutcome)
		result.Environment = actualEnvironment
	}
	return s.commitToolResult(ctx, inv, result, result.Outcome == OutcomeUnknown)
}
