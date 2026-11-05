package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models/provider"
	"github.com/oines/astral-core/models/provider/anthropic"
	"github.com/oines/astral-core/models/provider/google"
	"github.com/oines/astral-core/models/provider/openai/completions"
	"github.com/oines/astral-core/models/provider/openai/responses"
)

const omittedImage = "(image omitted: model does not support images)"
const omittedToolImage = "(tool image omitted: model does not support images)"

func TestNonVisionProvidersUseOneProjectionForEveryRequestPath(t *testing.T) {
	for protocol, response := range map[string]string{
		completions.Protocol: `{"id":"ok","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
		responses.Protocol:   `{"id":"ok","status":"completed","output":[{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`,
		anthropic.Protocol:   `{"id":"ok","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`,
		google.Protocol:      `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`,
	} {
		t.Run(protocol, func(t *testing.T) {
			cfg := config("target", "scope")
			cfg.Images = "unsupported"
			var received [][]byte
			cfg.HTTPClient = &http.Client{Transport: executionTransport(func(request *http.Request) (*http.Response, error) {
				raw, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				received = append(received, raw)
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(response)), Request: request}, nil
			})}
			model, err := provider.New(protocol, cfg)
			if err != nil {
				t.Fatal(err)
			}
			image := core.Content{Type: "image", MediaType: "image/png", Data: json.RawMessage(`"eA=="`), URL: "host-file://original"}
			r := requestFor(model, []core.Message{
				{Role: core.RoleUser, Content: []core.Content{image, image}},
				{Role: core.RoleAssistant, Content: []core.Content{{Type: "tool_call", ToolCall: &core.ToolCall{ID: "read", Name: "read", Arguments: json.RawMessage(`{"path":"image.png"}`)}}}},
				{Role: core.RoleTool, ToolCallID: "read", Content: []core.Content{image, image}},
				{Role: core.RoleAssistant, Content: []core.Content{image}},
			})
			before, _ := json.Marshal(r)
			body, err := model.RequestBody(r)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(body)
			if strings.Count(string(want), omittedImage) != 2 || strings.Count(string(want), omittedToolImage) != 1 || bytes.Contains(want, []byte("eA==")) || bytes.Contains(want, []byte("host-file://")) {
				t.Fatalf("missing, duplicate or leaked image blocks: %s", want)
			}
			if err = model.ValidateRequest(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			prepared, err := model.PrepareRequest(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			count, err := model.EstimateTokens(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			preparedCount, err := model.EstimateTokens(context.Background(), prepared)
			if err != nil || count != preparedCount {
				t.Fatalf("budget differs from prepared content: %d != %d (%v)", count, preparedCount, err)
			}
			execution, err := model.(core.RequestExecutionPreparer).PrepareExecution(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = model.Stream(context.Background(), r, func(core.ModelEvent) {}); err != nil {
				t.Fatal("direct Stream:", err)
			}
			if _, err = provider.Call(context.Background(), model, r, nil); err != nil {
				t.Fatal("direct Call:", err)
			}
			if _, err = execution.Stream(context.Background(), func(core.ModelEvent) {}); err != nil {
				t.Fatal("frozen execution:", err)
			}
			for _, raw := range received {
				if !bytes.Equal(raw, want) {
					t.Fatalf("request paths disagree: got %s want %s", raw, want)
				}
			}
			after, _ := json.Marshal(r)
			if len(received) != 3 || !bytes.Equal(before, after) {
				t.Fatal("projection mutated source history or changed invocation count")
			}
		})
	}
}

func TestNonVisionDirectBodyKeepsForeignNativeReplayGuard(t *testing.T) {
	for protocol, field := range map[string]string{completions.Protocol: "chat.reasoning", responses.Protocol: "responses.item", anthropic.Protocol: "anthropic.block", google.Protocol: "google.part"} {
		t.Run(protocol, func(t *testing.T) {
			cfg := config("target", "scope")
			cfg.Images = "unsupported"
			model, err := provider.New(protocol, cfg)
			if err != nil {
				t.Fatal(err)
			}
			r := requestFor(model, []core.Message{
				{Role: core.RoleUser, Content: []core.Content{{Type: "image", Data: json.RawMessage(`"eA=="`), MediaType: "image/png"}}},
				{Role: core.RoleAssistant, Content: []core.Content{{Type: "reasoning", Text: "private", Data: json.RawMessage(`{"opaque":"foreign-cipher"}`), Metadata: origin(protocol, "foreign-scope", cfg.Model, field)}, {Type: "text", Text: "portable"}}},
			})
			if _, err := model.RequestBody(r); err == nil {
				t.Fatal("image downgrade bypassed the foreign native replay guard")
			}
			prepared, err := model.PrepareRequest(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			body, err := model.RequestBody(prepared)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(body)
			if bytes.Contains(raw, []byte("foreign-cipher")) || !bytes.Contains(raw, []byte(omittedImage)) || !bytes.Contains(raw, []byte("portable")) {
				t.Fatalf("prepared native/image projection: %s", raw)
			}
		})
	}
}

func TestSwitchingImageCapabilityKeepsHistoryAndExistingFrozenExecution(t *testing.T) {
	cfg := config("same-route", "scope")
	cfg.Images = "supported"
	var received [][]byte
	cfg.HTTPClient = &http.Client{Transport: executionTransport(func(request *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		received = append(received, raw)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)), Request: request}, nil
	})}
	visual, err := provider.New(completions.Protocol, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := requestFor(visual, []core.Message{{Role: core.RoleUser, Content: []core.Content{{Type: "image", Data: json.RawMessage(`"eA=="`), MediaType: "image/png"}}}})
	execution, err := visual.(core.RequestExecutionPreparer).PrepareExecution(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Images = "unsupported"
	text, err := provider.New(completions.Protocol, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Call(context.Background(), text, r, nil); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := execution.Stream(context.Background(), func(core.ModelEvent) {}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := provider.Call(context.Background(), visual, r, nil); err != nil {
		t.Fatal(err)
	}
	if len(received) != 4 || !bytes.Contains(received[0], []byte(omittedImage)) || bytes.Contains(received[0], []byte("eA==")) || !bytes.Equal(received[1], received[2]) || !bytes.Equal(received[1], received[3]) || !bytes.Contains(received[1], []byte("data:image/png;base64,eA==")) {
		t.Fatal("switching to text mutated source pixels, visual defaults or frozen retry bytes")
	}
}
