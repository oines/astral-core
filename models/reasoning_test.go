package models_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	core "github.com/oines/astral-core"
	models "github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/provider/anthropic"
	"github.com/oines/astral-core/models/provider/google"
	"github.com/oines/astral-core/models/provider/openai/completions"
	"github.com/oines/astral-core/models/provider/openai/responses"
)

func valuePtr[T any](value T) *T { return &value }

func reasoningConfig(format models.ReasoningFormat) models.Config {
	return models.Config{Spec: core.ModelSpec{ID: "profile", Revision: "1", ContextWindow: 100000, MaxOutputTokens: 8192}, Model: "opaque-model",
		ReasoningFormat: format, Reasoning: &models.ReasoningCapabilities{Toggle: valuePtr(true), Disable: valuePtr(true),
			DefaultEffort: "high", Efforts: []string{"low", "high", "vendor-future"}, OffEffort: "none", Budget: valuePtr(true), DynamicBudget: valuePtr(true)}}
}

func reasoningRequest() core.Request {
	return core.Request{ModelID: "profile", ModelRevision: "1", MaxOutputTokens: 8192, Messages: []core.Message{core.User("test")}}
}

type requestBodyModel interface {
	RequestBody(core.Request) (map[string]any, error)
}

func newReasoningModel(protocol string, config models.Config) (requestBodyModel, error) {
	switch protocol {
	case completions.Protocol:
		return completions.New(config)
	case responses.Protocol:
		return responses.New(config)
	case anthropic.Protocol:
		return anthropic.New(config)
	default:
		return google.New(config)
	}
}

func TestReasoningChoicesUseTheRequestEncoder(t *testing.T) {
	protocols := []string{completions.Protocol, responses.Protocol, anthropic.Protocol, google.Protocol}
	for _, protocol := range protocols {
		for _, format := range models.ReasoningFormats(protocol) {
			t.Run(protocol+"/"+string(format), func(t *testing.T) {
				cfg := reasoningConfig(format)
				if format == models.ReasoningFormatCustom {
					cfg.ReasoningMapping = &models.ReasoningMapping{On: json.RawMessage(`{"vendor":{"mode":"enabled"}}`), Off: json.RawMessage(`{"vendor":{"mode":"disabled"}}`),
						Efforts: map[string]json.RawMessage{"vendor-future": json.RawMessage(`{"vendor":{"mode":"future"}}`)}}
				}
				controls, err := models.ResolveReasoningControls(protocol, cfg)
				if err != nil {
					t.Fatal(err)
				}
				model, err := newReasoningModel(protocol, cfg)
				if err != nil {
					t.Fatal(err)
				}
				if len(controls.Options) == 0 || controls.Options[0].ID != "interface-default" {
					t.Fatalf("missing interface default: %+v", controls)
				}
				for _, option := range controls.Options {
					req := reasoningRequest()
					req.Options.Reasoning = option.Value
					body, err := model.RequestBody(req)
					if err != nil {
						t.Fatalf("advertised %s cannot execute: %v", option.ID, err)
					}
					encoded, err := models.EncodeReasoning(protocol, cfg, option.Value, req.MaxOutputTokens)
					if err != nil {
						t.Fatal(err)
					}
					for _, field := range encoded.OwnedFields {
						actual, actualSet := atPath(body, field)
						want, wantSet := atPath(encoded.Fields, field)
						if actualSet != wantSet || !reflect.DeepEqual(actual, want) {
							t.Fatalf("%s field %s: request=%v encoded=%v", option.ID, field, actual, want)
						}
					}
				}
				if controls.Budget != nil {
					req := reasoningRequest()
					budget := 0
					if controls.Budget.Min != nil {
						budget = *controls.Budget.Min
					}
					req.Options.Reasoning.BudgetTokens = &budget
					if _, err := model.RequestBody(req); err != nil {
						t.Fatalf("advertised budget cannot execute: %v", err)
					}
				}
			})
		}
	}
}

