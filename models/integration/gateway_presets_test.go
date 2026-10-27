package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	models "github.com/oines/astral-core/models"
)

func TestSelfHostedGatewayDefaults(t *testing.T) {
	want := map[string]struct{ base, auth string }{
		"floway":  {"http://127.0.0.1:8788/v1", "api_key"},
		"litellm": {"http://127.0.0.1:4000/v1", "api_key"},
		"magpie":  {"http://127.0.0.1:3425/v1", "optional_key"},
	}
	items := gatewayPresets()
	if len(items) != len(want) {
		t.Fatalf("unexpected gateways: %+v", items)
	}
	for _, preset := range items {
		value, ok := want[preset.ID]
		if !ok || preset.BaseURL != value.base || preset.Auth != value.auth || !preset.RequiresURL || preset.Protocol != "openai-responses" || preset.Dialect != "openai" || preset.ReasoningFormat != models.ReasoningFormatOpenAIResponses || preset.DocsURL == "" {
			t.Fatalf("incorrect self-hosted default: %+v", preset)
		}
	}
}

func TestGatewayLiveMetadataIsSourceSpecific(t *testing.T) {
	floway := json.RawMessage(`{"id":"team/alias","limits":{"max_context_window_tokens":128000,"max_prompt_tokens":120000,"max_output_tokens":8000},"chat":{"modalities":{"input":["text","image"],"output":["text"]},"reasoning":{"mandatory":true,"adaptive":true,"effort":{"supported":["high","max"],"default":"high"},"budget_tokens":{"min":1024}}}}`)
	values, err := liveModelRecommendations(ProviderConfig{PresetID: "floway"}, floway)
	if err != nil {
		t.Fatal(err)
	}
	item := Discovery{Recommendations: values}
	for field, expected := range map[string]string{"context_window": "128000", "max_input_tokens": "120000", "max_output_tokens": "8000", "images": `"supported"`, "reasoning.supported": "true", "reasoning.efforts": `["high","max"]`, "reasoning.default_effort": `"high"`, "reasoning.toggle": "false", "reasoning.budget": "true", "reasoning.min_budget": "1024"} {
		checkRecommendation(t, item, field, expected)
	}
	if _, exists := flattenRecommendations(values)["reasoning.dynamic_budget"]; exists {
		t.Fatal("adaptive model reasoning was reinterpreted as a dynamic budget sentinel")
	}
	custom, err := liveModelRecommendations(ProviderConfig{Dialect: "compatible"}, floway)
	if err != nil || len(custom) != 0 {
		t.Fatalf("custom connection inherited Floway-only fields: %v %v", custom, err)
	}
	magpie := json.RawMessage(`{"context_window":64000,"max_input_tokens":64000,"max_output_tokens":4000,"reasoning":false,"supported_reasoning_levels":[],"reasoning_levels":["high"],"modalities":{"input":["text"]}}`)
	values, err = liveModelRecommendations(ProviderConfig{PresetID: "magpie"}, magpie)
	if err != nil {
		t.Fatal(err)
	}
	item.Recommendations = values
	for field, expected := range map[string]string{"max_input_tokens": "64000", "images": `"unsupported"`, "reasoning.supported": "false", "reasoning.efforts": `[]`} {
		checkRecommendation(t, item, field, expected)
	}
}

func TestFlowayPricesUseOnlyUniqueBaseRates(t *testing.T) {
	for _, test := range []struct {
		entries string
		input   string
		output  string
	}{
		{`[{"selector":{"serviceTier":"priority"},"rates":{"input_tokens":"0.000010","output_tokens":"0.000020"}},{"rates":{"input_tokens":"0.000002","output_tokens":"0.000004"}}]`, "2", "4"},
		{`[{"selector":{"inputTokens":{"operator":"gt","value":200000}},"rates":{"input_tokens":"0.000010"}}]`, "", ""},
		{`[{"rates":{"input_tokens":"0.000002"}},{"rates":{"input_tokens":"0.000004"}}]`, "", ""},
	} {
		values, err := liveModelRecommendations(ProviderConfig{PresetID: "floway"}, json.RawMessage(`{"pricing":{"entries":`+test.entries+`}}`))
		if err != nil {
			t.Fatal(err)
		}
		for field, expected := range map[string]string{"input_price": test.input, "output_price": test.output} {
			if expected == "" {
				if _, present := values[field]; present {
					t.Fatalf("conditional or conflicting price was used as base: %v", values)
				}
			} else {
				checkRecommendation(t, Discovery{Recommendations: values}, field, expected)
			}
		}
	}
}

