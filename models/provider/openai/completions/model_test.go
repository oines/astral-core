package completions_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	astral "github.com/oines/astral-core"
	models "github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/provider/openai/completions"
)

func ptr[T any](v T) *T { return &v }
func config() models.Config {
	return models.Config{Spec: astral.ModelSpec{ID: "profile:1", Revision: "1", ContextWindow: 100000, MaxOutputTokens: 8192},
		Model: "remote", ReplayScope: "connection:1", Reasoning: &models.ReasoningCapabilities{OffEffort: "none"}}
}
func request() astral.Request {
	return astral.Request{ModelID: "profile:1", ModelRevision: "1", MaxOutputTokens: 1000, Messages: []astral.Message{astral.User("hello")}}
}

func TestStructuredThinkingContentRoundTrip(t *testing.T) {
	for _, streaming := range []bool{true, false} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					for _, chunk := range []string{
						`{"id":"native","model":"remote","choices":[{"index":0,"delta":{"content":[{"type":"thinking","thinking":[{"type":"text","text":"look "}],"opaque":9007199254740993}]}}]}`,
						`{"choices":[{"index":0,"delta":{"content":[{"type":"thinking","thinking":[{"type":"text","text":"up"}],"signature":"retain"},{"type":"text","text":"Looking "}]}}]}`,
						`{"choices":[{"index":0,"delta":{"content":"now","tool_calls":[{"index":0,"id":"lookup-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
					} {
						_, _ = fmt.Fprint(w, "data: "+chunk+"\n\n")
					}
					_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprint(w, `{"id":"native","model":"remote","choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"thinking","thinking":[{"type":"text","text":"look up"}],"opaque":9007199254740993,"signature":"retain"},{"type":"text","text":"Looking now"}],"tool_calls":[{"id":"lookup-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
				}
			}))
			defer server.Close()
			cfg := config()
			cfg.BaseURL, cfg.Streaming = server.URL, &streaming
			m, err := completions.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			req := request()
			reply, err := m.Stream(context.Background(), req, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(reply.Message.Content) != 3 || reply.Message.Content[0].Type != "reasoning" || reply.Message.Content[0].Text != "look up" || reply.Message.Content[1].Text != "Looking now" {
				t.Fatalf("wrong native content: %+v", reply.Message.Content)
			}
			if reply.Message.FinishReason != astral.FinishToolCalls {
				t.Fatalf("wrong finish: %+v", reply.Message)
			}
			req.Messages = append(req.Messages, reply.Message, astral.Message{Role: astral.RoleTool, ToolCallID: "lookup-1", Content: astral.Text("found")})
			body, err := m.RequestBody(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(body)
			for _, field := range []string{`"signature":"retain"`, `"opaque":9007199254740993`, `"type":"thinking"`, `"tool_call_id":"lookup-1"`} {
				if !strings.Contains(string(raw), field) {
					t.Fatalf("lost native replay field %s in %s", field, raw)
				}
			}
			if strings.Contains(string(raw), "reasoning_content") {
				t.Fatalf("native thinking incorrectly flattened: %s", raw)
			}
			cfg.ReplayScope = "another-connection"
			other, err := completions.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := other.RequestBody(req); !errors.Is(err, astral.ErrInvalid) {
				t.Fatalf("opaque native content crossed route: %v", err)
			}
		})
	}
}

