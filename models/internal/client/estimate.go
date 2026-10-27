package client

import (
	"encoding/json"
	"fmt"

	astral "github.com/oines/astral-core"
)

// Estimate counts model-visible content, not the serialized HTTP envelope.
// It is a fallback approximation, shared by full requests, incremental tails,
// retained history and summaries. Provider usage remains the better anchor.
func (c *Client) Estimate(req astral.Request) (int, error) {
	tokens := 3 + textTokens(req.SystemPrompt)
	imageTokens := c.Config.ImageTokenBudget
	if imageTokens == 0 {
		imageTokens = 1200
	}
	if imageTokens < 0 {
		return 0, fmt.Errorf("%w: negative image token estimate", astral.ErrInvalid)
	}
	add := func(n int) error {
		if n < 0 || tokens > int(^uint(0)>>1)-n {
			return fmt.Errorf("%w: token estimate overflow", astral.ErrInvalid)
		}
		tokens += n
		return nil
	}
	for _, tool := range req.Tools {
		if err := add(8 + textTokens(tool.Name) + textTokens(tool.Description) + jsonTokens(tool.Parameters)); err != nil {
			return 0, err
		}
	}
	// Structured output schemas and provider options can contribute prompt text;
	// ordinary numeric request controls do not reserve an output-sized input.
	options, err := json.Marshal(req.Options)
	if err != nil {
		return 0, err
	}
	if err := add(jsonTokens(options)); err != nil {
		return 0, err
	}
	for _, message := range req.Messages {
		if err := add(6 + textTokens(message.Name) + textTokens(message.ToolCallID)); err != nil {
			return 0, err
		}
		for _, part := range message.Content {
			n := 0
			switch part.Type {
			case "image":
				n = imageTokens
			case "file":
				return 0, fmt.Errorf("%w: file inputs require a host-provided token estimator", astral.ErrInvalid)
			case "tool_call":
				if part.ToolCall != nil {
					n = 4 + textTokens(part.ToolCall.Name) + jsonTokens(part.ToolCall.Arguments)
				}
			default:
				n = textTokens(part.Text) + jsonTokens(part.Data)
			}
			if err := add(n); err != nil {
				return 0, err
			}
		}
	}
	return tokens, nil
}

// ASCII prose/code uses Pi's approximate four characters per token. Count
// non-ASCII characters separately so Chinese text is not treated as English;
// supplementary characters (including emoji) receive two tokens each.
func textTokens(text string) int {
	ascii, other := 0, 0
	for _, r := range text {
		switch {
		case r < 128:
			ascii++
		case r > 0xffff:
			other += 2
		default:
			other++
		}
	}
	return (ascii+3)/4 + other
}

func jsonTokens(raw json.RawMessage) int {
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return 0
	}
	// Normalize whitespace and JSON escaping before counting content.
	var value any
	if json.Unmarshal(raw, &value) == nil {
		if text, ok := value.(string); ok {
			return textTokens(text)
		}
		if compact, err := json.Marshal(value); err == nil {
			raw = compact
		}
	}
	return textTokens(string(raw))
}
