package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models"
	modelprovider "github.com/oines/astral-core/models/provider"
	"github.com/oines/astral-core/store/memory"
)

// These tests start at the two raw HTTP documents, rather than constructing
// Discovery or ReasoningCapabilities. They must catch a lost catalog source or
// a missing API rule before controls and request encoding can hide that loss.
const resolutionCatalogFixture = `{
 "openai":{"models":{"gpt-fixture":{"name":"Maker fixture","limit":{"context":128000,"output":16000},"tool_call":true,"modalities":{"input":["text","image"]},"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","high"]}]}}},
 "openrouter":{"models":{"openai/gpt-fixture":{"limit":{"context":64000,"output":8000},"tool_call":false,"reasoning":true,"reasoning_options":[{"type":"effort","values":["medium"]}]}}},
 "xiaomi":{"models":{"mimo-v2.6-pro":{"limit":{"context":1048576,"output":131072},"reasoning":true,"reasoning_options":[{"type":"toggle"}]}}},
 "commandcode":{"models":{"xiaomi/mimo-v2.6-pro":{"limit":{"context":1000000}}}},
 "other":{"models":{"xiaomi/mimo-v2.6-pro":{"limit":{"context":64000,"output":8192},"reasoning":true,"reasoning_options":[{"type":"effort","values":["medium"]}]}}},
 "alibaba-cn":{"models":{
  "qwen-fixture-level":{"limit":{"context":128000,"output":8192},"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","high"]}]},
  "qwen-fixture-toggle":{"limit":{"context":128000,"output":8192},"reasoning":true,"reasoning_options":[{"type":"toggle"}]},
  "qwen-fixture-budget":{"limit":{"context":128000,"output":8192},"reasoning":true,"reasoning_options":[{"type":"budget_tokens","min":512,"max":8192}]}
 }},
 "fixture-lab":{"models":{"opaque-new-model":{"reasoning":true,"reasoning_options":[{"type":"effort","values":["vendor-native"]}]}}}
}`

type resolutionCatalogHarness struct {
	catalog        *Catalog
	baseURL        string
	liveRequests   atomic.Int32
	publicRequests atomic.Int32
}

func newResolutionCatalogHarness(t *testing.T, liveJSON string) *resolutionCatalogHarness {
	t.Helper()
	h := &resolutionCatalogHarness{}
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.liveRequests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer private-test-key" || r.Header.Get("X-Connection-Only") != "private-test-header" {
			t.Errorf("unexpected authenticated inventory request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveJSON))
	}))
	t.Cleanup(live.Close)
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.publicRequests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api.json" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Connection-Only") != "" {
			t.Errorf("public model metadata received private connection state: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resolutionCatalogFixture))
	}))
	t.Cleanup(public.Close)
	publicCatalog := NewModelCatalog("", public.Client())
	publicCatalog.url = public.URL + "/api.json"
	h.catalog, h.baseURL = &Catalog{HTTPClient: live.Client(), Public: publicCatalog}, live.URL+"/v1"
	return h
}

func (h *resolutionCatalogHarness) list(t *testing.T, config ProviderConfig) map[string]Discovery {
	t.Helper()
	items, err := h.catalog.List(context.Background(), config, "private-test-key", map[string]string{"X-Connection-Only": "private-test-header"})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Discovery{}
	for _, item := range items {
		byID[item.ID] = item
	}
	return byID
}

func resolveCatalogItem(t *testing.T, config ProviderConfig, item Discovery) Resolved {
	t.Helper()
	if item.ID == "" {
		t.Fatal("live inventory lost the expected remote identity")
	}
	resolved, err := Resolve(config, ModelConfig{ModelID: item.ID, Protocol: item.Protocol, Dialect: item.Dialect, Recommendations: item.Recommendations, RecommendationTarget: item.RecommendationTarget}, core.ModelSpec{ID: "local-reference", Revision: "fixture-revision"})
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func checkCatalogControls(t *testing.T, got models.ReasoningControls, format models.ReasoningFormat, options []models.ReasoningOption, diagnosticPaths ...string) {
	t.Helper()
	if got.Format != format || !reflect.DeepEqual(got.Options, options) || got.Budget != nil {
		t.Fatalf("incomplete or unexpected controls: %+v; want format=%s options=%+v and no budget", got, format, options)
	}
	paths := make([]string, 0, len(got.Diagnostics))
	for _, diagnostic := range got.Diagnostics {
		if diagnostic.Code != "unavailable-option" || diagnostic.Message == "" {
			t.Fatalf("unexpected or unexplained control diagnostic: %+v", diagnostic)
		}
		paths = append(paths, diagnostic.Path)
	}
	slices.Sort(paths)
	wantPaths := append([]string{}, diagnosticPaths...)
	slices.Sort(wantPaths)
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("diagnostic paths=%v, want %v", paths, wantPaths)
	}
}

