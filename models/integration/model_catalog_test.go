package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const catalogFixture = `{
 "openai":{"models":{"gpt-fixture":{"name":"Original model","limit":{"context":128000,"output":16000},"tool_call":true,"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","high"]}],"cost":{"input":1},"interleaved":{"field":"reasoning_content"}}}},
 "openrouter":{"models":{"openai/gpt-fixture":{"limit":{"context":64000},"tool_call":false,"reasoning_options":[],"cost":{"input":2}}}},
 "other":{"models":{"openai/gpt-fixture":{"limit":{"context":32000},"reasoning":true,"reasoning_options":[{"type":"effort","values":["medium"]}]}}}
}`

func fixtureSnapshot(t *testing.T, fixture string) *catalogSnapshot {
	t.Helper()
	snapshot, err := parseModelCatalog([]byte(fixture), time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func checkRecommendation(t *testing.T, item Discovery, field, expected string) {
	t.Helper()
	got := flattenRecommendations(item.Recommendations)[field]
	if !sameJSON(got, json.RawMessage(expected)) {
		t.Fatalf("%s: got %s, want %s; all=%v", field, got, expected, item.Recommendations)
	}
}

func TestRuntimeCatalogMatchesSlugAndPreservesFullIdentity(t *testing.T) {
	snapshot := fixtureSnapshot(t, catalogFixture)
	provider := ProviderConfig{Protocol: "openai-completions", Dialect: "compatible"}
	for _, id := range []string{"gpt-fixture", "vendor/gpt-fixture", "gateway/vendor/gpt-fixture", "GPT-fixture", "vendor/GPT-fixture"} {
		item := snapshot.recommend(provider, id)
		if item.ID != id {
			t.Fatal("remote identity changed")
		}
		checkRecommendation(t, item, "context_window", "128000")
		checkRecommendation(t, item, "reasoning.efforts", `["low","high"]`)
		if item.RecommendationSources["context_window"].Source != modelsDevURL+"#openai/gpt-fixture" {
			t.Fatalf("custom connection lost maker provenance: %+v", item.RecommendationSources)
		}
		for _, field := range []string{"reasoning_format", "reasoning_mapping", "defaults"} {
			if _, exists := item.Recommendations[field]; exists {
				t.Fatalf("metadata selected wire setting %s", field)
			}
		}
	}
	for _, id := range []string{"gpt-fixture:free", "gpt-fixture-20261023", "gpt-fixture-v2"} {
		if item := snapshot.recommend(provider, id); len(item.Recommendations) != 0 {
			t.Fatalf("non-exact slug matched: %s", id)
		}
	}
}

func TestRuntimeCatalogMakerFallbackRestoresMiMoOnCustomAndGatewayConnections(t *testing.T) {
	snapshot := fixtureSnapshot(t, `{
 "xiaomi":{"models":{"mimo-v2.6-pro":{"limit":{"context":1048576,"output":131072},"tool_call":true,"reasoning":true,"reasoning_options":[{"type":"toggle"}]}}},
 "other":{"models":{"xiaomi/mimo-v2.6-pro":{"limit":{"context":1000000,"output":65536},"reasoning":true,"reasoning_options":[{"type":"effort","values":["medium"]}]}}}
 }`)
	for _, preset := range []string{"", "commandcode"} {
		provider := ProviderConfig{PresetID: preset, Protocol: "openai-responses", Dialect: "openai"}
		for _, id := range []string{"mimo-v2.6-pro", "xiaomi/mimo-v2.6-pro", "proxy/vendor/mimo-v2.6-pro"} {
			item := snapshot.recommend(provider, id)
			if item.ID != id || item.Protocol != provider.Protocol || item.Dialect != provider.Dialect {
				t.Fatalf("metadata changed the connection identity: %+v", item)
			}
			checkRecommendation(t, item, "context_window", "1048576")
			checkRecommendation(t, item, "max_output_tokens", "131072")
			checkRecommendation(t, item, "reasoning.toggle", "true")
			if item.RecommendationSources["max_output_tokens"].Source != modelsDevURL+"#xiaomi/mimo-v2.6-pro" {
				t.Fatalf("wrong output limit source: %+v", item.RecommendationSources)
			}
			for _, field := range []string{"reasoning_format", "reasoning_mapping", "defaults"} {
				if _, exists := item.Recommendations[field]; exists {
					t.Fatalf("maker metadata invented API configuration %s", field)
				}
			}
		}
	}
}

func TestRuntimeCatalogExplicitNamespaceAndCurrentProviderPriority(t *testing.T) {
	snapshot := fixtureSnapshot(t, `{
 "new-maker":{"models":{"future-model":{"limit":{"context":10000,"output":1000}}}},
 "gateway":{"models":{"new-maker/future-model":{"limit":{"context":8000,"output":500}}}},
 "other":{"models":{"future-model":{"limit":{"context":2000,"output":200}}}}
 }`)
	custom := ProviderConfig{Protocol: "openai-completions", Dialect: "compatible"}
	item := snapshot.recommend(custom, "new-maker/future-model")
	checkRecommendation(t, item, "max_output_tokens", "1000")
	if item.RecommendationSources["max_output_tokens"].Source != modelsDevURL+"#new-maker/future-model" {
		t.Fatal("an explicit new namespace did not select its exact catalog record")
	}
	if ambiguous := snapshot.recommend(custom, "future-model"); len(ambiguous.Recommendations["max_output_tokens"]) != 0 {
		t.Fatal("an unrecognized bare slug guessed a preferred source")
	}
	gateway := custom
	gateway.PresetID = "gateway"
	item = snapshot.recommend(gateway, "new-maker/future-model")
	checkRecommendation(t, item, "max_output_tokens", "500")
	if item.RecommendationSources["max_output_tokens"].Source != modelsDevURL+"#gateway/new-maker/future-model" {
		t.Fatal("current provider did not override maker metadata")
	}
}

func TestRuntimeCatalogUsesFieldwiseSourcePriority(t *testing.T) {
	snapshot := fixtureSnapshot(t, catalogFixture)
	provider := ProviderConfig{PresetID: "openrouter", Protocol: "openai-responses", Dialect: "openai"}
	item := snapshot.recommend(provider, "openai/gpt-fixture")
	checkRecommendation(t, item, "context_window", "64000")
	checkRecommendation(t, item, "max_output_tokens", "16000")
	checkRecommendation(t, item, "tool_calling", `"unsupported"`)
	checkRecommendation(t, item, "reasoning.efforts", `[]`)
	checkRecommendation(t, item, "reasoning.toggle", `false`)
	if item.RecommendationSources["max_output_tokens"].Source != modelsDevURL+"#openai/gpt-fixture" || item.RecommendationSources["context_window"].Source != modelsDevURL+"#openrouter/openai/gpt-fixture" {
		t.Fatal("field provenance does not reflect source priority")
	}
	// Mutating one returned recommendation cannot change the cached snapshot.
	item.Recommendations["context_window"][0] = '9'
	again := snapshot.recommend(provider, "openai/gpt-fixture")
	checkRecommendation(t, again, "context_window", "64000")
}

func TestRuntimeCatalogConflictsStayUnknown(t *testing.T) {
	snapshot := fixtureSnapshot(t, `{
 "one":{"models":{"x":{"limit":{"context":1000,"output":100},"reasoning":null}}},
 "two":{"models":{"x":{"limit":{"context":2000,"output":100},"reasoning":true,"reasoning_options":[{"type":"effort","values":["high"]}]}}}
 }`)
	item := snapshot.recommend(ProviderConfig{}, "vendor/x")
	if _, ok := item.Recommendations["context_window"]; ok {
		t.Fatal("conflicting context limits were guessed")
	}
	if _, ok := item.Recommendations["reasoning"]; ok {
		t.Fatal("null/object reasoning conflict was merged")
	}
	checkRecommendation(t, item, "max_output_tokens", "100")
	// Ambiguous same-provider aliases must not silently fall through to a
	// maker's different limit. An exact full ID can still resolve the ambiguity.
	snapshot = fixtureSnapshot(t, `{
 "openai":{"models":{"gpt-x":{"limit":{"context":3000,"output":100}}}},
 "openrouter":{"models":{"a/gpt-x":{"limit":{"context":1000}},"b/gpt-x":{"limit":{"context":2000}}}}
 }`)
	provider := ProviderConfig{PresetID: "openrouter"}
	item = snapshot.recommend(provider, "proxy/gpt-x")
	if _, ok := item.Recommendations["context_window"]; ok {
		t.Fatal("ambiguous gateway aliases inherited an unconfirmed maker limit")
	}
	checkRecommendation(t, item, "max_output_tokens", "100")
	checkRecommendation(t, snapshot.recommend(provider, "a/gpt-x"), "context_window", "1000")
}

func TestCatalogMergePreservesFalseEmptyAndNull(t *testing.T) {
	fallback := fixtureSnapshot(t, catalogFixture).recommend(ProviderConfig{PresetID: "openai"}, "gpt-fixture")
	live := Discovery{ID: "gpt-fixture", Source: "https://gateway.example/models", Recommendations: map[string]json.RawMessage{
		"context_window": raw(nil), "images": raw("unsupported"),
		"reasoning": raw(map[string]any{"toggle": false, "efforts": []string{}, "default_effort": nil}),
	}}
	markRecommendationSources(&live, time.Now())
	merged := mergeCatalogDiscovery(ProviderConfig{}, fallback, live)
	checkRecommendation(t, merged, "context_window", "null")
	checkRecommendation(t, merged, "reasoning.toggle", "false")
	checkRecommendation(t, merged, "reasoning.efforts", "[]")
	checkRecommendation(t, merged, "reasoning.default_effort", "null")
	if merged.RecommendationSources["reasoning.efforts"].Source != live.Source {
		t.Fatal("live source lost")
	}
	live.Recommendations["reasoning"] = raw(map[string]any{"supported": false})
	markRecommendationSources(&live, time.Now())
	merged = mergeCatalogDiscovery(ProviderConfig{}, fallback, live)
	if string(merged.Recommendations["reasoning"]) != `{"supported":false}` {
		t.Fatalf("unsupported live reasoning inherited controls: %s", merged.Recommendations["reasoning"])
	}
	live.Recommendations["reasoning"] = raw(nil)
	merged = mergeCatalogDiscovery(ProviderConfig{}, fallback, live)
	checkRecommendation(t, merged, "reasoning", "null")
}

func TestCatalogCacheRefreshKeepsLastValidSnapshot(t *testing.T) {
	var requests atomic.Int32
	var mode atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("public catalog received a credential or non-GET request")
		}
		switch mode.Load() {
		case 1:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		case 2:
			_, _ = w.Write([]byte(`{"invalid":true}`))
		default:
			_, _ = w.Write([]byte(catalogFixture))
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "cache", "models.dev.json")
	now := time.Date(2026, 10, 23, 1, 0, 0, 0, time.UTC)
	catalog := NewModelCatalog(path, server.Client())
	catalog.url, catalog.now = server.URL, func() time.Time { return now }
	if requests.Load() != 0 {
		t.Fatal("constructor performed I/O")
	}
	first, err := catalog.load(context.Background())
	if err != nil || first == nil || requests.Load() != 1 {
		t.Fatalf("initial load: %v %v", first, err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A new process starts from the persisted, fresh cache without a request.
	restarted := NewModelCatalog(path, server.Client())
	restarted.url, restarted.now = server.URL, catalog.now
	if _, err := restarted.load(context.Background()); err != nil || requests.Load() != 1 {
		t.Fatalf("fresh disk cache requested the network: %v", err)
	}
	now = now.Add(25 * time.Hour)
	mode.Store(1)
	stale, err := restarted.load(context.Background())
	if err == nil || stale == nil || !stale.FetchedAt.Equal(first.FetchedAt) {
		t.Fatal("failed refresh did not preserve last valid snapshot")
	}
	_, _ = restarted.load(context.Background())
	if requests.Load() != 2 {
		t.Fatal("failed public refresh was immediately repeated")
	}
	mode.Store(2)
	now = now.Add(time.Minute)
	if stale, err = restarted.load(context.Background()); err == nil || stale == nil {
		t.Fatal("invalid catalog discarded stale data")
	}
	if data, _ := os.ReadFile(path); string(data) != string(saved) {
		t.Fatal("failed refresh replaced the valid disk cache")
	}
	mode.Store(0)
	now = now.Add(time.Minute)
	fresh, err := restarted.load(context.Background())
	if err != nil || !fresh.FetchedAt.Equal(now) {
		t.Fatal("valid refresh did not replace stale data")
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".models-dev-*"))
	if len(files) != 0 {
		t.Fatal("atomic cache writer leaked temporary files")
	}
}

func TestCatalogCancellationDoesNotThrottleNextLookup(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(catalogFixture))
	}))
	defer server.Close()
	catalog := NewModelCatalog("", server.Client())
	catalog.url = server.URL
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := catalog.load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	if _, err := catalog.load(context.Background()); err != nil || requests.Load() != 1 {
		t.Fatalf("cancellation throttled a later request: %v", err)
	}
}

