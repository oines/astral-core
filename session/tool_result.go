package session

import (
	"context"
	"encoding/json"
	"errors"

	protocol "github.com/oines/astral-core"
)

func (s *Session) boundResult(ctx context.Context, inv protocol.Invocation, result protocol.ToolResult) (protocol.ToolResult, error) {
	s.mu.Lock()
	configuration := clone(s.configurationLocked())
	windowBytes := s.windowBytes
	var assistant *protocol.Message
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
	encoded, encodeErr := json.Marshal(result)
	if encodeErr != nil {
		return result, encodeErr
	}
	if len(encoded) > s.agent.config.MaxContextBytes/2 || windowBytes+len(encoded) > 2*s.agent.config.MaxContextBytes-2048 {
		return deliveryFailure(result, "result_too_large", "The output exceeds the context byte budget."), nil
	}
	model, err := s.agent.model(ctx, configuration.ModelID)
	if err != nil {
		return deliveryFailure(result, "result_delivery_failed", err.Error()), err
	}
	request := protocol.Request{ModelID: model.Spec().ID, ModelRevision: model.Spec().Revision, SystemPrompt: configuration.SystemPrompt, Tools: configuration.Tools, Options: configuration.Options, MaxOutputTokens: outputReservation(s.agent.config.Budget.OutputTokens, model.Spec())}
	// Keep all declarations/calls and valid placeholder results; old conversation
	// messages are deliberately excluded when deciding whether one result alone
	// is impossible to deliver. Aggregate pressure is handled by compaction.
	request.Messages = append(request.Messages, *assistant)
	for _, call := range assistant.ToolCalls() {
		message := protocol.Message{Role: protocol.RoleTool, ToolCallID: call.ID, Name: call.Name, Content: protocol.Text("")}
		if call.ID == inv.Call.ID {
			message.Content = result.Content
			message.IsError = result.IsError
		}
		request.Messages = append(request.Messages, message)
	}
	request, err = protocol.PrepareRequest(ctx, model, request)
	if err == nil {
		_, err = checkBudget(ctx, model, request, s.agent.config.Budget.SafetyMargin)
	}
	if errors.Is(err, ErrBudget) {
		return deliveryFailure(result, "result_too_large", "The output exceeds the model's delivery budget."), nil
	}
	if err != nil {
		return deliveryFailure(result, "result_delivery_failed", err.Error()), err
	}
	return result, nil
}

func deliveryFailure(result protocol.ToolResult, code, reason string) protocol.ToolResult {
	message := reason + " Execution outcome: " + string(result.Outcome) + ". This is a result-delivery failure, not a request to execute the tool again. Do not repeat side effects; narrow the query or retrieve the artifact."
	if result.Artifact != "" {
		message += " Artifact: " + result.Artifact
	}
	result.Content = protocol.Text(message)
	result.Details = nil
	result.IsError = true
	result.Code = code
	return result
}
func normalizeResult(result protocol.ToolResult) protocol.ToolResult {
	if result.Outcome == "" {
		result.Outcome = protocol.OutcomeSucceeded
		if result.IsError {
			result.Outcome = protocol.OutcomeFailed
		}
	}
	if len(result.Content) == 0 {
		result.Content = protocol.Text("")
	}
	return result
}

func toolFailure(code, message string, outcome protocol.Outcome) protocol.ToolResult {
	return protocol.ToolResult{Content: protocol.Text(message), IsError: true, Code: code, Outcome: outcome}
}

func validOutcome(v protocol.Outcome) bool {
	return v == protocol.OutcomeSucceeded || v == protocol.OutcomeFailed || v == protocol.OutcomeNotExecuted || v == protocol.OutcomeUnknown
}

func validateToolResult(result protocol.ToolResult) error {
	if !validOutcome(result.Outcome) {
		return fmtInvalid("invalid execution outcome")
	}
	if err := validateMessage(protocol.Message{Role: protocol.RoleTool, ToolCallID: "result", Content: result.Content}); err != nil {
		return err
	}
	_, err := copyJSON(result)
	return err
}
