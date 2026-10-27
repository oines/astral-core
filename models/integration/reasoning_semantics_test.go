package integration

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/provider"
)

type semanticsBodyModel interface {
	RequestBody(core.Request) (map[string]any, error)
}

func semanticsResolveRaw(t *testing.T, providerConfig ProviderConfig, modelData string) Resolved {
	t.Helper()
	recommendations, err := liveModelRecommendations(providerConfig, json.RawMessage(modelData))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(providerConfig, ModelConfig{ModelID: "opaque-model", Recommendations: recommendations}, core.ModelSpec{ID: "local", Revision: "r", MaxOutputTokens: 8192})
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func semanticsWire(t *testing.T, resolved Resolved, option core.ReasoningOptions) map[string]any {
	t.Helper()
	model, err := provider.New(resolved.Protocol, resolved.Config)
	if err != nil {
		t.Fatal(err)
	}
	body, err := model.(semanticsBodyModel).RequestBody(core.Request{ModelID: "local", ModelRevision: "r", MaxOutputTokens: 8192, Messages: []core.Message{core.User("test")}, Options: core.GenerationOptions{Reasoning: option}})
	if err != nil {
		t.Fatal(err)
	}
	selected := map[string]any{}
	for _, name := range []string{"reasoning", "reasoning_effort", "thinking", "enable_thinking", "thinking_budget", "output_config", "generationConfig", "chat_template_kwargs"} {
		if value, present := body[name]; present {
			if name == "generationConfig" {
				if thinking, present := value.(map[string]any)["thinkingConfig"]; present {
					selected[name] = map[string]any{"thinkingConfig": thinking}
				}
				continue
			}
			selected[name] = value
		}
	}
	return selected
}

func semanticsIDs(controls models.ReasoningControls) []string {
	ids := make([]string, len(controls.Options))
	for i, option := range controls.Options {
		ids[i] = option.ID
	}
	return ids
}

func TestSameProviderUsesEachModelsDeclaredControlModes(t *testing.T) {
	config := ProviderConfig{PresetID: "qwen", Protocol: "openai-completions", Dialect: "qwen"}
	for _, tc := range []struct {
		name, capabilities string
		ids                []string
		selection          core.ReasoningOptions
		wire               map[string]any
		budget             bool
	}{
		{"levels", `{"supported":true,"toggle":false,"disable":false,"efforts":["low","high"],"budget":false}`, []string{"interface-default", "effort:low", "effort:high"}, core.ReasoningOptions{Effort: "high"}, map[string]any{"reasoning_effort": "high"}, false},
		{"toggle", `{"supported":true,"toggle":true,"disable":true,"efforts":[],"budget":false}`, []string{"interface-default", "off", "on"}, core.ReasoningOptions{Enabled: boolPointer(true)}, map[string]any{"enable_thinking": true}, false},
		{"budget", `{"supported":true,"toggle":false,"disable":false,"efforts":[],"budget":true,"min_budget":512,"max_budget":4096}`, []string{"interface-default"}, core.ReasoningOptions{BudgetTokens: intPointer(1024)}, map[string]any{"thinking_budget": 1024}, true},
		{"mandatory", `{"supported":true,"mandatory":true,"efforts":["minimal","high"],"budget":false}`, []string{"interface-default", "effort:minimal", "effort:high"}, core.ReasoningOptions{Effort: "minimal"}, map[string]any{"reasoning_effort": "minimal"}, false},
		{"both", `{"supported":true,"toggle":true,"disable":true,"efforts":["low","high"],"budget":false}`, []string{"interface-default", "off", "on", "effort:low", "effort:high"}, core.ReasoningOptions{Enabled: boolPointer(true), Effort: "high"}, map[string]any{"enable_thinking": true, "reasoning_effort": "high"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved := semanticsResolveRaw(t, config, `{"reasoning":`+tc.capabilities+`}`)
			if !reflect.DeepEqual(semanticsIDs(resolved.Controls), tc.ids) || (resolved.Controls.Budget != nil) != tc.budget || len(resolved.Controls.Diagnostics) != 0 {
				t.Fatalf("incorrect complete controls: %+v", resolved.Controls)
			}
			if got := semanticsWire(t, resolved, tc.selection); !reflect.DeepEqual(got, tc.wire) {
				t.Fatalf("wire=%v want %v", got, tc.wire)
			}
			if got := semanticsWire(t, resolved, core.ReasoningOptions{}); len(got) != 0 {
				t.Fatalf("capabilities leaked into API default: %v", got)
			}
		})
	}
}

func TestDocumentedProviderBindingsDoNotCrossConnectionsOrOverrideUnknowns(t *testing.T) {
	input := json.RawMessage(`{"supported":true,"toggle":true,"disable":true,"on_effort":null,"off_effort":null,"default_on":null,"efforts":[],"budget":false,"dynamic_budget":false}`)
	for _, preset := range []string{"mimo", "deepseek", "google", "fireworks", "commandcode", ""} {
		got, _, err := ResolveReasoningSemantics(ProviderConfig{PresetID: preset}, "openai-responses", input)
		if err != nil {
			t.Fatal(err)
		}
		var before, after map[string]json.RawMessage
		_ = json.Unmarshal(input, &before)
		_ = json.Unmarshal(got, &after)
		for key, value := range before {
			if !reflect.DeepEqual(value, after[key]) {
				t.Fatalf("%s overwrote %s: %s", preset, key, got)
			}
		}
	}
	for _, preset := range []string{"mimo", "deepseek", "google", "fireworks"} {
		for _, input := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`{"supported":false}`)} {
			got, _, err := ResolveReasoningSemantics(ProviderConfig{PresetID: preset}, "openai-responses", input)
			if err != nil || !reflect.DeepEqual(input, got) {
				t.Fatalf("%s invented support: %s %v", preset, got, err)
			}
		}
	}
}