func catalogDefaultOption() models.ReasoningOption {
	return models.ReasoningOption{ID: "interface-default", Value: core.ReasoningOptions{}}
}

func catalogEffortOption(effort string) models.ReasoningOption {
	return models.ReasoningOption{ID: "effort:" + effort, Value: core.ReasoningOptions{Effort: effort}}
}

func catalogSwitchOption(enabled bool) models.ReasoningOption {
	id := "off"
	if enabled {
		id = "on"
	}
	return models.ReasoningOption{ID: id, Value: core.ReasoningOptions{Enabled: &enabled}}
}

type rejectInferenceTransport struct{ calls *atomic.Int32 }

func (transport rejectInferenceTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	return nil, errors.New("this catalog acceptance test must never invoke a model")
}

func catalogAdapter(t *testing.T, resolved Resolved) modelprovider.Model {
	t.Helper()
	var calls atomic.Int32
	resolved.Config.HTTPClient = &http.Client{Transport: rejectInferenceTransport{calls: &calls}}
	t.Cleanup(func() {
		if calls.Load() != 0 {
			t.Errorf("catalog description or encoding invoked a model %d times", calls.Load())
		}
	})
	adapter, err := modelprovider.New(resolved.Protocol, resolved.Config)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func checkCatalogRequest(t *testing.T, adapter modelprovider.Model, remoteID string, selection core.ReasoningOptions, outputTokens int, expected map[string]any) map[string]any {
	t.Helper()
	// An explicit interface-default must also clear an independently configured
	// model default; passing the empty choice straight to the encoder is weaker.
	options, err := Options(core.GenerationOptions{Reasoning: core.ReasoningOptions{Effort: "default-must-not-leak"}}, core.GenerationOptions{}, &selection)
	if err != nil {
		t.Fatal(err)
	}
	body, err := adapter.RequestBody(core.Request{ModelID: adapter.Spec().ID, ModelRevision: adapter.Spec().Revision, MaxOutputTokens: outputTokens, Messages: []core.Message{core.User("hello")}, Options: options})
	if err != nil {
		t.Fatal(err)
	}
	if body["model"] != remoteID {
		t.Fatalf("catalog matching changed the wire identity: got %v, want %s", body["model"], remoteID)
	}
	fields := map[string]any{}
	for _, key := range []string{"reasoning", "reasoning_effort", "thinking", "enable_thinking", "thinking_budget", "chat_template_kwargs", "output_config"} {
		if value, present := body[key]; present {
			fields[key] = value
		}
	}
	if expected == nil {
		expected = map[string]any{}
	}
	if !reflect.DeepEqual(fields, expected) {
		t.Fatalf("reasoning wire fields=%v, want %v; body=%v", fields, expected, body)
	}
	return body
}

func TestCatalogResolutionE2ECustomMakerPriorityPreservesWireIdentity(t *testing.T) {
	h := newResolutionCatalogHarness(t, `{"data":[{"id":"gpt-fixture"},{"id":"vendor/gpt-fixture"},{"id":"gateway/vendor/gpt-fixture"}]}`)
	config := ProviderConfig{Protocol: "openai-completions", Dialect: "compatible", BaseURL: h.baseURL}
	if config.PresetID != "" {
		t.Fatal("this regression must exercise a custom connection without a preset")
	}
	items := h.list(t, config)
	if len(items) != 3 {
		t.Fatalf("directory identities=%v", items)
	}
	for _, id := range []string{"gpt-fixture", "vendor/gpt-fixture", "gateway/vendor/gpt-fixture"} {
		t.Run(id, func(t *testing.T) {
			manual, err := h.catalog.Recommend(context.Background(), config, id)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range []Discovery{items[id], manual} {
				for field, want := range map[string]string{"context_window": "128000", "max_output_tokens": "16000", "tool_calling": `"supported"`, "images": `"supported"`, "reasoning.efforts": `["low","high"]`} {
					checkRecommendation(t, item, field, want)
					if source := item.RecommendationSources[field].Source; source != modelsDevURL+"#openai/gpt-fixture" {
						t.Fatalf("%s lost maker provenance: %s", field, source)
					}
				}
				resolved := resolveCatalogItem(t, config, item)
				checkCatalogControls(t, resolved.Controls, models.ReasoningFormatOpenAIChat, []models.ReasoningOption{catalogDefaultOption(), catalogEffortOption("low"), catalogEffortOption("high")})
				adapter := catalogAdapter(t, resolved)
				for _, choice := range resolved.Controls.Options {
					var fields map[string]any
					if choice.Value.Effort != "" {
						fields = map[string]any{"reasoning_effort": choice.Value.Effort}
					}
					checkCatalogRequest(t, adapter, id, choice.Value, 1024, fields)
				}
			}
		})
	}
	if h.liveRequests.Load() != 1 || h.publicRequests.Load() != 1 {
		t.Fatalf("manual recommendation unexpectedly contacted the provider or bypassed the public cache: live=%d public=%d", h.liveRequests.Load(), h.publicRequests.Load())
	}
}

func TestCatalogResolutionE2EMiMoMetadataDoesNotChooseGatewayAPI(t *testing.T) {
	for _, preset := range []string{"commandcode", "mimo"} {
		t.Run(preset, func(t *testing.T) {
			h := newResolutionCatalogHarness(t, `{"data":[{"id":"mimo-v2.6-pro","context_length":1000000},{"id":"xiaomi/mimo-v2.6-pro","context_length":1000000}]}`)
			config := ProviderConfig{PresetID: preset, Protocol: "openai-responses", Dialect: "openai", BaseURL: h.baseURL}
			items := h.list(t, config)
			if len(items) != 2 {
				t.Fatalf("directory identities=%v", items)
			}
			for _, id := range []string{"mimo-v2.6-pro", "xiaomi/mimo-v2.6-pro"} {
				item := items[id]
				checkRecommendation(t, item, "context_window", "1000000")
				checkRecommendation(t, item, "max_output_tokens", "131072")
				if item.RecommendationSources["max_output_tokens"].Source != modelsDevURL+"#xiaomi/mimo-v2.6-pro" || item.RecommendationSources["context_window"].Source != h.baseURL+"/models" {
					t.Fatalf("live and maker field priorities lost: %+v", item.RecommendationSources)
				}
				resolved := resolveCatalogItem(t, config, item)
				if resolved.Config.Reasoning == nil {
					t.Fatal("raw public toggle capability disappeared before API resolution")
				}
				caps := resolved.Config.Reasoning
				if preset == "commandcode" {
					if caps.OnEffort != "" || caps.OffEffort != "" || caps.DefaultOn != nil {
						t.Fatalf("gateway borrowed MiMo's API bindings: %+v", caps)
					}
					checkCatalogControls(t, resolved.Controls, models.ReasoningFormatOpenAIResponses, []models.ReasoningOption{catalogDefaultOption()}, "reasoning.options.off", "reasoning.options.on")
				} else {
					if caps.OnEffort != "high" || caps.OffEffort != "none" || caps.DefaultOn == nil || !*caps.DefaultOn {
						t.Fatalf("MiMo's own API rule did not bind the discovered toggle: %+v", caps)
					}
					checkCatalogControls(t, resolved.Controls, models.ReasoningFormatOpenAIResponses, []models.ReasoningOption{catalogDefaultOption(), catalogSwitchOption(false), catalogSwitchOption(true)})
				}
				adapter := catalogAdapter(t, resolved)
				for _, choice := range resolved.Controls.Options {
					var fields map[string]any
					if choice.ID == "off" {
						fields = map[string]any{"reasoning": map[string]any{"effort": "none"}}
					} else if choice.ID == "on" {
						fields = map[string]any{"reasoning": map[string]any{"effort": "high"}}
					}
					checkCatalogRequest(t, adapter, id, choice.Value, 1024, fields)
				}
			}
		})
	}
}

func TestCatalogResolutionE2ELiveFalseNullAndEmptyBlockPublicFallback(t *testing.T) {
	for _, scenario := range []struct {
		name, rawModel, preset string
		fields                 map[string]string
	}{
		{"false", `{"id":"gpt-fixture","reasoning":false,"capabilities":{"supports":{"tool_calls":false}}}`, "magpie", map[string]string{"reasoning.supported": "false", "tool_calling": `"unsupported"`}},
		{"null parent", `{"id":"gpt-fixture","reasoning":null,"supported_reasoning_levels":null,"context_window":null,"max_output_tokens":null,"modalities":{"input":null}}`, "magpie", map[string]string{"reasoning": "null", "context_window": "null", "max_output_tokens": "null", "images": "null"}},
		{"null capabilities container", `{"id":"gpt-fixture","capabilities":{"reasoning":null}}`, "magpie", map[string]string{"reasoning": "null"}},
		{"null canonical container", `{"id":"gpt-fixture","reasoning_capabilities":null}`, "magpie", map[string]string{"reasoning": "null"}},
		{"null Floway container", `{"id":"gpt-fixture","chat":{"reasoning":null}}`, "floway", map[string]string{"reasoning": "null"}},
		{"null effort leaf", `{"id":"gpt-fixture","supported_reasoning_levels":null}`, "magpie", map[string]string{"reasoning.efforts": "null"}},
		{"empty", `{"id":"gpt-fixture","reasoning":true,"supported_reasoning_levels":[],"modalities":{"input":[]}}`, "magpie", map[string]string{"reasoning.efforts": `[]`, "images": `"unsupported"`}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h := newResolutionCatalogHarness(t, `{"data":[`+scenario.rawModel+`]}`)
			config := ProviderConfig{PresetID: scenario.preset, Protocol: "openai-responses", Dialect: "openai", BaseURL: h.baseURL}
			item := h.list(t, config)["gpt-fixture"]
			for field, want := range scenario.fields {
				checkRecommendation(t, item, field, want)
				if item.RecommendationSources[field].Source != h.baseURL+"/models" {
					t.Fatalf("explicit %s value lost live provenance: %+v", field, item.RecommendationSources[field])
				}
			}
			resolved := resolveCatalogItem(t, config, item)
			checkCatalogControls(t, resolved.Controls, models.ReasoningFormatOpenAIResponses, []models.ReasoningOption{catalogDefaultOption()})
			adapter := catalogAdapter(t, resolved)
			checkCatalogRequest(t, adapter, item.ID, core.ReasoningOptions{}, 1024, nil)
			_, err := adapter.RequestBody(core.Request{ModelID: adapter.Spec().ID, ModelRevision: adapter.Spec().Revision, MaxOutputTokens: 1024, Messages: []core.Message{core.User("hello")}, Options: core.GenerationOptions{Reasoning: core.ReasoningOptions{Effort: "high"}}})
			if !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("public fallback resurrected an unavailable live effort: %v", err)
			}
		})
	}
}

