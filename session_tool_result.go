package astral

import (
	"context"
	"encoding/json"
	"errors"
)

func (s *Session) boundResult(ctx context.Context, inv Invocation, result ToolResult) (ToolResult, error) {
	s.mu.Lock()
	configuration := clone(s.configurationLocked())
	windowBytes := s.windowBytes
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
	encoded, encodeErr := json.Marshal(result)
	if encodeErr != nil {
		return result, encodeErr
	}
	if len(encoded) > s.config.MaxContextBytes/2 || windowBytes+len(encoded) > 2*s.config.MaxContextBytes-2048 {
		return deliveryFailure(result, "result_too_large", "The output exceeds the context byte budget."), nil
	}
	model, err := s.resolveModel(ctx, configuration.Selection)
	if err != nil {
		// A later selection can be unavailable while an already authorized tool
		// finishes. Keep its bounded execution result; resolving the next request
		// reports that model error without destroying the tool's output.
		return result, nil
	}
	request := Request{ModelID: model.Spec().ID, ModelRevision: model.Spec().Revision, SystemPrompt: configuration.SystemPrompt, Tools: configuration.Tools, Options: resolvedOptions(model), MaxOutputTokens: outputReservation(s.config.Budget.OutputTokens, model.Spec())}
	// Keep all declarations/calls and valid placeholder results; old conversation
	// messages are deliberately excluded when deciding whether one result alone
	// is impossible to deliver. Aggregate pressure is handled by compaction.
	request.Messages = append(request.Messages, *assistant)
	for _, call := range assistant.ToolCalls() {
		message := Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: Text("")}
		if call.ID == inv.Call.ID {
			message.Content = result.Content
			message.IsError = result.IsError
		}
		request.Messages = append(request.Messages, message)
	}
	_, err = s.fitContext(ctx, model, Context{Request: request})
	if errors.Is(err, ErrBudget) {
		return deliveryFailure(result, "result_too_large", "The output exceeds the model's delivery budget."), nil
	}
	// Only a measured single-result budget failure justifies shortening output.
	// Protocol/configuration errors belong to the next request's preparation.
	return result, nil
}

func deliveryFailure(result ToolResult, code, reason string) ToolResult {
	message := reason + " Execution outcome: " + string(result.Outcome) + ". This is a result-delivery failure, not a request to execute the tool again. Do not repeat side effects; narrow the query or retrieve the artifact."
	if result.Artifact != "" {
		message += " Artifact: " + result.Artifact
	}
	result.Content = Text(message)
	result.Details = nil
	result.IsError = true
	result.Code = code
	return result
}