func atPath(fields map[string]any, path string) (any, bool) {
	var value any = fields
	for _, key := range strings.Split(path, ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		value, ok = object[key]
		if !ok {
			return nil, false
		}
	}
	return value, true
}

func TestReasoningNativeEncodingAndNoDefaultInjection(t *testing.T) {
	for _, tc := range []struct {
		format    models.ReasoningFormat
		protocol  string
		selection core.ReasoningOptions
		want      string
	}{
		{models.ReasoningFormatThinkingType, completions.Protocol, core.ReasoningOptions{Enabled: valuePtr(true)}, `{"thinking":{"type":"enabled"}}`},
		{models.ReasoningFormatQwen, completions.Protocol, core.ReasoningOptions{Enabled: valuePtr(false), BudgetTokens: valuePtr(0)}, `{"enable_thinking":false,"thinking_budget":0}`},
		{models.ReasoningFormatTogether, completions.Protocol, core.ReasoningOptions{Enabled: valuePtr(true)}, `{"reasoning":{"enabled":true}}`},
		{models.ReasoningFormatOpenAIResponses, responses.Protocol, core.ReasoningOptions{Effort: "vendor-future"}, `{"reasoning":{"effort":"vendor-future"}}`},
		{models.ReasoningFormatAnthropicBudget, anthropic.Protocol, core.ReasoningOptions{BudgetTokens: valuePtr(2048)}, `{"thinking":{"budget_tokens":2048,"type":"enabled"}}`},
		{models.ReasoningFormatGeminiBudget, google.Protocol, core.ReasoningOptions{Enabled: valuePtr(true)}, `{"generationConfig":{"thinkingConfig":{"thinkingBudget":-1}}}`},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			cfg := reasoningConfig(tc.format)
			got, err := models.EncodeReasoning(tc.protocol, cfg, tc.selection, 8192)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(got.Fields)
			if string(raw) != tc.want {
				t.Fatalf("got %s want %s", raw, tc.want)
			}
			blank, err := models.EncodeReasoning(tc.protocol, cfg, core.ReasoningOptions{}, 8192)
			if err != nil || len(blank.Fields) != 0 {
				t.Fatalf("interface default injected fields: %+v %v", blank, err)
			}
		})
	}
	cfg := reasoningConfig(models.ReasoningFormatOpenAIResponses)
	cfg.Reasoning.Efforts = []string{}
	cfg.Reasoning.OnEffort = "high"
	got, err := models.EncodeReasoning(responses.Protocol, cfg, core.ReasoningOptions{Enabled: valuePtr(true)}, 8192)
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := atPath(got.Fields, "reasoning.effort"); value != "high" {
		t.Fatalf("explicit on encoding lost: %+v", got)
	}
}

func TestReasoningRejectsUnknownAndConflictingSelections(t *testing.T) {
	cfg := reasoningConfig(models.ReasoningFormatThinkingType)
	for _, selection := range []core.ReasoningOptions{
		{Effort: "unknown"}, {Enabled: valuePtr(false), Effort: "high"}, {Effort: "high", BudgetTokens: valuePtr(2048)}, {Enabled: valuePtr(true), Effort: "none"},
	} {
		if _, err := models.EncodeReasoning(completions.Protocol, cfg, selection, 8192); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("accepted %+v: %v", selection, err)
		}
	}
	cfg.Reasoning = nil
	for _, selection := range []core.ReasoningOptions{{Enabled: valuePtr(true)}, {Enabled: valuePtr(false)}, {Effort: "high"}, {BudgetTokens: valuePtr(0)}} {
		if _, err := models.EncodeReasoning(completions.Protocol, cfg, selection, 8192); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("invented capability %+v", selection)
		}
	}
	cfg = reasoningConfig(models.ReasoningFormatAnthropicBudget)
	for _, budget := range []int{1023, 8192} {
		if _, err := models.EncodeReasoning(anthropic.Protocol, cfg, core.ReasoningOptions{BudgetTokens: &budget}, 8192); err == nil {
			t.Fatalf("accepted manual budget %d", budget)
		}
	}
}