func TestDialectReasoningControls(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dialect models.Dialect
		options astral.ReasoningOptions
		want    string
		bad     bool
	}{
		{"qwen", models.Qwen, astral.ReasoningOptions{Enabled: ptr(false), BudgetTokens: ptr(0)}, `"enable_thinking":false`, false},
		{"qwen effort", models.Qwen, astral.ReasoningOptions{Effort: "high"}, "", true},
		{"deepseek", models.DeepSeek, astral.ReasoningOptions{Enabled: ptr(true), Effort: "max"}, `"thinking":{"type":"enabled"}`, false},
		{"deepseek budget", models.DeepSeek, astral.ReasoningOptions{BudgetTokens: ptr(1024)}, "", true},
		{"openrouter effort", models.OpenRouter, astral.ReasoningOptions{Effort: "xhigh"}, `"reasoning":{"effort":"xhigh"}`, false},
		{"openrouter budget", models.OpenRouter, astral.ReasoningOptions{BudgetTokens: ptr(1024)}, `"reasoning":{"max_tokens":1024}`, false},
		{"openrouter conflict", models.OpenRouter, astral.ReasoningOptions{Effort: "high", BudgetTokens: ptr(1024)}, "", true},
		{"openai off", models.OpenAI, astral.ReasoningOptions{Enabled: ptr(false)}, `"reasoning_effort":"none"`, false},
		{"openai on conflict", models.OpenAI, astral.ReasoningOptions{Enabled: ptr(true), Effort: "none"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config()
			cfg.Dialect = tc.dialect
			cfg.Reasoning = &models.ReasoningCapabilities{Toggle: ptr(true), Disable: ptr(true), OffEffort: "none"}
			switch tc.dialect {
			case models.Qwen:
				cfg.ReasoningFormat = models.ReasoningFormatQwen
				cfg.Reasoning.Budget = ptr(true)
				cfg.Reasoning.Efforts = []string{}
			case models.DeepSeek:
				cfg.ReasoningFormat = models.ReasoningFormatThinkingType
				cfg.Reasoning.Efforts = []string{"max"}
			case models.OpenRouter:
				cfg.ReasoningFormat = models.ReasoningFormatOpenRouter
				cfg.Reasoning.Budget = ptr(true)
				cfg.Reasoning.Efforts = []string{"high", "xhigh"}
			}
			m, err := completions.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			req := request()
			req.Options.Reasoning = tc.options
			body, err := m.RequestBody(req)
			if tc.bad {
				if !errors.Is(err, astral.ErrInvalid) {
					t.Fatalf("expected validation error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(body)
			if !strings.Contains(string(raw), tc.want) || strings.Contains(string(raw), "extra_body") {
				t.Fatalf("wrong native request: %s", raw)
			}
			if tc.dialect == models.Qwen && body["thinking_budget"] != 0 {
				t.Fatal("explicit zero budget was lost")
			}
		})
	}
}

func TestLateEncryptedReasoningAndToolFragmentsRoundTrip(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var value json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&value)
		body = string(value)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"id":"r1","model":"remote","choices":[{"index":0,"delta":{"reasoning_details":[{"type":"reasoning.text","text":"plan","index":9007199254740993}]},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"content":"Looking up."},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"reasoning_details":[{"type":"reasoning.encrypted","id":"rs1","data":"opaque1","format":"vendor-v1"},{"type":"reasoning.encrypted","id":"rs2","data":"opaque2","format":"vendor-v1"}],"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`,
			`[DONE]`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
	}))
	defer server.Close()
	cfg := config()
	cfg.BaseURL, cfg.Dialect = server.URL, models.OpenRouter
	m, err := completions.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var events []astral.ModelEvent
	reply, err := m.Stream(context.Background(), request(), func(e astral.ModelEvent) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	if reply.Message.FinishReason != astral.FinishToolCalls || len(reply.Message.Content) != 3 {
		t.Fatalf("reply: %+v", reply)
	}
	if !reply.Usage.Available || reply.Usage.CachedInputTokens == nil || *reply.Usage.CachedInputTokens != 0 {
		t.Fatalf("zero usage: %+v", reply.Usage)
	}
	for _, event := range events {
		if event.Type == "tool_call_delta" && (*event.ContentIndex != 2 || event.ToolCallID != "call1") {
			t.Fatalf("wrong event target: %+v", event)
		}
	}
	if !strings.Contains(string(reply.Message.Content[0].Data), "9007199254740993") {
		t.Fatal("native number lost precision")
	}
	if !strings.Contains(body, `"stream_options":{"include_usage":true}`) {
		t.Fatal("usage request missing")
	}
	req := request()
	req.Messages = append(req.Messages, reply.Message, astral.Message{Role: astral.RoleTool, ToolCallID: "call1", Content: astral.Text("found")})
	replay, err := m.RequestBody(req)
	if err != nil {
		t.Fatal(err)
	}
	assistant := replay["messages"].([]any)[1].(map[string]any)
	details := assistant["reasoning_details"].([]any)
	if len(details) != 3 || details[1].(map[string]any)["data"] != "opaque1" || details[2].(map[string]any)["id"] != "rs2" {
		t.Fatalf("lost/reordered encrypted entries: %+v", details)
	}
	cfg.ReplayScope = "another-credential-revision"
	other, _ := completions.New(cfg)
	if _, err := other.RequestBody(req); !errors.Is(err, astral.ErrInvalid) {
		t.Fatal("opaque state crossed provider scope")
	}
}

