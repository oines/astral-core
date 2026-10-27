package completions

import (
	"encoding/json"
	"fmt"
	"strings"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/models/internal/client"
)

type stream struct {
	client                   *client.Client
	emit                     func(astral.ModelEvent)
	parts                    []astral.Content
	text, reasoning, refusal int
	calls                    map[int]int
	arguments                map[int]string
	reasoningContent         *string
	reasoningText            string
	details                  []chatReasoningDetail
	nativeContent            bool
	nativeParts              map[int][]json.RawMessage
	nativeIndex              int
	nativeKind               string
	usage                    astral.Usage
	id, model, requestID     string
	rawFinish                string
	done, sawChoice          bool
}

func newStream(c *client.Client, emit func(astral.ModelEvent)) *stream {
	if emit == nil {
		emit = func(astral.ModelEvent) {}
	}
	return &stream{client: c, emit: emit, text: -1, reasoning: -1, refusal: -1,
		calls: map[int]int{}, arguments: map[int]string{}, nativeIndex: -1, nativeParts: map[int][]json.RawMessage{}}
}

func (s *stream) add(part astral.Content) int {
	index := len(s.parts)
	s.parts = append(s.parts, part)
	data, _ := json.Marshal(map[string]any{"type": part.Type})
	s.emit(astral.ModelEvent{Type: "content_start", ContentIndex: &index, Data: data, Metadata: part.Metadata})
	return index
}

func (s *stream) delta(index int, kind, text string) {
	event := astral.ModelEvent{Type: kind, ContentIndex: &index, Text: text}
	if call := s.parts[index].ToolCall; call != nil {
		event.ToolCallID = call.ID
		event.Data, _ = json.Marshal(map[string]any{"name": call.Name})
	}
	s.emit(event)
}

func (s *stream) process(chunk chatChunkResponse) error {
	if s.id == "" {
		s.id, s.model = chunk.ID, chunk.Model
	}
	if chunk.Usage != nil {
		s.usage = chunk.Usage.Chat()
	}
	if len(chunk.Choices) == 0 {
		return nil
	}
	if len(chunk.Choices) != 1 || chunk.Choices[0].Index != 0 {
		return fmt.Errorf("%w: expected one chat choice", astral.ErrIncompleteResponse)
	}
	s.sawChoice = true
	choice := chunk.Choices[0]
	delta := choice.Delta
	if delta.ReasoningContent != nil || delta.Reasoning != "" || len(delta.ReasoningDetails) > 0 {
		if s.reasoning < 0 {
			s.reasoning = s.add(astral.Content{Type: "reasoning", Metadata: s.client.Metadata("chat.reasoning")})
		}
		previous := s.parts[s.reasoning].Text
		if delta.ReasoningContent != nil {
			if s.reasoningContent == nil {
				value := ""
				s.reasoningContent = &value
			}
			*s.reasoningContent += *delta.ReasoningContent
		}
		s.reasoningText += delta.Reasoning
		s.details = mergeReasoningDetails(s.details, delta.ReasoningDetails)
		text := s.reasoningText
		if s.reasoningContent != nil {
			text = *s.reasoningContent
		} else if text == "" {
			text = detailText(s.details)
		}
		s.parts[s.reasoning].Text = text
		if strings.HasPrefix(text, previous) && len(text) > len(previous) {
			s.delta(s.reasoning, "reasoning_delta", strings.TrimPrefix(text, previous))
		}
		// This remains live until the terminal event. Encrypted details arriving
		// after answer text still update the same durable reasoning block.
	}
	if err := s.appendContent(delta.Content); err != nil {
		return err
	}
	if delta.Refusal != "" {
		if s.refusal < 0 {
			s.refusal = s.add(astral.Content{Type: "refusal"})
		}
		s.parts[s.refusal].Text += delta.Refusal
		s.delta(s.refusal, "refusal_delta", delta.Refusal)
	}
	for _, fragment := range delta.ToolCalls {
		if fragment.Index < 0 || (fragment.Type != "" && fragment.Type != "function") {
			return fmt.Errorf("%w: invalid tool-call fragment", astral.ErrIncompleteResponse)
		}
		index, exists := s.calls[fragment.Index]
		if !exists {
			index = s.add(astral.Content{Type: "tool_call", ToolCall: &astral.ToolCall{Arguments: json.RawMessage(`{}`)}})
			s.calls[fragment.Index] = index
		}
		call := s.parts[index].ToolCall
		if fragment.ID != "" {
			if call.ID != "" && call.ID != fragment.ID {
				return fmt.Errorf("%w: tool-call ID changed during streaming", astral.ErrIncompleteResponse)
			}
			call.ID = fragment.ID
		}
		if fragment.Function.Name != "" {
			if call.Name != "" && call.Name != fragment.Function.Name {
				return fmt.Errorf("%w: tool name changed during streaming", astral.ErrIncompleteResponse)
			}
			call.Name = fragment.Function.Name
		}
		s.arguments[index] += fragment.Function.Arguments
		if fragment.Function.Arguments != "" {
			s.delta(index, "tool_call_delta", fragment.Function.Arguments)
		}
	}
	for _, image := range delta.Images {
		raw, _ := json.Marshal(image)
		s.add(astral.Content{Type: "image", URL: image.ImageURL.URL, Data: raw, Metadata: s.client.Metadata("chat.image")})
	}
	if choice.FinishReason != nil && *choice.FinishReason != "" {
		s.rawFinish = *choice.FinishReason
		s.done = true
	}
	return nil
}

