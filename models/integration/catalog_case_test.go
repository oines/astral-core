package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models"
)

const caseCatalogFixture = `{
 "moonshotai":{"models":{"kimi-k3.1-pro":{"name":"Kimi fixture","limit":{"context":1048576,"input":1000000,"output":131072},"tool_call":true,"modalities":{"input":["text","image"]},"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","high","max"]}],"cost":{"input":0.5,"output":2}}}},
 "other":{"models":{"other/kimi-k3.1-pro":{"limit":{"context":64000,"output":8192},"tool_call":false,"modalities":{"input":["text"]},"reasoning":true,"reasoning_options":[{"type":"effort","values":["medium"]}],"cost":{"input":1,"output":4}}}}
}`

func TestCatalogCaseFallbackRestoresKimiCapabilitiesWithoutChangingWireID(t *testing.T) {
	snapshot := fixtureSnapshot(t, caseCatalogFixture)
	config := ProviderConfig{PresetID: "commandcode", Protocol: "openai-responses", Dialect: "openai"}
	for _, id := range []string{"moonshotai/Kimi-K3.1-Pro", "Kimi-K3.1-Pro", "gateway/moonshotai/Kimi-K3.1-Pro"} {
		t.Run(id, func(t *testing.T) {
			item := enrichDiscovery(config, snapshot, Discovery{ID: id, Name: id})
			if item.ID != id || item.Protocol != config.Protocol || item.Dialect != config.Dialect {
				t.Fatalf("case-insensitive metadata lookup changed the configured target: %+v", item)
			}
			for field, want := range map[string]string{
				"context_window": "1048576", "max_input_tokens": "1000000", "max_output_tokens": "131072",
				"tool_calling": `"supported"`, "images": `"supported"`, "reasoning.supported": "true",
				"reasoning.efforts": `["low","high","max"]`, "input_price": "0.5", "output_price": "2",
			} {
				checkRecommendation(t, item, field, want)
				if source := item.RecommendationSources[field].Source; source != modelsDevURL+"#moonshotai/kimi-k3.1-pro" {
					t.Fatalf("%s lost the canonical public metadata source: %s", field, source)
				}
			}
			resolved := resolveCatalogItem(t, config, item)
			checkCatalogControls(t, resolved.Controls, models.ReasoningFormatOpenAIResponses, []models.ReasoningOption{catalogDefaultOption(), catalogEffortOption("low"), catalogEffortOption("high"), catalogEffortOption("max")})
			adapter := catalogAdapter(t, resolved)
			for _, choice := range resolved.Controls.Options {
				var fields map[string]any
				if choice.Value.Effort != "" {
					fields = map[string]any{"reasoning": map[string]any{"effort": choice.Value.Effort}}
				}
				checkCatalogRequest(t, adapter, id, choice.Value, 1024, fields)
			}
		})
	}
}

func TestCatalogCaseFallbackKeepsSourcePriority(t *testing.T) {
	snapshot := fixtureSnapshot(t, `{
 "moonshotai":{"models":{"kimi-priority":{"limit":{"context":128000,"output":16000},"tool_call":true}}},
 "moonshotai-cn":{"models":{"kimi-priority":{"limit":{"context":64000,"output":8000}}}},
 "commandcode":{"models":{"moonshotai/kimi-priority":{"limit":{"output":4096},"tool_call":false}}},
 "other":{"models":{"other/kimi-priority":{"limit":{"context":32000,"output":2000}}}}
 }`)
	for _, test := range []struct {
		name, preset, id, context, output, contextSource, outputSource string
	}{
		{"maker", "", "Kimi-priority", "128000", "16000", "moonshotai/kimi-priority", "moonshotai/kimi-priority"},
		{"explicit namespace", "", "moonshotai-cn/Kimi-priority", "64000", "8000", "moonshotai-cn/kimi-priority", "moonshotai-cn/kimi-priority"},
		{"current provider", "commandcode", "moonshotai/Kimi-priority", "128000", "4096", "moonshotai/kimi-priority", "commandcode/moonshotai/kimi-priority"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := ProviderConfig{PresetID: test.preset, Protocol: "openai-responses", Dialect: "openai"}
			item := snapshot.recommend(config, test.id)
			checkRecommendation(t, item, "context_window", test.context)
			checkRecommendation(t, item, "max_output_tokens", test.output)
			if item.ID != test.id || item.RecommendationSources["context_window"].Source != modelsDevURL+"#"+test.contextSource || item.RecommendationSources["max_output_tokens"].Source != modelsDevURL+"#"+test.outputSource {
				t.Fatalf("case fallback changed identity or source ranking: %+v", item)
			}
		})
	}
}

