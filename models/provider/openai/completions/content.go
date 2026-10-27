package completions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	astral "github.com/oines/astral-core"
)

// Chat-compatible APIs may stream structured thinking/text chunks before
// switching back to string deltas. Native blocks stay intact in durable data;
// display text is extracted separately and never used to rebuild thinking.
func (s *stream) appendContent(raw json.RawMessage) error {
	data := bytes.TrimSpace(raw)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}
	if data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		if text == "" {
			return nil
		}
		if s.nativeContent {
			block, _ := json.Marshal(map[string]string{"type": "text", "text": text})
			return s.appendNativeContent(block)
		}
		if s.text < 0 {
			s.text = s.add(astral.Content{Type: "text"})
		}
		s.parts[s.text].Text += text
		s.delta(s.text, "text_delta", text)
		return nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(data, &blocks); err != nil || data[0] != '[' {
		return fmt.Errorf("%w: chat content must be a string or a chunk array", astral.ErrIncompleteResponse)
	}
	s.nativeContent = true
	for _, block := range blocks {
		if err := s.appendNativeContent(block); err != nil {
			return err
		}
	}
	return nil
}

func (s *stream) appendNativeContent(raw json.RawMessage) error {
	var chunk struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(raw, &chunk); err != nil || chunk.Type == "" {
		return fmt.Errorf("%w: invalid structured chat content", astral.ErrIncompleteResponse)
	}
	kind, event, text := "provider", "", ""
	switch chunk.Type {
	case "text":
		kind, event, text = "text", "text_delta", chunk.Text
	case "thinking":
		kind, event = "reasoning", "reasoning_delta"
		var display strings.Builder
		for _, child := range chunk.Thinking {
			if child.Type == "text" {
				display.WriteString(child.Text)
			}
		}
		text = display.String()
	}
	// Group adjacent chunks only for display. Raw chunks retain their exact
	// order and all opaque fields, including future signatures and IDs.
	if s.nativeIndex < 0 || s.nativeIndex != len(s.parts)-1 || s.nativeKind != chunk.Type {
		s.nativeIndex = s.add(astral.Content{Type: kind, Metadata: s.client.Metadata("chat.content")})
		s.nativeKind = chunk.Type
	}
	index := s.nativeIndex
	s.nativeParts[index] = append(s.nativeParts[index], append(json.RawMessage(nil), raw...))
	s.parts[index].Text += text
	if event != "" && text != "" {
		s.delta(index, event, text)
	}
	return nil
}
