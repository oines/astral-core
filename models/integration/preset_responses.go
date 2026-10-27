package integration

import models "github.com/oines/astral-core/models"

// Only documented differences are listed here. These affect automatically
// added request fields, never native response items or encrypted replay data.
// See docs/provider-reasoning.md for the audited service contracts.
func ResponsesCompatibility(presetID string) *models.ResponsesCompatibility {
	no := false
	switch presetID {
	case "deepseek":
		return &models.ResponsesCompatibility{SupportsInclude: &no, SupportsStore: &no}
	case "groq", "fireworks":
		return &models.ResponsesCompatibility{SupportsInclude: &no}
	case "moonshot":
		return &models.ResponsesCompatibility{SupportsEncryptedReasoningInclude: &no}
	default:
		switch presetID {
		case "openai", "azure-openai", "codex", "openrouter", "xai", "floway", "":
			return nil
		}
		// These services do not document OpenAI's encrypted reasoning include.
		// Omit the optional default; this is not a claim that the service has
		// been observed rejecting it or cannot return native reasoning items.
		if FindPreset(presetID) != nil {
			return &models.ResponsesCompatibility{SupportsInclude: &no}
		}
		return nil
	}
}