func TestCatalogExactCaseBucketNeverBorrowsAnotherCasesFields(t *testing.T) {
	snapshot := fixtureSnapshot(t, `{
 "one":{"models":{"Kimi-Exact":{"limit":{"context":120000},"reasoning":true}}},
 "moonshotai":{"models":{"kimi-exact":{"limit":{"context":1048576,"output":131072},"tool_call":true,"modalities":{"input":["image"]},"reasoning":true,"reasoning_options":[{"type":"effort","values":["high"]}]}}}
 }`)
	config := ProviderConfig{PresetID: "commandcode", Protocol: "openai-responses", Dialect: "openai"}
	id := "moonshotai/Kimi-Exact"
	item := enrichDiscovery(config, snapshot, Discovery{ID: id, Name: id})
	checkRecommendation(t, item, "context_window", "120000")
	if item.RecommendationSources["context_window"].Source != modelsDevURL+"#one/Kimi-Exact" {
		t.Fatal("an exact-case candidate was displaced by a different-case maker")
	}
	for _, field := range []string{"max_output_tokens", "tool_calling", "images", "reasoning.efforts"} {
		if value, present := flattenRecommendations(item.Recommendations)[field]; present {
			t.Fatalf("exact-case bucket borrowed %s=%s from another spelling", field, value)
		}
	}
	resolved := resolveCatalogItem(t, config, item)
	checkCatalogControls(t, resolved.Controls, models.ReasoningFormatOpenAIResponses, []models.ReasoningOption{catalogDefaultOption()})
	checkCatalogRequest(t, catalogAdapter(t, resolved), id, core.ReasoningOptions{}, 0, nil)
}

func TestCatalogCaseFallbackDoesNotNormalizeVersionsOrSuffixes(t *testing.T) {
	snapshot := fixtureSnapshot(t, caseCatalogFixture)
	config := ProviderConfig{PresetID: "commandcode", Protocol: "openai-responses", Dialect: "openai"}
	for _, id := range []string{"moonshotai/Kimi-K3.1-Pro:fast", "moonshotai/Kimi-K3.1-Pro-20261027", "moonshotai/Kimi-K3.1-Pro-preview", "moonshotai/Kimi-K3.1", "moonshotai/Kimi-K3.1-Pro (latest)"} {
		if item := snapshot.recommend(config, id); item.ID != id || len(item.Recommendations) != 0 {
			t.Fatalf("case fallback matched a different version or suffix: %+v", item)
		}
	}
}

func TestCatalogCaseFallbackAggregatesConflictingCandidates(t *testing.T) {
	snapshot := fixtureSnapshot(t, `{
 "gateway":{"models":{
  "a/opaque-future":{"limit":{"context":128000,"output":1000}},
  "b/OPAQUE-FUTURE":{"limit":{"context":128000,"output":2000}},
  "c/opaque-future":{"limit":{"context":128000,"output":1000}}
 }}
 }`)
	config := ProviderConfig{PresetID: "gateway", Protocol: "openai-responses", Dialect: "openai"}
	item := snapshot.recommend(config, "remote/Opaque-Future")
	checkRecommendation(t, item, "context_window", "128000")
	if _, present := item.Recommendations["max_output_tokens"]; present {
		t.Fatalf("case fallback chose a first, last, or majority output value: %v", item.Recommendations)
	}
	if source := item.RecommendationSources["context_window"].Source; source != modelsDevURL+"#gateway/a/opaque-future" {
		t.Fatalf("combined fallback candidates have unstable provenance: %s", source)
	}
}

