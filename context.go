package astral

import (
	"context"
	"encoding/json"
	"fmt"
)

// Context is the effective request plus provenance in the active branch. Its
// messages may cover much less history than Rollout after explicit compaction.
type Context struct {
	HeadID          string               `json:"head_id"`
	ConfigurationID string               `json:"configuration_id"`
	Request         Request              `json:"request"`
	Sources         []string             `json:"sources"`
	Environment     *EnvironmentSnapshot `json:"environment,omitempty"`
	Binding         Binding              `json:"binding"`
	EstimatedInput  int                  `json:"estimated_input"`
}

func (s *Session) InspectContext(ctx context.Context) (Context, error) {
	s.mu.Lock()
	path := clone(s.pathLocked())
	s.mu.Unlock()
	view, err := project(path)
	if err != nil {
		return Context{}, err
	}
	model, err := s.agent.model(ctx, view.Request.ModelID)
	if err != nil {
		return Context{}, err
	}
	view.Request.MaxOutputTokens = outputReservation(s.agent.config.Budget.OutputTokens, model.Spec())
	view.EstimatedInput, err = estimate(ctx, model, view.Request)
	return view, err
}

func (s *Session) requestContext(ctx context.Context, model Model) (Context, error) {
	s.mu.Lock()
	path := clone(s.pathLocked())
	s.mu.Unlock()
	view, err := project(path)
	if err != nil {
		return Context{}, err
	}
	if err = s.agent.validateConfiguration(ctx, Configuration{SystemPrompt: view.Request.SystemPrompt, ModelID: model.Spec().ID, Tools: view.Request.Tools}); err != nil {
		return Context{}, err
	}
	view.Request.ModelID = model.Spec().ID
	view.Request.MaxOutputTokens = outputReservation(s.agent.config.Budget.OutputTokens, model.Spec())
	view.EstimatedInput, err = checkBudget(ctx, model, view.Request, s.agent.config.Budget.SafetyMargin)
	return view, err
}

func resultKey(assistantID, callID string) string { return assistantID + "\x00" + callID }

// project is the only normal history-to-context projection. Tool results are
// emitted in the assistant's call order as one contiguous batch. Environment
// entries inside that batch follow it, preserving their relative order. Since
// no model request observes a half batch, previously sent prefixes stay stable.
func project(path []Entry) (Context, error) {
	configuration, configurationID, err := effectiveConfiguration(path)
	if err != nil {
		return Context{}, err
	}
	view := Context{ConfigurationID: configurationID, Request: Request{ModelID: configuration.ModelID, SystemPrompt: configuration.SystemPrompt, Tools: configuration.Tools}}
	if len(path) > 0 {
		view.HeadID = path[len(path)-1].ID
	}
	view.Environment, view.Binding = effectiveEnvironment(path)
	start := 0
	var checkpoint *Entry
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].Kind == EntryCompaction {
			e := path[i]
			checkpoint = &e
			break
		}
	}
	if checkpoint != nil {
		found := false
		for i, e := range path {
			if e.ID == checkpoint.Compaction.ThroughID {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			return Context{}, fmt.Errorf("%w: compaction boundary missing", ErrCorrupt)
		}
		view.Request.Messages = append(view.Request.Messages, Message{Role: RoleContext, ContextKind: "summary", Content: Text(checkpoint.Compaction.Summary)})
		view.Sources = append(view.Sources, checkpoint.ID)
	}
	visible := path[start:]
	if err := validateToolTimeline(visible); err != nil {
		return Context{}, err
	}
	results := map[string]Entry{}
	assistants := map[string]Entry{}
	for _, e := range visible {
		if e.Kind == EntryMessage && e.Message.Role == RoleAssistant {
			assistants[e.ID] = e
		}
		if e.Kind == EntryToolResult || e.Kind == EntryToolResolution {
			key := resultKey(e.Tool.Invocation.AssistantID, e.Tool.Invocation.Call.ID)
			if prev, ok := results[key]; ok && e.Kind != EntryToolResolution {
				return Context{}, fmt.Errorf("%w: duplicate result for %s (%s)", ErrCorrupt, key, prev.ID)
			}
			results[key] = e
		}
	}
	for _, e := range results {
		assistant, ok := assistants[e.Tool.Invocation.AssistantID]
		if !ok {
			return Context{}, fmt.Errorf("%w: result without visible assistant", ErrIncompleteBatch)
		}
		matched := false
		for _, call := range assistant.Message.ToolCalls {
			if call.ID == e.Tool.Invocation.Call.ID && sameJSON(call, e.Tool.Invocation.Call) {
				matched = true
				break
			}
		}
		if !matched {
			return Context{}, fmt.Errorf("%w: unmatched tool result", ErrIncompleteBatch)
		}
	}
	add := func(message Message, id string) {
		view.Request.Messages = append(view.Request.Messages, message)
		view.Sources = append(view.Sources, id)
	}
	for _, e := range visible {
		switch e.Kind {
		case EntryMessage:
			if e.Message.Role == RoleTool {
				return Context{}, fmt.Errorf("%w: tool messages require a tool result entry", ErrCorrupt)
			}
			add(*e.Message, e.ID)
			for _, call := range e.Message.ToolCalls {
				result, ok := results[resultKey(e.ID, call.ID)]
				if !ok {
					return Context{}, fmt.Errorf("%w: %s", ErrIncompleteBatch, call.ID)
				}
				r := result.Tool.Result
				content := r.Content
				if len(content) == 0 {
					content = Text("")
				}
				add(Message{Role: RoleTool, Content: content, ToolCallID: call.ID, Name: call.Name, IsError: r.IsError}, result.ID)
			}
		case EntryEnvironment:
			data, err := json.Marshal(e.Environment)
			if err != nil {
				return Context{}, err
			}
			add(Message{Role: RoleContext, ContextKind: "environment", Content: Text(string(data))}, e.ID)
		}
	}
	if err = validateProtocol(view.Request.Messages); err != nil {
		return Context{}, err
	}
	return clone(view), nil
}

