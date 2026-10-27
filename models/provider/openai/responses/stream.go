package responses

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/models/internal/client"
	"github.com/oines/astral-core/models/internal/errorformat"
	"github.com/oines/astral-core/models/internal/usage"
	"github.com/oines/astral-core/models/internal/utils"
)

type response struct {
	ID, Model, Status string
	Output            []json.RawMessage        `json:"output"`
	Usage             *usage.OpenAI            `json:"usage"`
	IncompleteDetails *struct{ Reason string } `json:"incomplete_details"`
	Error             json.RawMessage          `json:"error"`
}

type streamItem struct {
	part astral.Content
	id   string
	done bool
}

type stream struct {
	client                       *client.Client
	emit                         func(astral.ModelEvent)
	items                        map[int]*streamItem
	id, model, requestID, status string
	reason                       astral.FinishReason
	usage                        astral.Usage
	terminal                     bool
}

func newStream(c *client.Client, emit func(astral.ModelEvent)) *stream {
	if emit == nil {
		emit = func(astral.ModelEvent) {}
	}
	return &stream{client: c, emit: emit, items: map[int]*streamItem{}}
}

func (s *stream) accept(index int, raw json.RawMessage, complete bool) error {
	if index < 0 || index >= 4096 {
		return fmt.Errorf("%w: output index outside supported bounds", astral.ErrIncompleteResponse)
	}
	part, err := decodeItem(raw, complete)
	if err != nil {
		// Keep malformed provider data for diagnosis, never as executable calls.
		s.items[index] = &streamItem{part: astral.Content{Type: "provider", Data: raw, Metadata: s.client.Metadata("responses.item")}}
		return err
	}
	part.Metadata = s.client.Metadata("responses.item")
	var identity struct{ ID string }
	_ = json.Unmarshal(raw, &identity)
	previous, exists := s.items[index]
	if exists {
		if previous.part.Type != part.Type || (previous.id != "" && previous.id != identity.ID) {
			return fmt.Errorf("%w: output item identity changed during streaming", astral.ErrIncompleteResponse)
		}
		if a, b := previous.part.ToolCall, part.ToolCall; a != nil && b != nil {
			if a.ID != "" && a.ID != b.ID || a.Name != "" && a.Name != b.Name {
				return fmt.Errorf("%w: function-call identity changed during streaming", astral.ErrIncompleteResponse)
			}
		}
	} else {
		data, _ := json.Marshal(map[string]any{"type": part.Type})
		s.emit(astral.ModelEvent{Type: "content_start", ContentIndex: &index, Data: data, Metadata: part.Metadata})
	}
	s.items[index] = &streamItem{part: part, id: identity.ID, done: complete}
	return nil
}

func (s *stream) process(event *utils.SSEEvent) error {
	var value struct {
		Type        string          `json:"type"`
		OutputIndex *int            `json:"output_index"`
		ItemID      string          `json:"item_id"`
		Item        json.RawMessage `json:"item"`
		Delta       string          `json:"delta"`
		Response    *response       `json:"response"`
		Error       json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(event.Data), &value); err != nil {
		return fmt.Errorf("decode Responses event: %w", err)
	}
	kind := value.Type
	if kind == "" {
		kind = event.Event
	}
	if value.Response != nil {
		if value.Response.ID != "" {
			if s.id != "" && s.id != value.Response.ID {
				return fmt.Errorf("%w: response ID changed", astral.ErrIncompleteResponse)
			}
			s.id = value.Response.ID
		}
		if value.Response.Model != "" {
			s.model = value.Response.Model
		}
	}
	switch kind {
	case "error":
		return utils.NewBodyError(Protocol, event.Header, []byte(event.Data), errorformat.DecodeOpenAIErrorEvent)
	case "response.failed":
		return utils.NewBodyError(Protocol, event.Header, []byte(event.Data), errorformat.DecodeOpenAIFailedEvent)
	case "response.completed", "response.incomplete":
		if value.Response == nil {
			return fmt.Errorf("%w: terminal event has no response", astral.ErrIncompleteResponse)
		}
		if kind == "response.incomplete" && value.Response.Status == "" {
			value.Response.Status = "incomplete"
		}
		if err := s.complete(*value.Response); err != nil {
			return err
		}
		return utils.ErrStreamDone
	case "response.output_item.added", "response.output_item.done":
		if value.OutputIndex == nil {
			return fmt.Errorf("%w: output item has no index", astral.ErrIncompleteResponse)
		}
		return s.accept(*value.OutputIndex, value.Item, kind == "response.output_item.done")
	case "response.output_text.delta", "response.refusal.delta", "response.reasoning_summary_text.delta",
		"response.reasoning_text.delta", "response.function_call_arguments.delta":
		if value.OutputIndex == nil {
			return fmt.Errorf("%w: delta has no output index", astral.ErrIncompleteResponse)
		}
		index := *value.OutputIndex
		item := s.items[index]
		if item == nil || item.done || (item.id != "" && value.ItemID != "" && item.id != value.ItemID) {
			return fmt.Errorf("%w: delta has no matching active output item", astral.ErrIncompleteResponse)
		}
		eventType, partType := "text_delta", "text"
		switch kind {
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			eventType, partType = "reasoning_delta", "reasoning"
		case "response.function_call_arguments.delta":
			eventType, partType = "tool_call_delta", "tool_call"
		}
		if item.part.Type != partType {
			return fmt.Errorf("%w: delta does not match its output item", astral.ErrIncompleteResponse)
		}
		update := astral.ModelEvent{Type: eventType, ContentIndex: &index, Text: value.Delta}
		if item.part.ToolCall != nil {
			update.ToolCallID = item.part.ToolCall.ID
			update.Data, _ = json.Marshal(map[string]any{"name": item.part.ToolCall.Name})
		} else {
			item.part.Text += value.Delta
		}
		s.emit(update)
	}
	return nil
}