func TestReasoningCustomMappingAndExtensionOwnership(t *testing.T) {
	for _, fragment := range []string{`{"model":"other"}`, `{"messages":[]}`, `{"generationConfig":{"maxOutputTokens":10}}`, `{"output_config":{"format":"x"}}`, `{"vendor":[]}`} {
		cfg := reasoningConfig(models.ReasoningFormatCustom)
		cfg.ReasoningMapping = &models.ReasoningMapping{On: json.RawMessage(fragment)}
		if _, err := models.ResolveReasoningControls(completions.Protocol, cfg); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("accepted mapping %s", fragment)
		}
	}
	cfg := reasoningConfig(models.ReasoningFormatCustom)
	cfg.Reasoning = nil
	cfg.ReasoningMapping = &models.ReasoningMapping{Fields: []string{"vendor.mode"}, On: json.RawMessage(`{"vendor":{"mode":true}}`), Off: json.RawMessage(`{"vendor":{"mode":false}}`), Efforts: map[string]json.RawMessage{"ultra": json.RawMessage(`{"vendor":{"mode":"ultra"}}`)}}
	controls, err := models.ResolveReasoningControls(completions.Protocol, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, option := range controls.Options {
		ids = append(ids, option.ID)
	}
	if !reflect.DeepEqual(ids, []string{"interface-default", "off", "on", "effort:ultra"}) {
		t.Fatalf("mapping options %v", ids)
	}
	cfg = reasoningConfig(models.ReasoningFormatOpenAIResponses)
	model, err := responses.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range []string{`{"reasoning":{"effort":"high"}}`, `{"reasoning":null}`, `{"thinking":false}`, `{"reasoning_effort":"high"}`} {
		req := reasoningRequest()
		req.Options.ProviderOptions = map[string]json.RawMessage{responses.Protocol: json.RawMessage(extra)}
		if _, err := model.RequestBody(req); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("extension bypassed default: %s", extra)
		}
	}
	req := reasoningRequest()
	req.Options.Reasoning.Effort = "high"
	req.Options.ProviderOptions = map[string]json.RawMessage{responses.Protocol: json.RawMessage(`{"reasoning":{"summary":"auto","effort":"high"}}`)}
	body, err := model.RequestBody(req)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := atPath(body, "reasoning.summary"); v != "auto" {
		t.Fatal("unrelated reasoning sibling lost")
	}
}