// Results may commit out of order within a batch, but a later message must
// never move ahead of an unfinished batch merely because projection sorts it.
func validateToolTimeline(path []Entry) error {
	pending := map[string]ToolCall{}
	completed := map[string]Entry{}
	for _, e := range path {
		switch e.Kind {
		case EntryMessage:
			if len(pending) > 0 {
				return fmt.Errorf("%w: message precedes outstanding tool results", ErrIncompleteBatch)
			}
			for _, call := range e.Message.ToolCalls {
				pending[resultKey(e.ID, call.ID)] = call
			}
		case EntryToolResult:
			key := resultKey(e.Tool.Invocation.AssistantID, e.Tool.Invocation.Call.ID)
			call, ok := pending[key]
			if !ok || !sameJSON(call, e.Tool.Invocation.Call) {
				return fmt.Errorf("%w: out-of-sequence tool result", ErrIncompleteBatch)
			}
			delete(pending, key)
			completed[key] = e
		case EntryToolResolution:
			key := resultKey(e.Tool.Invocation.AssistantID, e.Tool.Invocation.Call.ID)
			previous, ok := completed[key]
			if !ok || !previous.Tool.NeedsResolution || !sameJSON(previous.Tool.Invocation, e.Tool.Invocation) {
				return fmt.Errorf("%w: resolution without unresolved result", ErrIncompleteBatch)
			}
			completed[key] = e
		}
	}
	if len(pending) > 0 {
		return ErrIncompleteBatch
	}
	return nil
}

func validateProtocol(messages []Message) error {
	var pending []ToolCall
	for _, message := range messages {
		if err := validateMessage(message); err != nil {
			return err
		}
		if len(pending) > 0 {
			if message.Role != RoleTool || message.ToolCallID != pending[0].ID {
				return fmt.Errorf("%w: expected result %s", ErrIncompleteBatch, pending[0].ID)
			}
			pending = pending[1:]
			continue
		}
		if message.Role == RoleTool {
			return fmt.Errorf("%w: orphan tool result", ErrIncompleteBatch)
		}
		if message.Role == RoleAssistant && len(message.ToolCalls) > 0 {
			pending = message.ToolCalls
		}
	}
	if len(pending) > 0 {
		return ErrIncompleteBatch
	}
	return nil
}
