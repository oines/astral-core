package integration

import (
	"bytes"
	"encoding/json"
	"slices"
	"time"

	"github.com/oines/astral-core/models"
)

// ResolveReasoningSemantics binds declared model capabilities to the selected
// provider's API contract. It never identifies a provider from a model name,
// invents model support, or turns capability defaults into request options.
// Explicit null, false and empty lists remain authoritative.
func ResolveReasoningSemantics(provider ProviderConfig, protocol string, data json.RawMessage) (json.RawMessage, []models.ReasoningDiagnostic, error) {
	if len(data) == 0 || isNull(data) {
		return append(json.RawMessage(nil), data...), nil, nil
	}
	var fields map[string]json.RawMessage
	var caps models.ReasoningCapabilities
	if json.Unmarshal(data, &fields) != nil || fields == nil || json.Unmarshal(data, &caps) != nil {
		return nil, nil, invalid("invalid reasoning capabilities")
	}
	if caps.Supported != nil && !*caps.Supported {
		return append(json.RawMessage(nil), data...), nil, nil
	}
	declared := yes(caps.Supported) || yes(caps.Toggle) || yes(caps.Disable) || len(caps.Efforts) > 0 || yes(caps.Budget) || yes(caps.DynamicBudget)
	if !declared {
		return append(json.RawMessage(nil), data...), nil, nil
	}
	missing := func(key string, value any) {
		if _, present := fields[key]; !present {
			fields[key] = raw(value)
		}
	}
	switch provider.PresetID {
	case "mimo", "mimo-token-plan":
		if protocol == "openai-responses" {
			// MiMo accepts all non-none effort spellings as the same enabled
			// state. This binding belongs to MiMo's endpoint, not its models
			// when served through a gateway.
			if yes(caps.Toggle) {
				missing("on_effort", "high")
			}
			if yes(caps.Disable) {
				missing("off_effort", "none")
			}
			missing("default_on", true)
		}
	case "deepseek":
		if protocol == "openai-responses" {
			missing("efforts", []string{"low", "high", "max"})
			missing("default_effort", "high")
			missing("default_on", true)
			if yes(caps.Toggle) {
				active := caps.DefaultEffort
				if active == "" && (len(caps.Efforts) == 0 || slices.Contains(caps.Efforts, "high")) {
					active = "high"
				}
				if active != "" && active != "none" {
					missing("on_effort", active)
				}
			}
			if yes(caps.Disable) {
				missing("off_effort", "none")
			}
		}
	case "google":
		if protocol == "google-generative-ai" && yes(caps.Budget) {
			// -1 is a documented GenerateContent budget sentinel. It is
			// unrelated to a level called minimal or a model default.
			missing("dynamic_budget", true)
		}
	case "fireworks":
		if protocol == "anthropic-messages" {
			// Fireworks translates budget_tokens to effort bands. That API
			// cannot promise an exact budget, even when the underlying
			// model's public record offers one on another endpoint.
			// An explicit conflicting capability is retained so controls
			// diagnose it and invocation rejects it. User overrides must
			// never be silently rewritten by an API fallback.
			missing("budget", false)
			missing("dynamic_budget", false)
			if yes(caps.Toggle) && slices.Contains([]string{"low", "medium", "high", "max", "xhigh"}, caps.DefaultEffort) {
				missing("on_effort", caps.DefaultEffort)
			}
		}
	}
	result, err := json.Marshal(fields)
	return result, nil, err
}

func yes(value *bool) bool { return value != nil && *value }

// NormalizeDiscoveryReasoning applies exactly the same API binding as Resolve.
// Catalog enrichment calls this after sparse live/public merging, so provider
// semantics cannot replace an explicit unknown. Resolve repeats it safely for
// manually configured models and unsaved previews.
func NormalizeDiscoveryReasoning(provider ProviderConfig, discovery Discovery) Discovery {
	protocol, _, _, err := Route(provider, ModelConfig{ModelID: discovery.ID, Protocol: discovery.Protocol, Dialect: discovery.Dialect})
	if err != nil {
		return discovery
	}
	before := discovery.Recommendations["reasoning"]
	after, _, err := ResolveReasoningSemantics(provider, protocol, before)
	if err != nil || len(after) == 0 || bytes.Equal(before, after) {
		return discovery
	}
	copy := make(map[string]json.RawMessage, len(discovery.Recommendations))
	for field, value := range discovery.Recommendations {
		copy[field] = append(json.RawMessage(nil), value...)
	}
	copy["reasoning"] = after
	discovery.Recommendations = copy
	var old, current map[string]json.RawMessage
	_ = json.Unmarshal(before, &old)
	_ = json.Unmarshal(after, &current)
	sources := make(map[string]RecommendationSource, len(discovery.RecommendationSources))
	for field, source := range discovery.RecommendationSources {
		sources[field] = source
	}
	for field, value := range current {
		if !bytes.Equal(old[field], value) {
			sources["reasoning."+field] = RecommendationSource{Source: reasoningContractSource(provider.PresetID), UpdatedAt: time.Now().UTC()}
		}
	}
	discovery.RecommendationSources = sources
	return discovery
}

func reasoningContractSource(preset string) string {
	switch preset {
	case "mimo", "mimo-token-plan":
		return "https://mimo.mi.com/docs/en-US/api/chat/responses"
	case "deepseek":
		return "https://api-docs.deepseek.com/guides/thinking_mode/"
	case "google":
		return "https://ai.google.dev/gemini-api/docs/generate-content/thinking"
	case "fireworks":
		return "https://docs.fireworks.ai/tools-sdks/anthropic-compatibility"
	default:
		return "provider-api-contract"
	}
}
