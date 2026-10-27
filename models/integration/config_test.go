package integration

import (
	"encoding/json"
	"errors"
	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models"
	"reflect"
	"testing"
)

func TestResolveKeepsProviderWireContractIndependentOfModelID(t *testing.T) {
	for _, id := range []string{"arbitrary-new-model", "qwen-anything", "deepseek-anything", "vendor/kimi-anything"} {
		result, err := Resolve(ProviderConfig{PresetID: "together", Protocol: "openai-completions", Dialect: "compatible", BaseURL: "https://api.example/v1"}, ModelConfig{ModelID: id, Recommendations: map[string]json.RawMessage{"context_window": raw(8192), "max_output_tokens": raw(1024), "reasoning": raw(map[string]any{"efforts": []string{"vendor-level"}})}}, core.ModelSpec{ID: "local-ref", Revision: "1"})
		if err != nil {
			t.Fatal(err)
		}
		if result.Config.Model != id || result.Config.ReasoningFormat != models.ReasoningFormatTogether {
			t.Fatalf("model ID selected wire contract: %+v", result)
		}
		fields, err := models.EncodeReasoning(result.Protocol, result.Config, core.ReasoningOptions{Effort: "vendor-level"}, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if fields.Fields["reasoning_effort"] != "vendor-level" {
			t.Fatalf("native effort was translated: %+v", fields)
		}
	}
}
func TestResolveProtocolSwitchUsesCurrentProtocolDefaults(t *testing.T) {
	result, err := Resolve(ProviderConfig{PresetID: "qwen", Protocol: "openai-completions", Dialect: "qwen", BaseURL: "https://api.example/v1", ReasoningFormat: models.ReasoningFormatQwen}, ModelConfig{ModelID: "new-model", Protocol: "openai-responses", Recommendations: map[string]json.RawMessage{"context_window": raw(8192), "max_output_tokens": raw(1024)}}, core.ModelSpec{ID: "model", Revision: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Protocol != "openai-responses" || result.Config.ReasoningFormat != models.ReasoningFormatOpenAIResponses {
		t.Fatalf("old protocol format survived: %+v", result)
	}
}
func TestOptionsDistinguishesInheritedReasoningFromAPIDefault(t *testing.T) {
	temperature := 0.4
	defaults := core.GenerationOptions{Temperature: &temperature, Reasoning: core.ReasoningOptions{Effort: "native-high"}, ProviderOptions: map[string]json.RawMessage{"route": raw(map[string]any{"first": true})}}
	inherited, err := Options(defaults, core.GenerationOptions{}, nil)
	if err != nil || inherited.Reasoning.Effort != "native-high" {
		t.Fatalf("inherit: %+v %v", inherited, err)
	}
	apiDefault, err := Options(defaults, core.GenerationOptions{}, &core.ReasoningOptions{})
	if err != nil || apiDefault.Reasoning.Effort != "" || apiDefault.Temperature == nil {
		t.Fatalf("API default: %+v %v", apiDefault, err)
	}
	inherited.ProviderOptions["route"][0] = 'x'
	if defaults.ProviderOptions["route"][0] == 'x' {
		t.Fatal("resolved options alias defaults")
	}
}
func TestRebaseDropsOldRouteFactsAndKeepsGeneralModelData(t *testing.T) {
	original := map[string]json.RawMessage{"context_window": raw(8192), "max_output_tokens": raw(1024), "input_price": raw(2), "reasoning": raw(map[string]any{"toggle": true}), "streaming": raw(false)}
	next := RebaseRecommendations(original, "openai-completions", "openai-responses")
	if len(next["reasoning"]) != 0 || len(next["streaming"]) != 0 || string(next["context_window"]) != "8192" || string(next["input_price"]) != "2" {
		t.Fatalf("bad rebase: %+v", next)
	}
	if len(original["reasoning"]) == 0 {
		t.Fatal("rebase mutated source")
	}
	if len(RebaseRecommendations(original, "openai-responses", "openai-responses")["reasoning"]) == 0 {
		t.Fatal("unchanged route lost capability")
	}
}

func TestModelRoutesAndSessionHeaders(t *testing.T) {
	for _, test := range []struct{ protocol, suffix, dialect string }{{"anthropic-messages", "/anthropic/v1", "anthropic"}, {"google-generative-ai", "/google/v1beta", "google"}, {"openai-responses", "/openai/v1", "openai"}, {"openai-completions", "/openai/v1", "compatible"}} {
		preset := FindPreset("opencode-go")
		protocol, dialect, base, err := Route(ProviderConfig{PresetID: preset.ID, Protocol: preset.Protocol, Dialect: preset.Dialect, BaseURL: preset.BaseURL}, ModelConfig{Protocol: test.protocol})
		if err != nil || protocol != test.protocol || dialect != test.dialect || base != preset.BaseURL+test.suffix {
			t.Fatalf("route %s: %s %s %s %v", test.protocol, protocol, dialect, base, err)
		}
	}
	if SessionHeaders("opencode-go", "s1")["X-Opencode-Session"] != "s1" || SessionHeaders("github-copilot", "s2")["X-Interaction-Id"] != "s2" || SessionHeaders("openai", "s3") != nil {
		t.Fatal("session metadata escaped provider")
	}
}

func TestRebaseSourcesMatchesRemainingLeafFields(t *testing.T) {
	sources := map[string]RecommendationSource{"context_window": {Source: "general"}, "reasoning.efforts": {Source: "old-route"}, "reasoning.supported": {Source: "old-route"}}
	recommendations := RebaseRecommendations(map[string]json.RawMessage{"context_window": raw(8192), "reasoning": raw(map[string]any{"efforts": []string{"high"}, "supported": true})}, "openai-completions", "openai-responses")
	next := RebaseSources(sources, recommendations)
	if len(next) != 1 || next["context_window"].Source != "general" || len(sources) != 3 {
		t.Fatalf("bad source rebase: %+v", next)
	}
}

func TestEffectiveValuesPreservesUnknownTokenLimits(t *testing.T) {
	for _, test := range []struct {
		name                       string
		recommendations, overrides map[string]json.RawMessage
		context, output            int
	}{
		{name: "all omitted"},
		{name: "missing output", recommendations: map[string]json.RawMessage{"context_window": raw(1048576)}, context: 1048576},
		{name: "missing window", recommendations: map[string]json.RawMessage{"max_output_tokens": raw(8192)}, output: 8192},
		{name: "explicit zero override", recommendations: map[string]json.RawMessage{"context_window": raw(1048576), "max_output_tokens": raw(8192)}, overrides: map[string]json.RawMessage{"max_output_tokens": raw(0)}, context: 1048576},
		{name: "explicit null override", recommendations: map[string]json.RawMessage{"context_window": raw(1048576), "max_output_tokens": raw(8192)}, overrides: map[string]json.RawMessage{"max_output_tokens": raw(nil)}, context: 1048576},
	} {
		t.Run(test.name, func(t *testing.T) {
			values, sources, err := EffectiveValues(test.recommendations, test.overrides)
			if err != nil {
				t.Fatal(err)
			}
			if values.ContextWindow != test.context || values.MaxOutputTokens != test.output || values.MaxInputTokens != 0 {
				t.Fatalf("unknown metadata acquired a fabricated limit: %+v", values)
			}
			if len(test.overrides) > 0 && sources["max_output_tokens"] != "override" {
				t.Fatal("unknown override lost provenance")
			}
		})
	}
}

func TestResolveReasoningControlsWithUnknownBudgets(t *testing.T) {
	for _, test := range []struct {
		name            string
		limits          map[string]json.RawMessage
		context, output int
	}{
		{name: "unknown window and output", limits: map[string]json.RawMessage{}},
		{name: "known window unknown output", limits: map[string]json.RawMessage{"context_window": raw(1048576)}, context: 1048576},
		{name: "unknown window known output", limits: map[string]json.RawMessage{"max_output_tokens": raw(8192)}, output: 8192},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.limits["reasoning"] = raw(map[string]any{"supported": true, "efforts": []string{"vendor-native"}})
			result, err := Resolve(ProviderConfig{PresetID: "commandcode", Protocol: "openai-responses", Dialect: "openai"}, ModelConfig{ModelID: "vendor/new-runtime-model", Recommendations: test.limits}, core.ModelSpec{ID: "local-ref", Revision: "1"})
			if err != nil {
				t.Fatal(err)
			}
			if result.Config.Spec.ContextWindow != test.context || result.Config.Spec.MaxOutputTokens != test.output {
				t.Fatalf("resolve invented token limits: %+v", result.Config.Spec)
			}
			if result.Controls.Format != models.ReasoningFormatOpenAIResponses || len(result.Controls.Options) != 2 || result.Controls.Options[0].ID != "interface-default" || result.Controls.Options[1].ID != "effort:vendor-native" {
				t.Fatalf("unknown model budget suppressed native controls: %+v", result.Controls)
			}
			for _, option := range result.Controls.Options {
				encoded, err := models.EncodeReasoning(result.Protocol, result.Config, option.Value, test.output)
				if err != nil {
					t.Fatalf("advertised control cannot encode without Loop budget: %v", err)
				}
				if option.ID == "interface-default" && len(encoded.Fields) != 0 {
					t.Fatal("API default added reasoning parameters")
				}
				if option.ID == "effort:vendor-native" && !reflect.DeepEqual(encoded.Fields, map[string]any{"reasoning": map[string]any{"effort": "vendor-native"}}) {
					t.Fatalf("native effort changed: %+v", encoded.Fields)
				}
			}
		})
	}
	// No model capability facts still yields the universally valid API default.
	result, err := Resolve(ProviderConfig{Protocol: "openai-responses"}, ModelConfig{ModelID: "new-model"}, core.ModelSpec{ID: "local"})
	if err != nil || len(result.Controls.Options) != 1 || result.Controls.Options[0].ID != "interface-default" {
		t.Fatalf("sparse draft cannot describe API default: %+v (%v)", result.Controls, err)
	}
}

func TestEffectiveValuesAndResolveRejectNegativeTokenLimits(t *testing.T) {
	for _, field := range []string{"context_window", "max_input_tokens", "max_output_tokens"} {
		t.Run(field, func(t *testing.T) {
			recommendations := map[string]json.RawMessage{field: raw(-1)}
			if _, _, err := EffectiveValues(recommendations, nil); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("negative %s accepted: %v", field, err)
			}
			if _, err := Resolve(ProviderConfig{Protocol: "openai-responses"}, ModelConfig{ModelID: "new-model", Recommendations: recommendations}, core.ModelSpec{ID: "local"}); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("negative %s accepted by Resolve: %v", field, err)
			}
		})
	}
}