func TestCatalogResolutionE2EUnknownBudgetsDescribeAndEncodeButLoopRejects(t *testing.T) {
	h := newResolutionCatalogHarness(t, `{"data":[{"id":"fixture-lab/opaque-new-model"}]}`)
	config := ProviderConfig{Protocol: "openai-completions", Dialect: "compatible", BaseURL: h.baseURL}
	item := h.list(t, config)["fixture-lab/opaque-new-model"]
	resolved := resolveCatalogItem(t, config, item)
	if resolved.Values.ContextWindow != 0 || resolved.Values.MaxOutputTokens != 0 {
		t.Fatalf("metadata invented budgets for an unlisted model: %+v", resolved.Values)
	}
	checkCatalogControls(t, resolved.Controls, models.ReasoningFormatOpenAIChat, []models.ReasoningOption{catalogDefaultOption(), catalogEffortOption("vendor-native")})
	adapter := catalogAdapter(t, resolved)
	body := checkCatalogRequest(t, adapter, item.ID, core.ReasoningOptions{}, 0, nil)
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if _, present := body[key]; present {
			t.Fatalf("direct encoding invented an output limit: %v", body)
		}
	}
	checkCatalogRequest(t, adapter, item.ID, core.ReasoningOptions{Effort: "vendor-native"}, 0, map[string]any{"reasoning_effort": "vendor-native"})
	ctx := context.Background()
	loopConfig := core.Config{Model: adapter, Store: memory.New(), EstimateTokens: func(context.Context, core.Model, core.Request) (int, error) { return 1, nil }}
	if session, err := core.NewSession(ctx, loopConfig, core.SessionOptions{}); !errors.Is(err, core.ErrInvalid) {
		if session != nil {
			_ = session.Close(ctx)
		}
		t.Fatalf("bounded Loop accepted the same catalog adapter without budgets: %v", err)
	}

	loopConfig.Model = nil
	loopConfig.Selection = core.ModelSelection{ID: "selected-unknown-budget-model"}
	loopConfig.ResolveModel = func(context.Context, core.ModelSelection) (core.ResolvedModel, error) {
		return core.ResolvedModel{Model: adapter}, nil
	}
	session, err := core.NewSession(ctx, loopConfig, core.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(ctx)
	prompt, err := session.Prompt(ctx, core.Input{ID: "unknown-budget-input", Message: core.User("execute")})
	if err == nil {
		_, err = prompt.Run.Wait(ctx)
	}
	if !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("fresh resolution accepted the same unknown budgets at the Loop boundary: %v", err)
	}
}