func boolPointer(value bool) *bool { return &value }
func intPointer(value int) *int    { return &value }

func TestDocumentedReasoningAPIModesUseActualRequestFields(t *testing.T) {
	for _, tc := range []struct {
		name      string
		provider  ProviderConfig
		caps      string
		ids       []string
		selection core.ReasoningOptions
		wire      map[string]any
		budget    bool
	}{
		{"deepseek-levels-and-toggle", ProviderConfig{PresetID: "deepseek", Protocol: "openai-responses", Dialect: "deepseek"}, `{"supported":true,"toggle":true,"disable":true}`, []string{"interface-default", "off", "on", "effort:low", "effort:high", "effort:max"}, core.ReasoningOptions{Enabled: boolPointer(false)}, map[string]any{"reasoning": map[string]any{"effort": "none"}}, false},
		{"google-level-only", ProviderConfig{PresetID: "google", Protocol: "google-generative-ai"}, `{"supported":true,"toggle":false,"disable":false,"efforts":["minimal","low","high"],"budget":false}`, []string{"interface-default", "effort:minimal", "effort:low", "effort:high"}, core.ReasoningOptions{Effort: "minimal"}, map[string]any{"generationConfig": map[string]any{"thinkingConfig": map[string]any{"thinkingLevel": "minimal"}}}, false},
		{"google-budget-auto", ProviderConfig{PresetID: "google", Protocol: "google-generative-ai"}, `{"supported":true,"toggle":true,"disable":true,"efforts":[],"budget":true,"min_budget":0,"max_budget":4096}`, []string{"interface-default", "off", "on"}, core.ReasoningOptions{Enabled: boolPointer(true)}, map[string]any{"generationConfig": map[string]any{"thinkingConfig": map[string]any{"thinkingBudget": -1}}}, true},
		{"anthropic-budget", ProviderConfig{PresetID: "anthropic", Protocol: "anthropic-messages"}, `{"supported":true,"toggle":false,"disable":false,"efforts":[],"budget":true,"min_budget":1024,"max_budget":4096}`, []string{"interface-default"}, core.ReasoningOptions{BudgetTokens: intPointer(2048)}, map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 2048}}, true},
		{"anthropic-adaptive", ProviderConfig{PresetID: "anthropic", Protocol: "anthropic-messages"}, `{"supported":true,"toggle":false,"disable":false,"efforts":["low","high"],"budget":false}`, []string{"interface-default", "effort:low", "effort:high"}, core.ReasoningOptions{Effort: "high"}, map[string]any{"thinking": map[string]any{"type": "adaptive"}, "output_config": map[string]any{"effort": "high"}}, false},
		{"moonshot-effort-only", ProviderConfig{PresetID: "moonshot", Protocol: "openai-completions"}, `{"supported":true,"toggle":false,"disable":false,"efforts":["low","high"],"budget":false}`, []string{"interface-default", "effort:low", "effort:high"}, core.ReasoningOptions{Effort: "high"}, map[string]any{"reasoning_effort": "high"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved := semanticsResolveRaw(t, tc.provider, `{"reasoning":`+tc.caps+`}`)
			if !reflect.DeepEqual(semanticsIDs(resolved.Controls), tc.ids) || (resolved.Controls.Budget != nil) != tc.budget || len(resolved.Controls.Diagnostics) != 0 {
				t.Fatalf("complete controls: %+v", resolved.Controls)
			}
			if got := semanticsWire(t, resolved, tc.selection); !reflect.DeepEqual(got, tc.wire) {
				t.Fatalf("wire=%#v want %#v", got, tc.wire)
			}
			if resolved.Values.Defaults.Reasoning.Enabled != nil || resolved.Values.Defaults.Reasoning.Effort != "" || resolved.Values.Defaults.Reasoning.BudgetTokens != nil {
				t.Fatal("API capability default changed request defaults")
			}
		})
	}
}

