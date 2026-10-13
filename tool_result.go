package astral

import (
	"context"
	"errors"
)

func normalizeResult(result ToolResult) ToolResult {
	if result.Outcome == "" {
		result.Outcome = OutcomeSucceeded
		if result.IsError {
			result.Outcome = OutcomeFailed
		}
	}
	if len(result.Content) == 0 {
		result.Content = Text("")
	}
	return result
}

func toolFailure(code, message string, outcome Outcome) ToolResult {
	return ToolResult{Content: Text(message), IsError: true, Code: code, Outcome: outcome}
}

func validOutcome(v Outcome) bool {
	return v == OutcomeSucceeded || v == OutcomeFailed || v == OutcomeNotExecuted || v == OutcomeUnknown
}

func validateToolResult(result ToolResult) error {
	if !validOutcome(result.Outcome) {
		return fmtInvalid("invalid execution outcome")
	}
	if err := validateMessage(Message{Role: RoleTool, ToolCallID: "result", Content: result.Content}); err != nil {
		return err
	}
	_, err := copyJSON(result)
	return err
}

func (s *Session) commitToolResult(ctx context.Context, inv Invocation, result ToolResult, needsResolution bool) (ToolResult, error) {
	result = normalizeResult(result)
	cleanup, cancel := persistContext(ctx)
	defer cancel()
	bounded, budgetErr := s.boundResult(cleanup, inv, result)
	result = bounded
	record := ToolRecord{Invocation: inv, Result: &result, NeedsResolution: needsResolution}
	entries := []Entry{{Kind: EntryToolResult, RunID: inv.RunID, TurnID: inv.TurnID, Tool: &record}}
	if result.Environment != nil {
		entries = append(entries, Entry{Kind: EntryEnvironment, RunID: inv.RunID, TurnID: inv.TurnID, Environment: result.Environment})
	}
	s.mu.Lock()
	committed, err := s.appendLocked(cleanup, entries...)
	s.mu.Unlock()
	if err != nil {
		return result, err
	}
	s.publish(Event{Type: EventToolEnd, RunID: inv.RunID, TurnID: inv.TurnID, ToolCallID: inv.Call.ID, ExecutionID: inv.ExecutionID, EntryID: committed[0].ID})
	if needsResolution {
		return result, &UnknownOutcomeError{ExecutionID: inv.ExecutionID}
	}
	return result, budgetErr
}

func (s *Session) boundResult(ctx context.Context, inv Invocation, result ToolResult) (ToolResult, error) {
	s.mu.Lock()
	configuration := clone(s.configurationLocked())
	var assistant *Message
	for _, e := range s.pathLocked() {
		if e.ID == inv.AssistantID {
			assistant = e.Message
			break
		}
	}
	s.mu.Unlock()
	if assistant == nil {
		return result, ErrCorrupt
	}
	model, err := s.agent.model(ctx, configuration.ModelID)
	if err != nil {
		return deliveryFailure(result, "result_delivery_failed", err.Error()), err
	}
	request := Request{ModelID: model.Spec().ID, SystemPrompt: configuration.SystemPrompt, Tools: configuration.Tools, MaxOutputTokens: outputReservation(s.agent.config.Budget.OutputTokens, model.Spec())}
	// Keep all declarations/calls and valid placeholder results; old conversation
	// messages are deliberately excluded when deciding whether one result alone
	// is impossible to deliver. Aggregate pressure is handled by compaction.
	request.Messages = append(request.Messages, *assistant)
	for _, call := range assistant.ToolCalls {
		message := Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: Text("")}
		if call.ID == inv.Call.ID {
			message.Content = result.Content
			message.IsError = result.IsError
		}
		request.Messages = append(request.Messages, message)
	}
	_, err = checkBudget(ctx, model, request, s.agent.config.Budget.SafetyMargin)
	if errors.Is(err, ErrBudget) {
		return deliveryFailure(result, "result_too_large", "The output exceeds the model's delivery budget."), nil
	}
	if err != nil {
		return deliveryFailure(result, "result_delivery_failed", err.Error()), err
	}
	return result, nil
}

func deliveryFailure(result ToolResult, code, reason string) ToolResult {
	message := reason + " Execution outcome: " + string(result.Outcome) + ". This is a result-delivery failure, not a request to execute the tool again. Do not repeat side effects; narrow the query or retrieve the artifact."
	if result.Artifact != "" {
		message += " Artifact: " + result.Artifact
	}
	result.Content = Text(message)
	result.IsError = true
	result.Code = code
	return result
}