func TestLiteLLMMetadataPreservesConfiguredBasePrefix(t *testing.T) {
	for _, prefix := range []string{"", "/v1", "/proxy/team/v1"} {
		t.Run(prefix, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case prefix + "/models":
					_, _ = w.Write([]byte(`{"data":[{"id":"team/alias"}]}`))
				case prefix + "/model/info":
					_, _ = w.Write([]byte(`{"data":[{"model_name":"team/alias","model_info":{"max_input_tokens":64000,"max_output_tokens":4000}}]}`))
				default:
					t.Errorf("configured gateway base prefix was rewritten: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			bridge := &Catalog{}
			provider := ProviderConfig{PresetID: "litellm", Protocol: "openai-responses", Dialect: "openai", BaseURL: server.URL + prefix}
			items, err := bridge.List(context.Background(), provider, "", nil)
			if err != nil || len(items) != 1 {
				t.Fatalf("catalog: %+v %v", items, err)
			}
			checkRecommendation(t, items[0], "context_window", "64000")
			if items[0].RecommendationSources["context_window"].Source != server.URL+prefix+"/model/info" {
				t.Fatal("metadata provenance lost the configured base prefix")
			}
		})
	}
}

func TestGatewayCatalogPreservesUserProtocolAndRemoteIdentity(t *testing.T) {
	for _, id := range []string{"floway", "magpie", "litellm"} {
		for _, protocol := range []string{"openai-responses", "anthropic-messages", "openai-completions"} {
			t.Run(id+"/"+protocol, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/model/info" {
						http.Error(w, "metadata not allowed", http.StatusForbidden)
						return
					}
					if r.URL.Path != "/v1/models" || r.Header.Get("User-Agent") != "astral-core" {
						t.Errorf("unexpected catalog request %s", r.URL)
					}
					_, _ = w.Write([]byte(`{"data":[{"id":"company/group/model:fast","display_name":"Team model","kind":"chat","context_window":64000,"max_output_tokens":8000,"native_endpoints":["/v1/messages"],"endpoints":{"anthropicMessages":{}},"supported_endpoints":["/responses"]},{"id":"image-generator","kind":"image"},{"id":"embedding-model","mode":"embedding"}]}`))
				}))
				defer server.Close()
				bridge := &Catalog{}
				provider := ProviderConfig{PresetID: id, Protocol: protocol, Dialect: "openai", BaseURL: server.URL + "/v1"}
				ctx := context.Background()
				items, err := bridge.List(ctx, provider, "", nil)
				if err != nil || len(items) != 1 {
					t.Fatalf("catalog: %+v %v", items, err)
				}
				if items[0].ID != "company/group/model:fast" || items[0].Name != "Team model" || items[0].Protocol != protocol {
					t.Fatalf("gateway identity/protocol was rewritten: %+v", items[0])
				}
			})
		}
	}
}