func TestReasoningCapabilitiesPresenceAndMetadata(t *testing.T) {
	unknown, _ := json.Marshal(models.ReasoningCapabilities{})
	empty, _ := json.Marshal(models.ReasoningCapabilities{Efforts: []string{}, Disable: valuePtr(false)})
	if strings.Contains(string(unknown), "efforts") || !strings.Contains(string(empty), `"efforts":[]`) || !strings.Contains(string(empty), `"disable":false`) {
		t.Fatalf("presence lost: %s %s", unknown, empty)
	}
	for _, tc := range []struct{ source, record, want string }{
		{"deepseek", `{"effort":{"supported_levels":["low","max","ultra"],"default_level":"max"}}`, `{"default_effort":"max","efforts":["low","max","ultra"]}`},
		{"deepseek", `{"effort":{"supported_levels":null,"default_level":null}}`, `{"default_effort":null,"efforts":null}`},
		{"codex", `{"supported_reasoning_levels":[{"effort":"xhigh","description":"native"}],"default_reasoning_level":"xhigh"}`, `{"default_effort":"xhigh","efforts":["xhigh"]}`},
		{"github-copilot", `{"capabilities":{"supports":{"reasoning":false}},"supported_reasoning_effort":[]}`, `{"efforts":[],"supported":false}`},
		{"magpie", `{"reasoning":false,"reasoning_levels":[]}`, `{"efforts":[],"supported":false}`},
		{"magpie", `{"reasoning":null,"reasoning_levels":["old"],"supported_reasoning_levels":null}`, `{"efforts":null,"supported":null}`},
		{"magpie", `{"reasoning":true,"reasoning_levels":["old"],"supported_reasoning_levels":[{"effort":"ultra"}]}`, `{"efforts":["ultra"],"supported":true}`},
		{"floway", `{"chat":{"reasoning":{"effort":{"supported":["none","ultra"],"default":"ultra"},"budget_tokens":{"min":0,"max":8192},"mandatory":true,"adaptive":true}}}`, `{"budget":true,"default_effort":"ultra","default_on":true,"disable":false,"efforts":["none","ultra"],"max_budget":8192,"min_budget":0,"supported":true,"toggle":false}`},
		{"floway", `{"chat":{"reasoning":{"effort":{"supported":[],"default":null},"budget_tokens":{},"mandatory":false,"adaptive":true}}}`, `{"budget":true,"default_effort":null,"efforts":[],"supported":true}`},
		{"floway", `{"chat":{"reasoning":{"budget_tokens":{"min":null,"max":2048}}}}`, `{"budget":true,"max_budget":2048,"min_budget":null,"supported":true}`},
		{"floway", `{"chat":{"reasoning":null}}`, `{"supported":null}`},
		{"floway", `{"chat":{}}`, `{}`},
		{"compatible", `{"reasoning":true,"reasoning_levels":["ultra"],"chat":{"reasoning":{"budget_tokens":{}}}}`, `{"supported":true}`},
		{"openrouter", `{"reasoning":{"mandatory":false,"default_enabled":false}}`, `{"budget":false,"default_on":false,"disable":true,"efforts":[],"supported":true,"toggle":true}`},
		{"openrouter", `{"id":"openrouter/auto"}`, `{}`},
		{"compatible", `{"id":"reasoner-2026"}`, `{}`},
	} {
		got, err := models.ParseReasoningMetadata(tc.source, json.RawMessage(tc.record))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(got)
		if string(raw) != tc.want {
			t.Fatalf("%s got %s want %s", tc.source, raw, tc.want)
		}
	}
	got, err := models.ParseReasoningMetadata("openrouter", json.RawMessage(`{"reasoning":{"mandatory":true,"supported_efforts":null}}`))
	if err != nil {
		t.Fatal(err)
	}
	var efforts []string
	_ = json.Unmarshal(got["efforts"], &efforts)
	if len(efforts) != 6 || slices.Contains(efforts, "none") || string(got["disable"]) != "false" {
		t.Fatalf("mandatory semantics lost: %v", got)
	}
}

func TestOmittedFormatUsesStrictProtocolDefault(t *testing.T) {
	cfg := reasoningConfig("")
	cfg.Dialect = models.DeepSeek
	encoded, err := models.EncodeReasoning(completions.Protocol, cfg, core.ReasoningOptions{Effort: "high"}, 8192)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := encoded.Fields["reasoning_effort"]; !ok {
		t.Fatalf("empty format guessed dialect encoding: %+v", encoded.Fields)
	}
	if _, ok := encoded.Fields["thinking"]; ok {
		t.Fatal("empty format retained legacy dialect behavior")
	}
	cfg.Reasoning = nil
	for _, selection := range []core.ReasoningOptions{{Enabled: valuePtr(true)}, {Enabled: valuePtr(false)}, {Effort: "high"}, {BudgetTokens: valuePtr(1024)}} {
		if _, err := models.EncodeReasoning(completions.Protocol, cfg, selection, 8192); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("undeclared reasoning selection accepted: %+v (%v)", selection, err)
		}
	}
	if _, err := models.EncodeReasoning(completions.Protocol, cfg, core.ReasoningOptions{}, 8192); err != nil {
		t.Fatal(err)
	}
}