func TestCustomProviderReadsEachLiveModelsExplicitReasoningFormat(t *testing.T) {
	h := newResolutionCatalogHarness(t, `{"data":[
	 {"id":"format-a","reasoning_format":"qwen","reasoning":{"supported":true,"toggle":true,"disable":true,"efforts":[]}},
	 {"id":"format-b","reasoning_format":"thinking-boolean","reasoning":{"supported":true,"toggle":true,"disable":true,"efforts":[]}},
	 {"id":"format-default","reasoning":{"supported":true,"toggle":false,"disable":false,"efforts":["native-low"]}}
	]}`)
	config := ProviderConfig{Protocol: "openai-completions", Dialect: "compatible", BaseURL: h.baseURL}
	items := h.list(t, config)
	for _, tc := range []struct {
		id     string
		format models.ReasoningFormat
		wire   map[string]any
	}{
		{"format-a", models.ReasoningFormatQwen, map[string]any{"enable_thinking": true}},
		{"format-b", models.ReasoningFormatThinkingBoolean, map[string]any{"thinking": true}},
		{"format-default", models.ReasoningFormatOpenAIChat, map[string]any{"reasoning_effort": "native-low"}},
	} {
		resolved := resolveCatalogItem(t, config, items[tc.id])
		if resolved.Controls.Format != tc.format || len(resolved.Controls.Diagnostics) != 0 {
			t.Fatalf("%s wrong format: %+v", tc.id, resolved.Controls)
		}
		selection := core.ReasoningOptions{Enabled: boolPointer(true)}
		if tc.id == "format-default" {
			selection = core.ReasoningOptions{Effort: "native-low"}
		}
		// This helper's fixed identity is replaced with the catalog fixture's
		// reference so the public adapter validates the complete request.
		resolved.Config.Spec.ID = "local"
		resolved.Config.Spec.Revision = "r"
		if got := semanticsWire(t, resolved, selection); !reflect.DeepEqual(got, tc.wire) {
			t.Fatalf("%s fields=%v", tc.id, got)
		}
	}
	for _, format := range []string{"not-a-format", "gemini", "openai-responses"} {
		if _, err := liveModelRecommendations(config, json.RawMessage(`{"reasoning_format":"`+format+`"}`)); err == nil {
			t.Fatalf("incompatible/unknown live format accepted: %s", format)
		}
	}
	// The actual per-model endpoint wins over the provider's initial surface.
	h = newResolutionCatalogHarness(t, `{"data":[{"id":"route-model","supported_endpoints":["/responses"],"reasoning_format":"openai-responses"}]}`)
	config.BaseURL = h.baseURL
	item := h.list(t, config)["route-model"]
	if item.Protocol != "openai-responses" || string(item.Recommendations["reasoning_format"]) != `"openai-responses"` {
		t.Fatalf("format validated against wrong endpoint: %+v", item)
	}
}