func TestCatalogConcurrentLookupsShareOneFetch(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(catalogFixture))
	}))
	defer server.Close()
	catalog := NewModelCatalog("", server.Client())
	catalog.url = server.URL
	var workers sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			snapshot, err := catalog.load(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			item := snapshot.recommend(ProviderConfig{PresetID: "openai"}, "gpt-fixture")
			checkRecommendation(t, item, "context_window", "128000")
			item.Recommendations["context_window"][0] = '9'
		}()
	}
	close(start)
	workers.Wait()
	if requests.Load() != 1 {
		t.Fatalf("concurrent lookups made %d public requests", requests.Load())
	}
}

func TestCatalogKeepsValidatedResponseWhenDiskCacheCannotBeWritten(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(catalogFixture))
	}))
	defer server.Close()
	// Renaming a cache file onto an existing directory must fail, while the
	// successfully parsed public response can still serve this process.
	catalog := NewModelCatalog(t.TempDir(), server.Client())
	catalog.url = server.URL
	snapshot, err := catalog.load(context.Background())
	if err == nil || snapshot == nil {
		t.Fatalf("valid response was discarded on disk failure: %v %v", snapshot, err)
	}
	checkRecommendation(t, snapshot.recommend(ProviderConfig{PresetID: "openai"}, "gpt-fixture"), "context_window", "128000")
	if _, err = catalog.load(context.Background()); err != nil {
		t.Fatalf("validated in-memory snapshot was not cached: %v", err)
	}
}

