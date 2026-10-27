package models

import (
	"errors"
	"strings"
)

// IsContextOverflow recognizes provider context-limit failures only. A generic
// 400/413, rate limit, quota error, or local validation failure is insufficient.
// Providers do not all expose a machine-readable code, so known context/input
// wording is accepted only on a suitable API error response.
func IsContextOverflow(err error) bool {
	var api *APIError
	if !errors.As(err, &api) {
		return false
	}
	if api.Kind == KindContextOverflow {
		return true
	}
	if api.Kind != "" && api.Kind != KindUnknown {
		return false
	}
	if api.StatusCode != 0 && api.StatusCode != 400 && api.StatusCode != 413 && api.StatusCode != 422 {
		return false
	}
	for _, code := range []string{api.Code, api.Type} {
		switch strings.ToLower(code) {
		case "context_length_exceeded", "context_window_exceeded", "model_context_window_exceeded", "prompt_too_long", "input_too_long", "too_many_tokens", "input_token_limit_exceeded":
			return true
		}
	}
	message := strings.ToLower(api.Message)
	for _, phrase := range []string{"rate limit", "too many requests", "throttling", "service unavailable"} {
		if strings.Contains(message, phrase) {
			return false
		}
	}
	for _, phrase := range []string{"maximum context length", "context length exceeded", "context_length_exceeded", "context window exceeded", "exceeds the context window", "prompt is too long", "prompt too long", "prompt exceeds max length", "input is too long", "maximum prompt length", "reduce the length of the messages", "maximum allowed input length", "exceeds the available context size", "greater than the context length", "context window exceeds limit", "exceeded model token limit", "range of input length should be", "input token count exceeds", "input token count (", "exceeds the model's maximum context", "exceeds the maximum number of tokens"} {
		if strings.Contains(message, phrase) {
			if phrase == "input token count (" {
				return strings.Contains(message, "exceeds") && strings.Contains(message, "maximum")
			}
			return true
		}
	}
	return false
}
