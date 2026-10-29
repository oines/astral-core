package astral

import (
	"context"
	"fmt"
)

// Context is the effective request plus provenance in the active branch. Its
// messages contain only the active window and its provenance.
type Context struct {
	anchor          *contextUsageAnchor
	HeadID          string         `json:"head_id"`
	ConfigurationID string         `json:"configuration_id"`
	Selection       ModelSelection `json:"selection"`
	Request         Request        `json:"request"`
	Sources         []string       `json:"sources"`
	EstimatedInput  int            `json:"estimated_input"`
}

// ReadContext returns the bounded active history projection without resolving a
// model, collecting environment context or estimating tokens. It can be used to
// inspect history or construct an independent model request after a connection
// has been removed. No complete rollout is read into memory.
func (s *Session) ReadContext(ctx context.Context) (Context, error) {
	if err := ctx.Err(); err != nil {
		return Context{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Context{}, ErrClosed
	}
	path := clone(s.pathLocked())
	s.mu.Unlock()
	return project(path)
}

// InspectContext prepares and estimates the current model's view without
// invoking it. ReadContext is the model-independent history API.
func (s *Session) InspectContext(ctx context.Context) (Context, error) {
	view, err := s.ReadContext(ctx)
	if err != nil {
		return Context{}, err
	}
	model, err := s.resolveModel(ctx, view.Selection)
	if err != nil {
		return Context{}, err
	}
	view.Request.Options = resolvedOptions(model)
	return s.measureContext(ctx, model, view)
}

func resultKey(assistantID, callID string) string { return assistantID + "\x00" + callID }

// project is the only normal history-to-context projection. Tool results are
// emitted in the assistant's call order as one contiguous batch. Context may
// only be committed at a complete batch boundary. Since
// no model request observes a half batch, previously sent prefixes stay stable.
func project(path []Entry) (Context, error) {
	configuration, configurationID, err := effectiveConfiguration(path)
	if err != nil {
		return Context{}, err
	}
	view := Context{ConfigurationID: configurationID, Selection: configuration.Selection, Request: Request{ModelID: configuration.Selection.ID, SystemPrompt: configuration.SystemPrompt, Tools: configuration.Tools, Options: configuration.Selection.Options}}
	if configuration.Selection.Reasoning != nil {
		view.Request.Options.Reasoning = *configuration.Selection.Reasoning
	}
	if len(path) > 0 {
		view.HeadID = path[len(path)-1].ID
	}
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
		view.Request.Messages = append(view.Request.Messages, Message{Role: RoleUser, ContextKind: "summary", Content: Text(checkpoint.Compaction.Summary)})
		view.Sources = append(view.Sources, checkpoint.ID)
	}
	visible := path[start:]
	if err := validateToolTimeline(visible); err != nil {
		return Context{}, err
	}
	results := map[string]Entry{}
	assistants := map[string]Entry{}
	controls := map[string]bool{}
	for _, e := range visible {
		if e.Kind == EntryMessage && e.Message.Role == RoleAssistant {
			assistants[e.ID] = e
		}
		if e.Kind == EntryToolResult || e.Kind == EntryToolResolution || e.Kind == EntryToolControl {
			key := resultKey(e.Tool.Invocation.AssistantID, e.Tool.Invocation.Call.ID)
			if prev, ok := results[key]; ok && e.Kind != EntryToolResolution {
				return Context{}, fmt.Errorf("%w: duplicate result for %s (%s)", ErrCorrupt, key, prev.ID)
			}
			results[key] = e
			if e.Kind == EntryToolControl {
				controls[e.Tool.Invocation.AssistantID] = true
			}
		}
	}
	for _, e := range results {
		assistant, ok := assistants[e.Tool.Invocation.AssistantID]
		if !ok {
			return Context{}, fmt.Errorf("%w: result without visible assistant", ErrIncompleteBatch)
		}
		matched := false
		for _, call := range assistant.Message.ToolCalls() {
			if call.ID == e.Tool.Invocation.Call.ID && sameJSON(call, e.Tool.Invocation.Call) {
				matched = true
				break
			}
		}
		if !matched {
			return Context{}, fmt.Errorf("%w: unmatched tool result", ErrIncompleteBatch)
		}
		if e.Kind == EntryToolControl {
			if len(assistant.Message.ToolCalls()) != 1 {
				return Context{}, fmt.Errorf("%w: control in mixed batch", ErrCorrupt)
			}
			for _, part := range assistant.Message.Content {
				if part.Type != "tool_call" && part.Type != "reasoning" {
					return Context{}, fmt.Errorf("%w: control with assistant output", ErrCorrupt)
				}
			}
		}
	}
	add := func(message Message, id string) {
		view.Request.Messages = append(view.Request.Messages, message)
		view.Sources = append(view.Sources, id)
	}
	for _, e := range visible {
		switch e.Kind {
		case EntryMessage:
			if e.Message.Failed() || controls[e.ID] {
				continue
			}
			if e.Message.Role == RoleTool {
				return Context{}, fmt.Errorf("%w: tool messages require a tool result entry", ErrCorrupt)
			}
			add(*e.Message, e.ID)
			for _, call := range e.Message.ToolCalls() {
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
		case EntryContext:
			add(*e.Message, e.ID)
		}
	}
	if err = validateProtocol(view.Request.Messages); err != nil {
		return Context{}, err
	}
	view = clone(view)
	view.anchor = contextUsageAnchorFor(path, view.Sources)
	return view, nil
}

func contextUsageAnchorFor(path []Entry, sources []string) *contextUsageAnchor {
	// Only a successful assistant created after the current compaction/config
	// may anchor this projection. Summarizer usage never appears here.
	entries := make(map[string]Entry, len(sources))
	for _, entry := range path {
		if entry.Kind == EntryCompaction {
			clear(entries)
			continue
		}
		entries[entry.ID] = entry
	}
	for i := len(sources) - 1; i >= 0; i-- {
		entry := entries[sources[i]]
		if entry.Generation != nil && entry.Message != nil && entry.Message.Role == RoleAssistant && !entry.Message.Failed() &&
			entry.Usage.Available && !entry.Usage.InputMissing && !entry.Usage.OutputMissing &&
			entry.Usage.InputTokens >= 0 && entry.Usage.OutputTokens >= 0 && (entry.Usage.InputTokens > 0 || entry.Usage.OutputTokens > 0) {
			return &contextUsageAnchor{index: i, usage: entry.Usage, generation: *entry.Generation}
		}
	}
	return nil
}

// Results may commit out of order within a batch, but a later message must
// never move ahead of an unfinished batch merely because projection sorts it.
func validateToolTimeline(path []Entry) error {
	pending := map[string]ToolCall{}
	completed := map[string]Entry{}
	for _, e := range path {
		switch e.Kind {
		case EntryMessage, EntryContext:
			if e.Message.Failed() {
				continue
			}
			if len(pending) > 0 {
				return fmt.Errorf("%w: message precedes outstanding tool results", ErrIncompleteBatch)
			}
			for _, call := range e.Message.ToolCalls() {
				pending[resultKey(e.ID, call.ID)] = call
			}
		case EntryToolResult:
			key := resultKey(e.Tool.Invocation.AssistantID, e.Tool.Invocation.Call.ID)
			call, ok := pending[key]
			if !ok || !sameJSON(call, e.Tool.Invocation.Call) || completed[key].Tool != nil {
				return fmt.Errorf("%w: out-of-sequence tool result", ErrIncompleteBatch)
			}
			delete(pending, key)
			completed[key] = e
		case EntryToolControl, EntryToolUnresolved:
			key := resultKey(e.Tool.Invocation.AssistantID, e.Tool.Invocation.Call.ID)
			call, ok := pending[key]
			previous := completed[key]
			if !ok || !sameJSON(call, e.Tool.Invocation.Call) || previous.Tool != nil && (e.Kind != EntryToolControl || previous.Kind != EntryToolUnresolved || !sameJSON(previous.Tool.Invocation, e.Tool.Invocation)) {
				return fmt.Errorf("%w: out-of-sequence control", ErrIncompleteBatch)
			}
			if e.Kind == EntryToolControl {
				delete(pending, key)
			}
			completed[key] = e
		case EntryToolResolution:
			key := resultKey(e.Tool.Invocation.AssistantID, e.Tool.Invocation.Call.ID)
			previous, ok := completed[key]
			if !ok || !previous.Tool.NeedsResolution || !sameJSON(previous.Tool.Invocation, e.Tool.Invocation) {
				return fmt.Errorf("%w: resolution without unresolved result", ErrIncompleteBatch)
			}
			if previous.Kind == EntryToolUnresolved {
				delete(pending, key)
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
		if message.Role == RoleAssistant {
			pending = message.ToolCalls()
		}
	}
	if len(pending) > 0 {
		return ErrIncompleteBatch
	}
	return nil
}