func TestCatalogCaseFallbackFullIDPriorityDoesNotReintroduceLastWins(t *testing.T) {
	for _, source := range []struct {
		name, preset, catalog string
	}{
		{"current provider", "commandcode", `{
 "moonshotai":{"models":{"kimi-k3.1-pro":{"limit":{"context":1048576,"output":65536}}}},
 "commandcode":{"models":{"moonshotai/kimi-k3.1-pro":{"limit":{"context":1048576,"output":131072}},"MoonshotAI/KIMI-K3.1-PRO":{"limit":{"context":1048576,"output":262144}}}}
 }`},
		{"explicit namespace", "", `{
 "moonshotai":{"models":{"kimi-k3.1-pro":{"limit":{"context":1048576,"output":131072}},"moonshotai/KIMI-K3.1-PRO":{"limit":{"context":1048576,"output":262144}}}}
 }`},
	} {
		t.Run(source.name, func(t *testing.T) {
			snapshot := fixtureSnapshot(t, source.catalog)
			id := "moonshotai/Kimi-K3.1-Pro"
			item := snapshot.recommend(ProviderConfig{PresetID: source.preset, Protocol: "openai-responses", Dialect: "openai"}, id)
			checkRecommendation(t, item, "context_window", "1048576")
			if item.ID != id {
				t.Fatalf("full-ID matching changed the remote spelling: %s", item.ID)
			}
			if _, present := item.Recommendations["max_output_tokens"]; present {
				t.Fatalf("same-priority full-ID aliases with conflicting output limits chose a winner: %v", item.Recommendations)
			}
		})
	}
}

func TestCatalogExactCaseFullIDWinsBeforeFoldingNamespaceAliases(t *testing.T) {
	for _, test := range []struct {
		name, preset, catalog, outputSource string
	}{
		{"current provider", "commandcode", `{
 "commandcode":{"models":{"moonshotai/Kimi-ExactFull":{"limit":{"context":128000,"output":131072}},"MoonshotAI/Kimi-ExactFull":{"limit":{"context":128000,"output":262144}}}}
 }`, "commandcode/moonshotai/Kimi-ExactFull"},
		{"explicit namespace", "", `{
 "moonshotai":{"models":{"Kimi-ExactFull":{"limit":{"context":128000,"output":131072}},"MoonshotAI/Kimi-ExactFull":{"limit":{"context":128000,"output":262144}}}}
 }`, "moonshotai/Kimi-ExactFull"},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := "moonshotai/Kimi-ExactFull"
			item := fixtureSnapshot(t, test.catalog).recommend(ProviderConfig{PresetID: test.preset}, id)
			checkRecommendation(t, item, "max_output_tokens", "131072")
			if item.ID != id || item.RecommendationSources["max_output_tokens"].Source != modelsDevURL+"#"+test.outputSource {
				t.Fatalf("full-ID fallback overrode an exact spelling: %+v", item)
			}
		})
	}
}

func TestCatalogCaseFallbackPreservesLiveNullFalseAndEmpty(t *testing.T) {
	snapshot := fixtureSnapshot(t, caseCatalogFixture)
	config := ProviderConfig{PresetID: "commandcode", Protocol: "openai-responses", Dialect: "openai"}
	for _, test := range []struct {
		name   string
		values map[string]json.RawMessage
		want   map[string]string
	}{
		{"null", map[string]json.RawMessage{"max_output_tokens": raw(nil), "reasoning": raw(nil)}, map[string]string{"max_output_tokens": "null", "reasoning": "null"}},
		{"false", map[string]json.RawMessage{"tool_calling": raw("unsupported"), "reasoning": raw(map[string]any{"supported": false})}, map[string]string{"tool_calling": `"unsupported"`, "reasoning.supported": "false"}},
		{"empty", map[string]json.RawMessage{"reasoning": raw(map[string]any{"toggle": false, "disable": false, "efforts": []string{}})}, map[string]string{"reasoning.toggle": "false", "reasoning.disable": "false", "reasoning.efforts": `[]`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			live := Discovery{ID: "moonshotai/Kimi-K3.1-Pro", Source: "https://directory.invalid/models", Recommendations: test.values}
			markRecommendationSources(&live, time.Now())
			item := enrichDiscovery(config, snapshot, live)
			checkRecommendation(t, item, "context_window", "1048576")
			for field, want := range test.want {
				checkRecommendation(t, item, field, want)
				if source := item.RecommendationSources[field].Source; source != live.Source {
					t.Fatalf("explicit live %s lost priority after case fallback: %s", field, source)
				}
			}
			if test.name == "false" {
				for field := range flattenRecommendations(item.Recommendations) {
					if strings.HasPrefix(field, "reasoning.") && field != "reasoning.supported" {
						t.Fatalf("disabled live reasoning inherited fallback %s", field)
					}
				}
			}
		})
	}
}