func TestLiteLLMDeploymentMetadataOnlyEnrichesListedAliases(t *testing.T) {
	var detailCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer client-virtual-key" || r.Header.Get("Gateway-Header") != "private" {
			t.Error("gateway request did not preserve configured credentials")
		}
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"team/fast","max_output_tokens":9000},{"id":"team/mixed"}]}`))
		case "/v1/model/info":
			detailCalls.Add(1)
			_, _ = w.Write([]byte(`{"data":[
			 {"model_name":"team/fast","litellm_params":{"model":"openai/private-upstream"},"model_info":{"id":"private-deployment","max_input_tokens":128000,"max_output_tokens":8000,"supports_reasoning":true,"supports_function_calling":true,"supports_vision":false,"input_cost_per_token":0.000002}},
			 {"model_name":"team/mixed","model_info":{"max_input_tokens":64000,"max_output_tokens":4000,"supports_reasoning":true}},
			 {"model_name":"team/mixed","model_info":{"max_input_tokens":32000,"max_output_tokens":4000,"supports_reasoning":false}},
			 {"model_name":"hidden-model","model_info":{"max_input_tokens":128000,"max_output_tokens":8000}}
			]}`))
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	bridge := &Catalog{}
	provider := ProviderConfig{PresetID: "litellm", Protocol: "openai-responses", Dialect: "openai", BaseURL: server.URL + "/v1"}
	key, headers := "client-virtual-key", map[string]string{"Gateway-Header": "private"}
	ctx := context.Background()
	items, err := bridge.List(ctx, provider, key, headers)
	if err != nil || len(items) != 2 || detailCalls.Load() != 1 {
		t.Fatalf("catalog: %+v %v; detail calls %d", items, err, detailCalls.Load())
	}
	for field, expected := range map[string]string{"context_window": "128000", "max_input_tokens": "128000", "max_output_tokens": "9000", "reasoning.supported": "true", "tool_calling": `"supported"`, "images": `"unsupported"`, "input_price": "2"} {
		checkRecommendation(t, items[0], field, expected)
	}
	if items[0].ID != "team/fast" || items[0].RecommendationSources["context_window"].Source != server.URL+"/v1/model/info" || items[0].RecommendationSources["max_output_tokens"].Source != server.URL+"/v1/models" {
		t.Fatalf("identity/provenance changed: %+v", items[0])
	}
	checkRecommendation(t, items[1], "max_output_tokens", "4000")
	for _, field := range []string{"context_window", "max_input_tokens", "reasoning"} {
		if _, present := items[1].Recommendations[field]; present {
			t.Fatalf("conflicting deployments guessed %s: %s", field, items[1].Recommendations[field])
		}
	}
	// Merely pointing a custom connection at the same URL does not authorize
	// vendor-specific auxiliary calls or reinterpret its metadata.
	provider.PresetID = ""
	if _, err := bridge.List(ctx, provider, key, headers); err != nil || detailCalls.Load() != 1 {
		t.Fatalf("custom provider probed LiteLLM metadata: %v, calls=%d", err, detailCalls.Load())
	}
}

func TestLiteLLMDeploymentConflictsDoNotFallThroughToPublicCatalog(t *testing.T) {
	provider := ProviderConfig{PresetID: "litellm", Protocol: "openai-responses", Dialect: "openai"}
	detail := consensusGatewayMetadata("team/gpt-fixture", "https://gateway.example/v1/model/info", []map[string]json.RawMessage{
		{"context_window": raw(64000), "max_output_tokens": raw(4000), "reasoning": raw(map[string]any{"supported": true, "efforts": []string{"high"}})},
		{"context_window": raw(nil), "max_output_tokens": raw(4000), "reasoning": raw(map[string]any{"supported": false})},
	})
	item := enrichDiscovery(provider, fixtureSnapshot(t, catalogFixture), detail)
	for _, field := range []string{"context_window", "reasoning"} {
		if _, present := item.Recommendations[field]; present {
			t.Fatalf("deployment conflict was replaced by public metadata: %s=%s", field, item.Recommendations[field])
		}
	}
	checkRecommendation(t, item, "max_output_tokens", "4000")
}

func TestLiteLLMOptionalMetadataFailureAndCrossOriginRedirect(t *testing.T) {
	var leaked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer other.Close()
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable, http.StatusFound, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/models" {
					_, _ = w.Write([]byte(`{"data":[{"id":"visible-model","max_input_tokens":64000,"max_output_tokens":4000}]}`))
					return
				}
				w.Header().Set("Location", other.URL)
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`invalid metadata`))
			}))
			defer server.Close()
			bridge := &Catalog{}
			provider := ProviderConfig{PresetID: "litellm", Protocol: "openai-responses", Dialect: "openai", BaseURL: server.URL + "/v1"}
			items, err := bridge.List(context.Background(), provider, "", nil)
			if err != nil || len(items) != 1 || items[0].ID != "visible-model" {
				t.Fatalf("optional metadata destroyed successful listing: %+v %v", items, err)
			}
			checkRecommendation(t, items[0], "context_window", "64000")
		})
	}
	if leaked.Load() != 0 {
		t.Fatal("metadata request followed a redirect to another origin")
	}
}

func TestCatalogRequiresAnInventoryEvenForHTTP200(t *testing.T) {
	for _, test := range []struct {
		body  string
		valid bool
	}{
		{`{"code":1001,"msg":"missing authorization","success":false}`, false},
		{`{"data":null}`, false},
		{`{"data":[]}`, true},
		{`{"models":[]}`, true},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(test.body)) }))
		bridge := &Catalog{}
		provider := ProviderConfig{Protocol: "openai-responses", Dialect: "openai", BaseURL: server.URL}
		items, err := bridge.List(context.Background(), provider, "", nil)
		server.Close()
		if test.valid != (err == nil) || test.valid && len(items) != 0 {
			t.Fatalf("inventory validity for %s: items=%+v err=%v", test.body, items, err)
		}
	}
}