func TestEmptyThinkingAndNonstreamPrecision(t *testing.T) {
	for _, dialect := range []models.Dialect{models.DeepSeek, models.OpenRouter} {
		t.Run(string(dialect), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"id":"r","model":"remote","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"reasoning_content":"","reasoning_details":[{"type":"reasoning.encrypted","data":"secret","index":9007199254740993}],"tool_calls":[{"id":"call","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}`)
			}))
			defer server.Close()
			cfg := config()
			cfg.BaseURL, cfg.Streaming, cfg.Dialect = server.URL, ptr(false), dialect
			m, _ := completions.New(cfg)
			reply, err := m.Stream(context.Background(), request(), func(astral.ModelEvent) { t.Error("fake nonstream progress") })
			if err != nil {
				t.Fatal(err)
			}
			if reply.Usage.Available || reply.Message.Content[0].Text != "" || !strings.Contains(string(reply.Message.Content[0].Data), "9007199254740993") {
				t.Fatalf("lost native state: %+v", reply)
			}
			if !strings.Contains(string(reply.Message.Content[0].Data), `"reasoning_content":""`) {
				t.Fatal("empty thinking key was dropped")
			}
		})
	}
}

func TestIncompleteAndHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       models.ErrorKind
		transient  bool
	}{
		{"truncated", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n", 200, "", true},
		{"invalid arguments", "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"function\":{\"name\":\"x\",\"arguments\":\"{\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n", 200, "", false},
		{"authentication", `{"error":{"code":"invalid_api_key","message":"bad"}}`, 401, models.KindAuthentication, false},
		{"rate limit", `{"error":{"code":"rate_limit_exceeded","message":"slow"}}`, 429, models.KindRateLimited, true},
		{"body error", "data: {\"error\":{\"code\":\"insufficient_quota\",\"message\":\"empty\"}}\n\n", 200, models.KindQuotaExhausted, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			cfg := config()
			cfg.BaseURL = server.URL
			m, _ := completions.New(cfg)
			reply, err := m.Stream(context.Background(), request(), nil)
			if err == nil || reply.Message.FinishReason != astral.FinishError {
				t.Fatalf("failure became success: %+v %v", reply, err)
			}
			if tc.want != "" {
				var api *models.APIError
				if !errors.As(err, &api) || api.Kind != tc.want {
					t.Fatalf("wrong error: %v", err)
				}
			}
			var retry *astral.ModelError
			if tc.transient && (!errors.As(err, &retry) || !retry.Transient) {
				t.Fatalf("transient cause missing: %v", err)
			}
		})
	}
}

func TestCancellationStopsEmission(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	cfg := config()
	cfg.BaseURL = server.URL
	m, _ := completions.New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	emissions := 0
	reply, err := m.Stream(ctx, request(), func(e astral.ModelEvent) {
		emissions++
		if e.Type == "text_delta" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || reply.Message.FinishReason != astral.FinishError || emissions != 2 {
		t.Fatalf("cancel: %+v %v events=%d", reply, err, emissions)
	}
}