func (s *stream) complete(value response) error {
	if hasError(value.Error) {
		return fmt.Errorf("%w: terminal response contains an error: %s", astral.ErrIncompleteResponse, value.Error)
	}
	s.id, s.model, s.status = value.ID, value.Model, value.Status
	if value.Usage != nil {
		s.usage = value.Usage.Responses()
	}
	if value.Output != nil {
		for index := range s.items {
			if index >= len(value.Output) {
				return fmt.Errorf("%w: terminal output lost an emitted item", astral.ErrIncompleteResponse)
			}
		}
		for i, raw := range value.Output {
			if err := s.accept(i, raw, true); err != nil {
				return err
			}
		}
	}
	s.reason = astral.FinishStop
	switch value.Status {
	case "completed":
		for _, item := range s.items {
			if item.part.ToolCall != nil {
				s.reason = astral.FinishToolCalls
			}
		}
	case "incomplete":
		if value.IncompleteDetails == nil || value.IncompleteDetails.Reason != "max_output_tokens" {
			return fmt.Errorf("%w: incomplete Responses result", astral.ErrIncompleteResponse)
		}
		s.reason = astral.FinishLength
	default:
		return fmt.Errorf("%w: response has nonterminal status %q", astral.ErrIncompleteResponse, value.Status)
	}
	for _, item := range s.items {
		if !item.done {
			return fmt.Errorf("%w: terminal response lacks a final output item", astral.ErrIncompleteResponse)
		}
	}
	s.terminal = true
	return nil
}

func (s *stream) finish(cause error) (astral.Reply, error) {
	parts := make([]astral.Content, 0, len(s.items))
	var err error
	for i := 0; i < len(s.items); i++ {
		item := s.items[i]
		if item == nil {
			err = fmt.Errorf("%w: missing output item "+strconv.Itoa(i), astral.ErrIncompleteResponse)
			break
		}
		parts = append(parts, item.part)
	}
	if cause == nil && !s.terminal && err == nil {
		err = &astral.ModelError{Code: "incomplete_stream", Message: "provider response ended before its terminal event",
			Cause: astral.ErrIncompleteResponse, Transient: true}
	}
	reason := s.reason
	if cause != nil || err != nil {
		reason = astral.FinishError
		// Invalid/incomplete tool calls must remain non-executable diagnostics.
		for i, part := range parts {
			if part.ToolCall != nil {
				part.Type, part.ToolCall = "provider", nil
				parts[i] = part
			}
		}
	}
	if len(parts) == 0 && reason != astral.FinishError {
		parts = []astral.Content{{Type: "text"}}
	}
	reply := astral.Reply{Message: astral.Message{Role: astral.RoleAssistant, Content: parts, FinishReason: reason,
		Metadata: s.client.ResponseMetadata(s.id, s.requestID, s.model, s.status)}, Usage: s.usage}
	if cause == nil && err == nil {
		if validation := reply.Message.Validate(); validation != nil {
			reply.Message.FinishReason = astral.FinishError
			return reply, validation
		}
		for i := range parts {
			index := i
			s.emit(astral.ModelEvent{Type: "content_end", ContentIndex: &index})
		}
	}
	if cause != nil {
		reply.Message.ErrorMessage = strings.TrimSpace(cause.Error())
	} else if err != nil {
		reply.Message.ErrorMessage = err.Error()
	}
	return reply, err
}
