package integration

import models "github.com/oines/astral-core/models"

// presetReasoningBindings is deliberately exhaustive. A binding selects the
// wire format, not the controls any particular model supports. Keep the audit
// in docs/provider-reasoning.md and its coverage test in sync with this table.
var presetReasoningBindings = map[string]models.ReasoningFormat{
	"anthropic":           models.ReasoningFormatAnthropic,
	"azure-openai":        models.ReasoningFormatOpenAIResponses,
	"cerebras":            models.ReasoningFormatOpenAIChat,
	"cloudflare":          models.ReasoningFormatOpenAIChat,
	"codex":               models.ReasoningFormatOpenAIResponses,
	"deepseek":            models.ReasoningFormatOpenAIResponses,
	"fireworks":           models.ReasoningFormatOpenAIResponses,
	"github-copilot":      models.ReasoningFormatOpenAIChat,
	"google":              models.ReasoningFormatGemini,
	"groq":                models.ReasoningFormatOpenAIResponses,
	"huggingface":         models.ReasoningFormatOpenAIResponses,
	"lmstudio":            models.ReasoningFormatOpenAIResponses,
	"minimax":             models.ReasoningFormatAnthropicAdaptive,
	"mistral":             models.ReasoningFormatOpenAIChat,
	"moonshot":            models.ReasoningFormatThinkingType,
	"newapi":              models.ReasoningFormatOpenAIResponses,
	"ollama":              models.ReasoningFormatOpenAIResponses,
	"openai":              models.ReasoningFormatOpenAIResponses,
	"openrouter":          models.ReasoningFormatOpenAIResponses,
	"perplexity":          models.ReasoningFormatOpenAIResponses,
	"qwen":                models.ReasoningFormatOpenAIResponses,
	"together":            models.ReasoningFormatTogether,
	"xai":                 models.ReasoningFormatOpenAIResponses,
	"zai":                 models.ReasoningFormatThinkingType,
	"zhipu":               models.ReasoningFormatOpenAIResponses,
	"commandcode":         models.ReasoningFormatOpenAIResponses,
	"opencode":            models.ReasoningFormatOpenAIChat,
	"opencode-go":         models.ReasoningFormatOpenAIChat,
	"mimo":                models.ReasoningFormatOpenAIResponses,
	"mimo-token-plan":     models.ReasoningFormatOpenAIResponses,
	"minimax-token-plan":  models.ReasoningFormatAnthropicAdaptive,
	"zai-coding-plan":     models.ReasoningFormatThinkingType,
	"zhipu-coding-plan":   models.ReasoningFormatOpenAIResponses,
	"bailian-coding-plan": models.ReasoningFormatQwen,
	"kimi-code":           models.ReasoningFormatOpenAIResponses,
	"floway":              models.ReasoningFormatOpenAIResponses,
	"litellm":             models.ReasoningFormatOpenAIResponses,
	"magpie":              models.ReasoningFormatOpenAIResponses,
}

func applyPresetReasoning(p *ProviderPreset) {
	p.Protocols = presetSupportedProtocols(p.ID)
	p.ReasoningFormat = presetReasoningBindings[p.ID]
	if p.ReasoningFormat == "" {
		p.ReasoningFormat = ReasoningFormat(p.ID, p.Protocol)
	}
}

// presetReasoningFormat is a provider route rule, never a model-name heuristic.
// Caller resolves explicit model/provider overrides before this fallback.
// Changing protocol must not retain another protocol's extension fields.
func ReasoningFormat(presetID, protocol string) models.ReasoningFormat {
	switch protocol {
	case "openai-responses":
		return models.ReasoningFormatOpenAIResponses
	case "anthropic-messages":
		switch presetID {
		case "minimax", "minimax-token-plan", "fireworks":
			return models.ReasoningFormatAnthropicAdaptive
		default:
			return models.ReasoningFormatAnthropic
		}
	case "google-generative-ai":
		return models.ReasoningFormatGemini
	case "openai-completions":
		switch presetID {
		case "deepseek", "zai", "zhipu", "zai-coding-plan", "zhipu-coding-plan", "mimo", "mimo-token-plan":
			return models.ReasoningFormatThinkingType
		case "moonshot":
			return models.ReasoningFormatThinkingType
		case "qwen", "bailian-coding-plan":
			return models.ReasoningFormatQwen
		case "openrouter":
			return models.ReasoningFormatOpenRouter
		case "together":
			return models.ReasoningFormatTogether
		case "fireworks":
			return models.ReasoningFormatFireworks
		default:
			return models.ReasoningFormatOpenAIChat
		}
	default:
		return models.ReasoningFormatProtocolDefault
	}
}