func TestCatalogResolutionE2ESameProviderKeepsModelSpecificControls(t *testing.T) {
	h := newResolutionCatalogHarness(t, `{"data":[{"id":"qwen-fixture-level"},{"id":"qwen-fixture-toggle"},{"id":"qwen-fixture-budget"}]}`)
	config := ProviderConfig{PresetID: "qwen", Protocol: "openai-completions", Dialect: "qwen", BaseURL: h.baseURL}
	items := h.list(t, config)
	if len(items) != 3 {
		t.Fatalf("directory identities=%v", items)
	}
	for _, scenario := range []struct {
		id      string
		options []models.ReasoningOption
	}{
		{"qwen-fixture-level", []models.ReasoningOption{catalogDefaultOption(), catalogEffortOption("low"), catalogEffortOption("high")}},
		{"qwen-fixture-toggle", []models.ReasoningOption{catalogDefaultOption(), catalogSwitchOption(false), catalogSwitchOption(true)}},
		{"qwen-fixture-budget", []models.ReasoningOption{catalogDefaultOption()}},
	} {
		t.Run(scenario.id, func(t *testing.T) {
			item := items[scenario.id]
			resolved := resolveCatalogItem(t, config, item)
			controls := resolved.Controls
			if scenario.id == "qwen-fixture-budget" {
				if controls.Budget == nil || controls.Budget.Min == nil || *controls.Budget.Min != 512 || controls.Budget.Max == nil || *controls.Budget.Max != 8192 {
					t.Fatalf("raw budget bounds did not reach the effective control: %+v", controls)
				}
				// The budget has been checked above; the remaining full control
				// shape must still contain no invented switch or effort choices.
				controls.Budget = nil
			}
			checkCatalogControls(t, controls, models.ReasoningFormatQwen, scenario.options)
			adapter := catalogAdapter(t, resolved)
			for _, choice := range resolved.Controls.Options {
				var fields map[string]any
				switch {
				case choice.Value.Effort != "":
					fields = map[string]any{"reasoning_effort": choice.Value.Effort}
				case choice.Value.Enabled != nil:
					fields = map[string]any{"enable_thinking": *choice.Value.Enabled}
				}
				checkCatalogRequest(t, adapter, item.ID, choice.Value, 1024, fields)
			}
			if resolved.Controls.Budget != nil {
				budget := 2048
				checkCatalogRequest(t, adapter, item.ID, core.ReasoningOptions{BudgetTokens: &budget}, 4096, map[string]any{"thinking_budget": 2048})
				tooSmall := 511
				if _, err := adapter.RequestBody(core.Request{ModelID: adapter.Spec().ID, ModelRevision: adapter.Spec().Revision, MaxOutputTokens: 4096, Messages: []core.Message{core.User("hello")}, Options: core.GenerationOptions{Reasoning: core.ReasoningOptions{BudgetTokens: &tooSmall}}}); !errors.Is(err, core.ErrInvalid) {
					t.Fatalf("raw catalog minimum was ignored during encoding: %v", err)
				}
			}
		})
	}
}

