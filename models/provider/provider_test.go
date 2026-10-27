package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/provider"
	"github.com/oines/astral-core/models/provider/anthropic"
	"github.com/oines/astral-core/models/provider/google"
	"github.com/oines/astral-core/models/provider/openai/completions"
	"github.com/oines/astral-core/models/provider/openai/responses"
)

func config(id, scope string) models.Config {
	streaming := false
	return models.Config{Spec: core.ModelSpec{ID: id, Revision: "1", ContextWindow: 100000, MaxOutputTokens: 8192}, Model: "remote-" + id, ReplayScope: scope, Streaming: &streaming}
}

func origin(protocol, scope, model, field string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"astral_models": map[string]string{"protocol": protocol, "scope": scope, "model": model, "field": field}})
	return raw
}

func requestFor(model core.Model, messages []core.Message) core.Request {
	return core.Request{ModelID: model.Spec().ID, ModelRevision: model.Spec().Revision, MaxOutputTokens: 1024, Messages: messages}
}

func TestDirectCallSwitchesTargetsWithoutChangingHistory(t *testing.T) {
	var requestsA, requestsB []string
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Error(err)
		}
		requestsA = append(requestsA, string(raw))
		fmt.Fprint(w, `{"id":"ra","status":"completed","output":[{"type":"reasoning","id":"r1","summary":[],"encrypted_content":"cipher-A"},{"type":"function_call","id":"f1","call_id":"call|unsafe","name":"lookup","arguments":"{}"},{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"A-text"}]}]}`)
	}))
	defer serverA.Close()
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Error(err)
		}
		requestsB = append(requestsB, string(raw))
		fmt.Fprint(w, `{"id":"rb","choices":[{"index":0,"message":{"role":"assistant","content":"B-text","reasoning_content":"private-B"},"finish_reason":"stop"}]}`)
	}))
	defer serverB.Close()
	cfgA, cfgB := config("A", "scope-A"), config("B", "scope-B")
	cfgA.BaseURL, cfgB.BaseURL = serverA.URL, serverB.URL
	modelA, err := provider.New(responses.Protocol, cfgA)
	if err != nil {
		t.Fatal(err)
	}
	modelB, err := provider.New(completions.Protocol, cfgB)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	replyA, err := provider.Call(ctx, modelA, core.Request{Messages: []core.Message{core.User("go")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	history := []core.Message{core.User("go"), replyA.Message, {Role: core.RoleTool, ToolCallID: "call|unsafe", Content: core.Text("result")}}
	before, _ := json.Marshal(history)
	replyB, err := provider.Call(ctx, modelB, requestFor(modelB, history), nil)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(history)
	if string(before) != string(after) {
		t.Fatal("target preparation mutated durable source history")
	}
	if strings.Contains(requestsB[0], "cipher-A") || !strings.Contains(requestsB[0], "A-text") || !strings.Contains(requestsB[0], "result") {
		t.Fatalf("B projection: %s", requestsB[0])
	}
	var body struct {
		Messages []struct {
			ToolCalls []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		}
	}
	if err := json.Unmarshal([]byte(requestsB[0]), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages[1].ToolCalls) != 1 || body.Messages[1].ToolCalls[0].ID == "call|unsafe" || body.Messages[1].ToolCalls[0].ID != body.Messages[2].ToolCallID {
		t.Fatalf("foreign tool identity was not paired: %s", requestsB[0])
	}
	if len(replyB.Message.Content) != 2 || len(replyB.Message.Content[0].Data) == 0 {
		t.Fatal("new target stopped accumulating native reasoning")
	}
	history = append(history, replyB.Message, core.User("back to A"))
	if _, err := provider.Call(ctx, modelA, requestFor(modelA, history), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requestsA[1], "cipher-A") || !strings.Contains(requestsA[1], "B-text") || strings.Contains(requestsA[1], "private-B") {
		t.Fatalf("A return projection: %s", requestsA[1])
	}
}

func TestEveryProtocolProjectsForeignNativeContent(t *testing.T) {
	native := origin(responses.Protocol, "source", "source-model", "responses.item")
	messages := []core.Message{core.User("go"), {Role: core.RoleAssistant, FinishReason: core.FinishToolCalls, Content: []core.Content{
		{Type: "reasoning", Data: json.RawMessage(`{"type":"reasoning","encrypted_content":"secret"}`), Metadata: native},
		{Type: "text", Text: "portable", Data: json.RawMessage(`{"type":"message","content":[]}`), Metadata: native},
		{Type: "tool_call", ToolCall: &core.ToolCall{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{}`)}, Data: json.RawMessage(`{"type":"function_call"}`), Metadata: native},
	}}, {Role: core.RoleTool, ToolCallID: "call-1", Content: core.Text("result")}, {Role: core.RoleAssistant, Content: []core.Content{{Type: "reasoning", Data: json.RawMessage(`{"type":"reasoning"}`), Metadata: native}}}, core.User("continue")}
	for _, protocol := range []string{completions.Protocol, responses.Protocol, anthropic.Protocol, google.Protocol} {
		t.Run(protocol, func(t *testing.T) {
			model, err := provider.New(protocol, config("target", "target-scope"))
			if err != nil {
				t.Fatal(err)
			}
			request := requestFor(model, messages)
			prepared, err := core.PrepareRequest(context.Background(), model, request)
			if err != nil {
				t.Fatal(err)
			}
			if len(prepared.Messages) != 4 || len(prepared.Messages[1].Content) != 2 {
				t.Fatalf("private-only messages survived: %+v", prepared.Messages)
			}
			body, err := model.RequestBody(prepared)
			if err != nil {
				t.Fatal(err)
			}
			wire, _ := json.Marshal(body)
			if strings.Contains(string(wire), "secret") || !strings.Contains(string(wire), "portable") || !strings.Contains(string(wire), "call-1") || !strings.Contains(string(wire), "result") {
				t.Fatalf("projection: %s", wire)
			}
			twice, err := core.PrepareRequest(context.Background(), model, prepared)
			if err != nil || !reflect.DeepEqual(twice, prepared) {
				t.Fatal("preparation is not idempotent", err)
			}
		})
	}
}

func TestSnapshotRestoresFinalConfigurationWithoutCredentials(t *testing.T) {
	cfg := config("snapshot", "")
	cfg.APIKey = "SECRET-KEY"
	cfg.Headers = map[string]string{"Authorization": "SECRET-HEADER"}
	cfg.HTTPClient = &http.Client{}
	cfg.ReasoningFormat = models.ReasoningFormatOpenAIResponses
	cfg.Reasoning = &models.ReasoningCapabilities{Efforts: []string{"vendor-level"}}
	frozen, err := provider.Freeze(responses.Protocol, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(frozen.Data), "SECRET") || strings.Contains(string(frozen.Data), "Authorization") || strings.Contains(string(frozen.Data), "HTTPClient") {
		t.Fatalf("credentials leaked: %s", frozen.Data)
	}
	restored, err := provider.Restore(frozen, provider.Credentials{APIKey: "ROTATED-KEY"})
	if err != nil {
		t.Fatal(err)
	}
	normalized := restored.SnapshotConfig()
	if normalized.ReplayScope == "" || normalized.APIKey != "" || normalized.Headers != nil || normalized.ReasoningFormat != models.ReasoningFormatOpenAIResponses || !reflect.DeepEqual(normalized.Reasoning.Efforts, []string{"vendor-level"}) {
		t.Fatalf("bad restore: %+v", normalized)
	}
	normalized.Reasoning.Efforts[0] = "mutated"
	if restored.SnapshotConfig().Reasoning.Efforts[0] != "vendor-level" {
		t.Fatal("snapshot exposed mutable adapter state")
	}
	frozen.Spec.ID = "different"
	if _, err := provider.Restore(frozen, provider.Credentials{}); err == nil {
		t.Fatal("accepted changed snapshot identity")
	}
}

func TestFrozenImagesAreEncodedByEveryAdapter(t *testing.T) {
	for _, protocol := range []string{completions.Protocol, responses.Protocol, anthropic.Protocol, google.Protocol} {
		t.Run(protocol, func(t *testing.T) {
			cfg := config("image", "scope")
			cfg.Images = "supported"
			model, err := provider.New(protocol, cfg)
			if err != nil {
				t.Fatal(err)
			}
			req := requestFor(model, []core.Message{{Role: core.RoleUser, Content: []core.Content{{Type: "image", MediaType: "image/png", Data: json.RawMessage(`"eA=="`), URL: "host-file://original"}}}})
			body, err := model.RequestBody(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(body)
			if !strings.Contains(string(raw), "eA==") || strings.Contains(string(raw), "host-file") {
				t.Fatalf("image not encoded: %s", raw)
			}
			if string(req.Messages[0].Content[0].Data) != `"eA=="` {
				t.Fatal("image snapshot mutated")
			}
			short, err := model.EstimateTokens(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			// A larger valid payload must not be charged as text.
			req.Messages[0].Content[0].Data = json.RawMessage(`"` + strings.Repeat("eHh4", 1000) + `"`)
			long, err := model.EstimateTokens(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if short != long {
				t.Fatalf("base64 charged as text: %d != %d", short, long)
			}
			cfg.Images = "unsupported"
			model, err = provider.New(protocol, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := model.RequestBody(req); err == nil {
				t.Fatal("unsupported image accepted")
			}
		})
	}
}

func TestNativeToolIDCanBeReusedAfterForeignBatch(t *testing.T) {
	cfg := config("target", "scope")
	model, err := provider.New(responses.Protocol, cfg)
	if err != nil {
		t.Fatal(err)
	}
	call := func(scope string) core.Message {
		return core.Message{Role: core.RoleAssistant, FinishReason: core.FinishToolCalls, Content: []core.Content{{Type: "tool_call", ToolCall: &core.ToolCall{ID: "shared|id", Name: "lookup", Arguments: json.RawMessage(`{}`)}, Data: json.RawMessage(`{"type":"function_call","call_id":"shared|id","name":"lookup","arguments":"{}"}`), Metadata: origin(responses.Protocol, scope, cfg.Model, "responses.item")}}}
	}
	result := core.Message{Role: core.RoleTool, ToolCallID: "shared|id", Content: core.Text("result")}
	req := requestFor(model, []core.Message{core.User("go"), call("other"), result, call("scope"), result})
	prepared, err := core.PrepareRequest(context.Background(), model, req)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Messages[1].Content[0].ToolCall.ID != prepared.Messages[2].ToolCallID {
		t.Fatal("foreign pair broken")
	}
	if prepared.Messages[3].Content[0].ToolCall.ID != "shared|id" || prepared.Messages[4].ToolCallID != "shared|id" {
		t.Fatal("earlier foreign normalization changed later accepted native pair")
	}
}

func TestDeclaredToolCapabilityDoesNotEraseHistoricalTools(t *testing.T) {
	cfg := config("tools", "scope")
	cfg.ToolCalling = "unsupported"
	model, err := provider.New(completions.Protocol, cfg)
	if err != nil {
		t.Fatal(err)
	}
	req := requestFor(model, []core.Message{core.User("go"), {Role: core.RoleAssistant, Content: []core.Content{{Type: "tool_call", ToolCall: &core.ToolCall{ID: "call1", Name: "lookup", Arguments: json.RawMessage(`{}`)}}}}, {Role: core.RoleTool, ToolCallID: "call1", Content: core.Text("result")}})
	if _, err := model.RequestBody(req); err != nil {
		t.Fatal("historical tool exchange should survive", err)
	}
	req.Tools = []core.ToolDefinition{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}
	if _, err := model.RequestBody(req); err == nil {
		t.Fatal("explicit unsupported tool declaration accepted")
	}
	cfg.ToolCalling = "unknown"
	model, err = provider.New(completions.Protocol, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.RequestBody(req); err != nil {
		t.Fatal("unknown capability should permit explicit invocation", err)
	}
}

func TestSnapshotRejectsQueryCredentialsInBaseURL(t *testing.T) {
	for _, protocol := range []string{completions.Protocol, responses.Protocol, anthropic.Protocol, google.Protocol} {
		t.Run(protocol, func(t *testing.T) {
			for _, suffix := range []string{"?api_key=SECRET-QUERY-KEY", "?api_version=version", "?"} {
				cfg := config("snapshot-query", "scope")
				cfg.BaseURL = "https://api.example.invalid/v1" + suffix
				frozen, err := provider.Freeze(protocol, cfg)
				if !errors.Is(err, core.ErrInvalid) {
					t.Fatalf("query base URL was not rejected: %v", err)
				}
				if len(frozen.Data) != 0 {
					t.Fatal("rejected URL produced a durable snapshot")
				}
				if strings.Contains(err.Error(), "SECRET-QUERY-KEY") {
					t.Fatal("query credential leaked into validation error")
				}
			}
		})
	}
}

func TestDirectCallKeepsUnknownModelBudgetsUnknown(t *testing.T) {
	for _, protocol := range []string{completions.Protocol, responses.Protocol, anthropic.Protocol, google.Protocol} {
		t.Run(protocol, func(t *testing.T) {
			var captured []map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				captured = append(captured, body)
				switch protocol {
				case completions.Protocol:
					fmt.Fprint(w, `{"id":"reply","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
				case responses.Protocol:
					fmt.Fprint(w, `{"id":"reply","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
				case anthropic.Protocol:
					fmt.Fprint(w, `{"id":"reply","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
				case google.Protocol:
					fmt.Fprint(w, `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
				}
			}))
			defer server.Close()
			streaming := false
			cfg := models.Config{Model: "runtime-new-model", BaseURL: server.URL, Streaming: &streaming}
			model, err := provider.New(protocol, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if model.Spec().ID != cfg.Model || model.Spec().ContextWindow != 0 || model.Spec().MaxOutputTokens != 0 {
				t.Fatalf("invented model budget: %+v", model.Spec())
			}
			req := core.Request{Messages: []core.Message{core.User("classify")}}
			_, err = provider.Call(context.Background(), model, req, nil)
			if protocol == anthropic.Protocol {
				if !errors.Is(err, core.ErrInvalid) || len(captured) != 0 {
					t.Fatalf("Messages must require explicit output before HTTP: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, present := outputLimit(protocol, captured[0]); present {
					t.Fatalf("omitted output acquired a made-up limit: %+v", captured[0])
				}
			}
			req.MaxOutputTokens = 321
			if _, err := provider.Call(context.Background(), model, req, nil); err != nil {
				t.Fatal("explicit limit with unknown model maximum", err)
			}
			if limit, present := outputLimit(protocol, captured[len(captured)-1]); !present || limit != float64(321) {
				t.Fatalf("explicit limit lost: %+v", captured)
			}
			req.MaxOutputTokens = -1
			if _, err := provider.Call(context.Background(), model, req, nil); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("negative output accepted: %v", err)
			}
			// A known upper bound is still enforced; omission still means API default.
			cfg.Spec.MaxOutputTokens = 128
			bounded, err := provider.New(protocol, cfg)
			if err != nil {
				t.Fatal(err)
			}
			req.MaxOutputTokens = 129
			if _, err := provider.Call(context.Background(), bounded, req, nil); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("known output maximum ignored: %v", err)
			}
			req.MaxOutputTokens = 0
			_, err = provider.Call(context.Background(), bounded, req, nil)
			if protocol == anthropic.Protocol {
				if !errors.Is(err, core.ErrInvalid) {
					t.Fatal("Messages invented an output limit from model maximum")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, present := outputLimit(protocol, captured[len(captured)-1]); present {
					t.Fatal("direct call replaced API default with known maximum")
				}
			}
		})
	}
}

func outputLimit(protocol string, body map[string]any) (any, bool) {
	switch protocol {
	case completions.Protocol:
		value, ok := body["max_completion_tokens"]
		return value, ok
	case responses.Protocol:
		value, ok := body["max_output_tokens"]
		return value, ok
	case anthropic.Protocol:
		value, ok := body["max_tokens"]
		return value, ok
	default:
		value, ok := body["generationConfig"].(map[string]any)["maxOutputTokens"]
		return value, ok
	}
}

func TestDirectModelRejectsNegativeMetadata(t *testing.T) {
	for _, spec := range []core.ModelSpec{{ContextWindow: -1}, {MaxOutputTokens: -1}, {MaxInputTokens: -1}} {
		if _, err := provider.New(responses.Protocol, models.Config{Model: "runtime-model", Spec: spec}); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("negative metadata accepted: %+v (%v)", spec, err)
		}
	}
	if _, err := provider.New(responses.Protocol, models.Config{}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("missing remote model accepted: %v", err)
	}
}
