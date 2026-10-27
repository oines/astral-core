package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/oines/astral-core/models"
)

// RecommendationTarget records which model and API route supplied a set of
// recommendations. It contains no effective configuration or credentials.
type RecommendationTarget struct {
	ModelID  string `json:"model_id"`
	Protocol string `json:"protocol"`
	Dialect  string `json:"dialect,omitempty"`
	Contract string `json:"contract"`
}

// RecommendationTargetFor uses the same route resolution as invocation.
// Catalog callers supply Discovery.ID, Protocol and Dialect as ModelConfig.
func RecommendationTargetFor(provider ProviderConfig, model ModelConfig) (RecommendationTarget, error) {
	protocol, dialect, endpoint, err := Route(provider, model)
	if err != nil {
		return RecommendationTarget{}, err
	}
	format := provider.ReasoningFormat
	if protocol != provider.Protocol {
		format = ""
	}
	if format == "" {
		format = ReasoningFormat(provider.PresetID, protocol)
	}
	var mapping *models.ReasoningMapping
	if format == models.ReasoningFormatCustom {
		mapping = provider.ReasoningMapping
	}
	encoded, err := json.Marshal([]any{provider.PresetID, strings.TrimRight(endpoint, "/"), provider.ConnectionScope, format, mapping})
	if err != nil {
		return RecommendationTarget{}, err
	}
	// Canonicalize embedded mapping JSON as well as its surrounding struct.
	var identity any
	if err = json.Unmarshal(encoded, &identity); err != nil {
		return RecommendationTarget{}, err
	}
	encoded, err = json.Marshal(identity)
	if err != nil {
		return RecommendationTarget{}, err
	}
	digest := sha256.Sum256(encoded)
	return RecommendationTarget{ModelID: model.ModelID, Protocol: protocol, Dialect: dialect, Contract: hex.EncodeToString(digest[:])}, nil
}

// RebaseModelConfig invalidates only recommendations acquired for another
// identity or route. New catalog facts carry their new target and survive.
// Explicit overrides are never changed; Resolve validates them on the target.
// A manual configuration without catalog provenance is bound on first use.
func RebaseModelConfig(provider ProviderConfig, model ModelConfig) (ModelConfig, error) {
	target, err := RecommendationTargetFor(provider, model)
	if err != nil {
		return model, err
	}
	previous := model.RecommendationTarget
	if previous != nil && (previous.ModelID == "" || previous.Protocol == "") {
		return model, invalid("a recommendation target requires a model ID and protocol")
	}
	switch {
	case previous == nil:
		model.Recommendations = rebaseRecommendations(model.Recommendations, false)
	case previous.ModelID != target.ModelID:
		model.Recommendations = nil
	default:
		model.Recommendations = rebaseRecommendations(model.Recommendations, previous.Protocol != target.Protocol || previous.Dialect != target.Dialect || previous.Contract != target.Contract)
	}
	model.RecommendationTarget = &target
	return model, nil
}

// RebaseRecommendations removes facts tied to a previously selected API route.
// General model limits and prices survive; explicit user overrides are separate
// and are never changed by this function.
func RebaseRecommendations(recommendations map[string]json.RawMessage, previousProtocol, nextProtocol string) map[string]json.RawMessage {
	return rebaseRecommendations(recommendations, previousProtocol != nextProtocol)
}

func rebaseRecommendations(recommendations map[string]json.RawMessage, routeChanged bool) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for field, value := range recommendations {
		if routeChanged {
			switch field {
			case "reasoning", "reasoning_format", "reasoning_mapping", "reasoning_replay", "require_reasoning_replay", "streaming", "defaults":
				continue
			}
		}
		out[field] = append(json.RawMessage(nil), value...)
	}
	return out
}

// RebaseSources retains provenance only for fields still present in the rebased
// recommendation data. It returns a new map and does not touch user overrides.
func RebaseSources(sources map[string]RecommendationSource, recommendations map[string]json.RawMessage) map[string]RecommendationSource {
	flat := flattenRecommendations(recommendations)
	out := map[string]RecommendationSource{}
	for field, source := range sources {
		if _, exists := flat[field]; exists {
			out[field] = source
		}
	}
	return out
}