func TestCatalogResolutionE2ERebindingInvalidatesOnlyAPIFacts(t *testing.T) {
	h := newResolutionCatalogHarness(t, `{"data":[{"id":"xiaomi/mimo-v2.6-pro","context_length":1000000}]}`)
	source := ProviderConfig{PresetID: "mimo", Protocol: "openai-responses", Dialect: "openai", BaseURL: h.baseURL, ConnectionScope: "source-connection"}
	item := h.list(t, source)["xiaomi/mimo-v2.6-pro"]
	// Prove that this is an actual normalized MiMo catalog record before
	// checking whether the same data is safe on another API contract.
	checkRecommendation(t, item, "reasoning.on_effort", `"high"`)
	checkRecommendation(t, item, "reasoning.off_effort", `"none"`)
	checkRecommendation(t, item, "reasoning.default_on", "true")
	model := ModelConfig{ModelID: item.ID, Protocol: item.Protocol, Dialect: item.Dialect, Recommendations: item.Recommendations, RecommendationTarget: item.RecommendationTarget, Overrides: map[string]json.RawMessage{"defaults": raw(map[string]any{"temperature": .25})}}
	before, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct {
		name, preset, baseURL, scope string
	}{
		{"custom connection", "", h.baseURL, source.ConnectionScope},
		{"CommandCode connection", "commandcode", h.baseURL, source.ConnectionScope},
		{"same preset at a different endpoint", "mimo", h.baseURL + "/another-endpoint", source.ConnectionScope},
		{"same API at a different connection", "mimo", h.baseURL, "another-connection"},
	} {
		t.Run(target.name, func(t *testing.T) {
			config := ProviderConfig{PresetID: target.preset, Protocol: source.Protocol, Dialect: source.Dialect, BaseURL: target.baseURL, ConnectionScope: target.scope}
			rebased, err := RebaseModelConfig(config, model)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(rebased.Overrides, model.Overrides) {
				t.Fatalf("changing a recommendation target rewrote explicit user overrides: %v", rebased.Overrides)
			}
			for _, field := range []string{"context_window", "max_output_tokens"} {
				if !sameJSON(rebased.Recommendations[field], item.Recommendations[field]) {
					t.Fatalf("changing API target discarded general model metadata %s: %v", field, rebased.Recommendations)
				}
			}
			for _, field := range []string{"reasoning", "reasoning_format", "reasoning_mapping", "reasoning_replay", "require_reasoning_replay", "defaults"} {
				if _, present := rebased.Recommendations[field]; present {
					t.Fatalf("another endpoint retained the source API recommendation %s: %s", field, rebased.Recommendations[field])
				}
			}
			resolved, err := Resolve(config, rebased, core.ModelSpec{ID: "rebound-model", Revision: "target-revision"})
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Values.ContextWindow != 1000000 || resolved.Values.MaxOutputTokens != 131072 || resolved.Values.Defaults.Temperature == nil || *resolved.Values.Defaults.Temperature != .25 {
				t.Fatalf("rebinding changed general model limits or explicit defaults: %+v", resolved.Values)
			}
			checkCatalogControls(t, resolved.Controls, models.ReasoningFormatOpenAIResponses, []models.ReasoningOption{catalogDefaultOption()})
			checkCatalogRequest(t, catalogAdapter(t, resolved), item.ID, core.ReasoningOptions{}, 1024, nil)
		})
	}
	after, err := json.Marshal(model)
	if err != nil || string(before) != string(after) {
		t.Fatal("rebasing a target mutated the source catalog configuration")
	}
}