func TestPublicReasoningMetadataDistinguishesMissingEmptyAndNull(t *testing.T) {
	for _, tc := range []struct {
		name, fields, field, expected string
	}{
		{"missing", `"reasoning":true`, "reasoning.efforts", ""},
		{"empty", `"reasoning":true,"reasoning_options":[]`, "reasoning.efforts", "[]"},
		{"null-options", `"reasoning":true,"reasoning_options":null`, "reasoning.efforts", "null"},
		{"null-reasoning", `"reasoning":null`, "reasoning", "null"},
		{"none-is-a-native-level", `"reasoning":true,"reasoning_options":[{"type":"effort","values":["none","high"]}]`, "reasoning.efforts", `["none","high"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := fixtureSnapshot(t, `{"fixture":{"models":{"model":{`+tc.fields+`}}}}`)
			item := snapshot.recommend(ProviderConfig{}, "model")
			if tc.expected == "" {
				if _, exists := flattenRecommendations(item.Recommendations)[tc.field]; exists {
					t.Fatalf("unknown field %s was manufactured", tc.field)
				}
			} else {
				checkRecommendation(t, item, tc.field, tc.expected)
			}
			if _, exists := flattenRecommendations(item.Recommendations)["reasoning.off_effort"]; exists {
				t.Fatal("catalog inferred wire off semantics from a native effort string")
			}
		})
	}
}