func TestFireworksAdaptiveEffortConstraintSurvivesFreezeAndRestore(t *testing.T) {
	config := ProviderConfig{PresetID: "fireworks", Protocol: "anthropic-messages"}
	resolved := semanticsResolveRaw(t, config, `{"reasoning":{"supported":true,"toggle":true,"disable":true,"efforts":["low","high"],"budget":true}}`)
	if !resolved.Config.RequireAdaptiveEffort || resolved.Controls.Budget != nil || !reflect.DeepEqual(semanticsIDs(resolved.Controls), []string{"interface-default", "off", "effort:low", "effort:high"}) {
		t.Fatalf("Fireworks advertised invalid controls: %+v", resolved.Controls)
	}
	paths := map[string]bool{}
	for _, diagnostic := range resolved.Controls.Diagnostics {
		paths[diagnostic.Path] = true
	}
	if len(paths) != 2 || !paths["reasoning.options.on"] || !paths["reasoning.budget"] {
		t.Fatalf("missing precise diagnostics: %+v", resolved.Controls.Diagnostics)
	}
	if got := semanticsWire(t, resolved, core.ReasoningOptions{}); len(got) != 0 {
		t.Fatalf("interface default emitted fields: %v", got)
	}
	if got := semanticsWire(t, resolved, core.ReasoningOptions{Effort: "high"}); !reflect.DeepEqual(got, map[string]any{"thinking": map[string]any{"type": "adaptive"}, "output_config": map[string]any{"effort": "high"}}) {
		t.Fatalf("effort encoding: %v", got)
	}
	snapshot, err := provider.Freeze(resolved.Protocol, resolved.Config)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := provider.Restore(snapshot, provider.Credentials{})
	if err != nil || !restored.SnapshotConfig().RequireAdaptiveEffort {
		t.Fatalf("constraint lost on restore: %v", err)
	}
	request := core.Request{ModelID: "local", ModelRevision: "r", MaxOutputTokens: 8192, Messages: []core.Message{core.User("test")}, Options: core.GenerationOptions{Reasoning: core.ReasoningOptions{Enabled: boolPointer(true)}}}
	if err := core.ValidateRequest(context.Background(), restored, request); err == nil {
		t.Fatal("unbound on accepted after restore")
	}
	request.Options.Reasoning = core.ReasoningOptions{BudgetTokens: intPointer(2048)}
	if err := core.ValidateRequest(context.Background(), restored, request); err == nil {
		t.Fatal("exact budget accepted on Fireworks adaptive API")
	}
	for _, tc := range []struct {
		binding string
		want    bool
	}{
		{`"default_effort":"high"`, true},
		{`"default_effort":"high","on_effort":null`, false},
		{`"default_effort":"none"`, false},
	} {
		bound := semanticsResolveRaw(t, config, `{"reasoning":{"supported":true,"toggle":true,"disable":true,"efforts":["low","high"],`+tc.binding+`}}`)
		if (bound.Config.Reasoning.OnEffort == "high") != tc.want {
			t.Fatalf("incorrect active binding: %+v", bound.Config.Reasoning)
		}
		if tc.want {
			got := semanticsWire(t, bound, core.ReasoningOptions{Enabled: boolPointer(true)})
			want := map[string]any{"thinking": map[string]any{"type": "adaptive"}, "output_config": map[string]any{"effort": "high"}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("source default was not used: %v", got)
			}
		}
	}
}
