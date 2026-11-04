package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
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

type executionTransport func(*http.Request) (*http.Response, error)

func (f executionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestPreparedExecutionOwnsOneExactBodyAcrossAttempts(t *testing.T) {
	for protocol, response := range map[string]string{
		completions.Protocol: `{"id":"ok","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
		responses.Protocol:   `{"id":"ok","status":"completed","output":[{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`,
		anthropic.Protocol:   `{"id":"ok","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`,
		google.Protocol:      `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`,
	} {
		t.Run(protocol, func(t *testing.T) {
			cfg := config("target", "scope")
			var received [][]byte
			cfg.HTTPClient = &http.Client{Transport: executionTransport(func(request *http.Request) (*http.Response, error) {
				raw, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				if request.Header.Get("X-Attempt") != strconv.Itoa(len(received)+1) {
					t.Error("attempt headers were frozen with the body")
				}
				received = append(received, raw)
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(response)), Request: request}, nil
			})}
			model, err := provider.New(protocol, cfg)
			if err != nil {
				t.Fatal(err)
			}
			request := requestFor(model, []core.Message{{Role: core.RoleUser, Content: []core.Content{{Type: "text", Text: "original"}, {Type: "image", MediaType: "image/png", Data: json.RawMessage(`"eA=="`)}}}})
			request.Tools = []core.ToolDefinition{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}
			body, err := model.RequestBody(request)
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			if err := model.ValidateRequest(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			execution, err := model.(core.RequestExecutionPreparer).PrepareExecution(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			// The execution must not retain either the request's RawJSON or the
			// mutable map returned by public RequestBody.
			request.Messages[0].Content[0].Text = "changed"
			request.Messages[0].Content[1].Data[1] = 'Z'
			request.Tools[0].Parameters[2] = 'X'
			body["model"] = "changed"
			for attempt := range 2 {
				ctx := models.WithRequestHeaders(context.Background(), map[string]string{"X-Attempt": strconv.Itoa(attempt + 1)})
				reply, err := execution.Stream(ctx, func(core.ModelEvent) {})
				if err != nil || reply.Message.Role != core.RoleAssistant {
					t.Fatalf("prepared attempt: %+v %v", reply, err)
				}
			}
			if len(received) != 2 || !bytes.Equal(received[0], want) || !bytes.Equal(received[1], want) {
				t.Fatal("prepared bytes differ from public wire encoding or between attempts")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := execution.Stream(ctx, nil); err != context.Canceled || len(received) != 2 {
				t.Fatalf("canceled execution reached transport: %v", err)
			}
		})
	}
}

func TestPublicRequestBodyDoesNotBorrowMutableSchema(t *testing.T) {
	for _, protocol := range []string{completions.Protocol, responses.Protocol, anthropic.Protocol, google.Protocol} {
		t.Run(protocol, func(t *testing.T) {
			model, err := provider.New(protocol, config("target", "scope"))
			if err != nil {
				t.Fatal(err)
			}
			request := requestFor(model, []core.Message{core.User("original")})
			request.Tools = []core.ToolDefinition{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}
			body, err := model.RequestBody(request)
			if err != nil {
				t.Fatal(err)
			}
			before, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			request.Tools[0].Parameters[2] = 'X'
			after, err := json.Marshal(body)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("public body shares caller-owned schema bytes")
			}
		})
	}
}

func BenchmarkSDKImageEstimate(b *testing.B) {
	model, err := provider.New(responses.Protocol, config("target", "scope"))
	if err != nil {
		b.Fatal(err)
	}
	request := requestFor(model, []core.Message{{Role: core.RoleUser, Content: []core.Content{{Type: "image", MediaType: "image/png", Data: json.RawMessage(`"` + strings.Repeat("eHh4", 1<<18) + `"`)}}}})
	b.ReportAllocs()
	b.SetBytes(int64(len(request.Messages[0].Content[0].Data)))
	b.ResetTimer()
	for range b.N {
		if _, err := model.EstimateTokens(context.Background(), request); err != nil {
			b.Fatal(err)
		}
	}
}

func TestEveryProviderForwardsReadonlyProjection(t *testing.T) {
	for _, protocol := range []string{completions.Protocol, responses.Protocol, anthropic.Protocol, google.Protocol} {
		t.Run(protocol, func(t *testing.T) {
			model, err := provider.New(protocol, config("target", "scope"))
			if err != nil {
				t.Fatal(err)
			}
			request := requestFor(model, []core.Message{{Role: core.RoleUser, Content: []core.Content{{Type: "image", MediaType: "image/png", Data: json.RawMessage(`"eA=="`)}}}})
			projector, ok := model.(interface {
				ProjectRequest(context.Context, core.Request) (core.Request, error)
			})
			if !ok {
				t.Fatal("provider hides readonly projection")
			}
			projected, err := projector.ProjectRequest(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if &projected.Messages[0].Content[0].Data[0] != &request.Messages[0].Content[0].Data[0] {
				t.Fatal("provider forwarding copied immutable pixels")
			}
			owned, err := model.PrepareRequest(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			owned.Messages[0].Content[0].Data[1] = 'Z'
			if string(request.Messages[0].Content[0].Data) != `"eA=="` {
				t.Fatal("public SDK preparation lost isolation")
			}
		})
	}
}