func (s *stream) finish(success bool) (astral.Reply, error) {
	for index, blocks := range s.nativeParts {
		s.parts[index].Data, _ = json.Marshal(map[string]any{"content": blocks})
	}
	if s.reasoning >= 0 {
		wire := map[string]any{}
		if s.reasoningContent != nil {
			wire["reasoning_content"] = *s.reasoningContent
		}
		if s.reasoningText != "" {
			wire["reasoning"] = s.reasoningText
		}
		if s.details != nil {
			wire["reasoning_details"] = s.details
		}
		s.parts[s.reasoning].Data, _ = json.Marshal(wire)
	}
	var err error
	for index, arguments := range s.arguments {
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		if !json.Valid([]byte(arguments)) {
			call := s.parts[index].ToolCall
			data, _ := json.Marshal(map[string]any{"type": "incomplete_tool_call", "id": call.ID, "name": call.Name, "arguments": arguments})
			s.parts[index] = astral.Content{Type: "provider", Data: data, Metadata: s.client.Metadata("chat.incomplete")}
			err = fmt.Errorf("%w: tool arguments are not complete JSON", astral.ErrIncompleteResponse)
			continue
		}
		s.parts[index].ToolCall.Arguments = json.RawMessage(arguments)
	}
	reason := astral.FinishStop
	switch s.rawFinish {
	case "", "stop", "content_filter":
		if len(s.calls) > 0 && s.rawFinish == "" {
			reason = astral.FinishToolCalls
		}
	case "tool_calls", "function_call":
		reason = astral.FinishToolCalls
	case "length":
		reason = astral.FinishLength
	default:
		err = fmt.Errorf("%w: unknown finish reason %q", astral.ErrIncompleteResponse, s.rawFinish)
	}
	if success && (!s.done || !s.sawChoice) {
		err = &astral.ModelError{Code: "incomplete_stream", Message: "provider response ended without a complete result",
			Cause: astral.ErrIncompleteResponse, Transient: true}
	}
	if !success || err != nil {
		reason = astral.FinishError
	}
	if len(s.parts) == 0 && reason != astral.FinishError {
		s.parts = append(s.parts, astral.Content{Type: "text"})
	}
	reply := astral.Reply{Message: astral.Message{Role: astral.RoleAssistant, Content: s.parts, FinishReason: reason,
		Metadata: s.client.ResponseMetadata(s.id, s.requestID, s.model, s.rawFinish)}, Usage: s.usage}
	if success && err == nil {
		if validation := reply.Message.Validate(); validation != nil {
			reply.Message.FinishReason = astral.FinishError
			return reply, validation
		}
		if reason == astral.FinishToolCalls && len(reply.Message.ToolCalls()) == 0 {
			reply.Message.FinishReason = astral.FinishError
			return reply, fmt.Errorf("%w: tool_calls finish without any calls", astral.ErrIncompleteResponse)
		}
		for index := range s.parts {
			i := index
			s.emit(astral.ModelEvent{Type: "content_end", ContentIndex: &i})
		}
	}
	return reply, err
}

// Ported from Twilight's reasoning-details merger. Index is optional and is not
// a stable cross-chunk identity. Only adjacent text/summary fragments merge;
// opaque/encrypted entries always retain their individual wire order.
func mergeReasoningDetails(acc, delta []chatReasoningDetail) []chatReasoningDetail {
	for _, detail := range delta {
		kind, _ := detail["type"].(string)
		field := ""
		switch kind {
		case "reasoning.text":
			field = "text"
		case "reasoning.summary":
			field = "summary"
		}
		if field == "" || len(acc) == 0 || acc[len(acc)-1]["type"] != kind {
			copy := chatReasoningDetail{}
			for key, value := range detail {
				copy[key] = value
			}
			acc = append(acc, copy)
			continue
		}
		target := acc[len(acc)-1]
		for key, value := range detail {
			piece, stringValue := value.(string)
			switch {
			case key == field && stringValue:
				previous, _ := target[key].(string)
				target[key] = previous + piece
			case stringValue && piece == "":
				if _, exists := target[key]; !exists {
					target[key] = value
				}
			default:
				target[key] = value
			}
		}
	}
	return acc
}

func detailText(details []chatReasoningDetail) string {
	var text strings.Builder
	for _, detail := range details {
		if value, ok := detail["text"].(string); ok {
			text.WriteString(value)
		} else if value, ok := detail["summary"].(string); ok {
			text.WriteString(value)
		}
	}
	return text.String()
}
