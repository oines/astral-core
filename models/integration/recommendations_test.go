package integration

import (
	"encoding/json"
	"reflect"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models"
)

func TestRecommendationBindingSeparatesModelAndRouteChanges(t *testing.T) {
	provider := ProviderConfig{Protocol: "openai-completions", Dialect: "compatible"}
	facts := map[string]json.RawMessage{
		"context_window": raw(8192), "max_output_tokens": raw(1024), "input_price": raw(1),
		"reasoning": raw(map[string]any{"efforts": []string{"high"}}), "streaming": raw(false), "defaults": raw(map[string]any{"temperature": .2}),
	}
	old, err := RecommendationTargetFor(provider, ModelConfig{ModelID: "vendor/old", Protocol: "openai-completions", Dialect: "compatible"})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := RecommendationTargetFor(provider, ModelConfig{ModelID: "vendor/new", Protocol: "openai-responses", Dialect: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	overrides := map[string]json.RawMessage{"reasoning": raw(map[string]any{"efforts": []string{"user-level"}})}
	for _, test := range []struct {
		name, modelID, protocol, dialect string
		target                           *RecommendationTarget
		wantFields                       int
	}{
		{"manual first binding", "vendor/old", "openai-completions", "compatible", nil, len(facts)},
		{"same model and route", "vendor/old", "openai-completions", "compatible", &old, len(facts)},
		{"different model", "vendor/new", "openai-completions", "compatible", &old, 0},
		{"different protocol", "vendor/old", "openai-responses", "openai", &old, 3},
		{"different dialect", "vendor/old", "openai-completions", "qwen", &old, 3},
		{"new catalog target", "vendor/new", "openai-responses", "openai", &fresh, len(facts)},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := ModelConfig{ModelID: test.modelID, Protocol: test.protocol, Dialect: test.dialect, Recommendations: facts, RecommendationTarget: test.target, Overrides: overrides}
			before, _ := json.Marshal(model)
			next, err := RebaseModelConfig(provider, model)
			if err != nil {
				t.Fatal(err)
			}
			if len(next.Recommendations) != test.wantFields || !reflect.DeepEqual(next.Overrides, overrides) {
				t.Fatalf("unexpected rebase: %+v", next)
			}
			if next.RecommendationTarget == nil || next.RecommendationTarget.ModelID != test.modelID || next.RecommendationTarget.Protocol != test.protocol {
				t.Fatalf("binding = %+v", next.RecommendationTarget)
			}
			if test.wantFields == 3 {
				for _, field := range []string{"context_window", "max_output_tokens", "input_price"} {
					if !reflect.DeepEqual(next.Recommendations[field], facts[field]) {
						t.Fatalf("model fact %s was lost", field)
					}
				}
			}
			after, _ := json.Marshal(model)
			if string(before) != string(after) {
				t.Fatal("rebase mutated the caller's configuration")
			}
		})
	}
}

func TestRecommendationContractUsesOnlyTheActiveAPIIdentity(t *testing.T) {
	provider := ProviderConfig{Protocol: "openai-completions", Dialect: "compatible", BaseURL: "https://fixture.example/v1", ConnectionScope: "connection-a"}
	model := ModelConfig{ModelID: "vendor/model"}
	original, err := RecommendationTargetFor(provider, model)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ProviderConfig){
		func(p *ProviderConfig) { p.BaseURL = "https://other.example/v1" },
		func(p *ProviderConfig) { p.PresetID = "commandcode" },
		func(p *ProviderConfig) { p.ConnectionScope = "connection-b" },
		func(p *ProviderConfig) { p.ReasoningFormat = models.ReasoningFormatThinkingBoolean },
	} {
		next := provider
		change(&next)
		target, err := RecommendationTargetFor(next, model)
		if err != nil || target.Contract == original.Contract {
			t.Fatalf("API identity change was not captured: %+v, %v", next, err)
		}
	}
	trailingSlash := provider
	trailingSlash.BaseURL += "/"
	if target, err := RecommendationTargetFor(trailingSlash, model); err != nil || target.Contract != original.Contract {
		t.Fatalf("trailing slash changed API identity: %+v, %v", target, err)
	}
	// A fixed Responses route does not inherit the provider's unrelated Chat
	// format or mapping, so changing those defaults must not clear its facts.
	model.Protocol = "openai-responses"
	fixed, err := RecommendationTargetFor(provider, model)
	if err != nil {
		t.Fatal(err)
	}
	provider.Protocol, provider.ReasoningFormat = "google-generative-ai", models.ReasoningFormatGemini
	if target, err := RecommendationTargetFor(provider, model); err != nil || target.Contract != fixed.Contract {
		t.Fatalf("an unrelated provider default changed a fixed API: %+v, %v", target, err)
	}
	provider.Protocol, provider.ReasoningFormat = "openai-completions", models.ReasoningFormatCustom
	provider.ReasoningMapping = &models.ReasoningMapping{On: raw(map[string]any{"thinking": map[string]any{"type": "enabled", "option": true}})}
	model.Protocol = "openai-completions"
	custom, err := RecommendationTargetFor(provider, model)
	if err != nil {
		t.Fatal(err)
	}
	provider.ReasoningMapping.On = json.RawMessage(`{"thinking":{"option":true,"type":"enabled"}}`)
	if target, err := RecommendationTargetFor(provider, model); err != nil || target.Contract != custom.Contract {
		t.Fatalf("mapping key order changed API identity: %+v, %v", target, err)
	}
	provider.ReasoningMapping.On = json.RawMessage(`{"thinking":{"type":"enabled"}}`)
	if target, err := RecommendationTargetFor(provider, model); err != nil || target.Contract == custom.Contract {
		t.Fatalf("mapping changes did not invalidate API identity: %+v, %v", target, err)
	}
}

func TestRecommendationBindingRejectsPartialIdentity(t *testing.T) {
	for _, target := range []*RecommendationTarget{{ModelID: "vendor/model"}, {Protocol: "openai-responses"}} {
		_, err := RebaseModelConfig(ProviderConfig{Protocol: "openai-responses"}, ModelConfig{ModelID: "vendor/model", RecommendationTarget: target})
		if err == nil {
			t.Fatalf("incomplete source identity accepted: %+v", target)
		}
	}
}

func TestRecommendationBindingDoesNotReinterpretAnUnknownSourceDialect(t *testing.T) {
	provider := ProviderConfig{Protocol: "openai-completions", BaseURL: "https://fixture.example/v1"}
	model := ModelConfig{ModelID: "vendor/model", Recommendations: map[string]json.RawMessage{
		"context_window": raw(8192), "max_output_tokens": raw(1024), "reasoning": raw(map[string]any{"efforts": []string{"high"}}),
	}}
	target, err := RecommendationTargetFor(provider, model)
	if err != nil || target.Dialect != "" {
		t.Fatalf("sparse source target = %+v, %v", target, err)
	}
	model.RecommendationTarget = &target
	provider.Dialect = "qwen"
	resolved, err := Resolve(provider, model, core.ModelSpec{ID: "selection"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Config.Dialect != models.Qwen || len(resolved.Values.Reasoning) != 0 || resolved.Values.ContextWindow != 8192 || resolved.Values.MaxOutputTokens != 1024 {
		t.Fatalf("new dialect reinterpreted old API facts: %+v", resolved)
	}
	if len(resolved.Controls.Options) != 1 || resolved.Controls.Options[0].ID != "interface-default" {
		t.Fatalf("old source controls survived: %+v", resolved.Controls)
	}
}
