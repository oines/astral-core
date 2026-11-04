package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models/provider"
	"github.com/oines/astral-core/models/provider/anthropic"
	"github.com/oines/astral-core/models/provider/google"
	"github.com/oines/astral-core/models/provider/openai/completions"
	"github.com/oines/astral-core/models/provider/openai/responses"
)

func TestDeferredExecutionMatchesPublicBodyForEveryProtocol(t *testing.T) {
	plain := strings.Repeat("eHh4", 512)
	quoted := func(s string) json.RawMessage { raw, _ := json.Marshal(s); return raw }
	for protocol, native := range map[string]struct{ field, data string }{
		completions.Protocol: {"chat.content", `{"content":[{"type":"text","text":"native <>&"}]}`},
		responses.Protocol:   {"responses.item", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"native <>&"}]}`},
		anthropic.Protocol:   {"anthropic.block", `{"type":"text","text":"native <>&"}`},
		google.Protocol:      {"google.part", `{"text":"native <>&"}`},
	} {
		t.Run(protocol, func(t *testing.T) {
			for name, image := range map[string]core.Content{
				"plain":             {Type: "image", MediaType: "image/png", Data: quoted(plain)},
				"marker collision":  {Type: "image", MediaType: "image/png", Data: quoted(plain)},
				"padding bits":      {Type: "image", MediaType: "image/jpeg", Data: quoted(plain + "AB==")},
				"whitespace":        {Type: "image", MediaType: "image/svg+xml", Data: json.RawMessage(" \n\t" + string(quoted(plain)) + "\n ")},
				"escaped":           {Type: "image", MediaType: "image/png", Data: json.RawMessage(`"\u0065` + plain[1:] + `"`)},
				"CRLF":              {Type: "image", MediaType: "image/png", Data: quoted(plain + "\r\n")},
				"small":             {Type: "image", MediaType: "image/png", Data: quoted("eA==")},
				"HTML MIME":         {Type: "image", MediaType: "image/pn<>&g", Data: quoted(plain)},
				"quote MIME":        {Type: "image", MediaType: "image/pn\"g", Data: quoted(plain)},
				"control MIME":      {Type: "image", MediaType: "image/pn\ng", Data: quoted(plain)},
				"UTF8 MIME":         {Type: "image", MediaType: "image/图\u2028\xff", Data: quoted(plain)},
				"valid UTF8 MIME":   {Type: "image", MediaType: "image/图\u2028", Data: quoted(plain)},
				"invalid UTF8 text": {Type: "image", MediaType: "image/png", Data: quoted(plain)},
				"separator MIME":    {Type: "image", MediaType: "image/png;base64,extra", Data: quoted(plain)},
				"data URL":          {Type: "image", URL: "data:image/png;base64," + plain},
				"HTTP URL":          {Type: "image", URL: "https://example.invalid/image.png"},
				"invalid base64":    {Type: "image", MediaType: "image/png", Data: quoted("invalid!")},
			} {
				t.Run(name, func(t *testing.T) {
					cfg := config("target", "scope")
					var received []byte
					cfg.HTTPClient = &http.Client{Transport: executionTransport(func(request *http.Request) (*http.Response, error) {
						var err error
						received, err = io.ReadAll(request.Body)
						if err != nil {
							return nil, err
						}
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: request}, nil
					})}
					model, err := provider.New(protocol, cfg)
					if err != nil {
						t.Fatal(err)
					}
					toolText := "tool result"
					if name == "marker collision" {
						toolText = "\x00astral-image-0\x00"
					}
					inputText := "<>&中文\u2028"
					if name == "invalid UTF8 text" {
						inputText += "\xff"
					}
					request := requestFor(model, []core.Message{
						{Role: core.RoleUser, Content: []core.Content{{Type: "text", Text: inputText}, image}},
						{Role: core.RoleAssistant, Content: []core.Content{{Type: "text", Text: "native <>&", Data: json.RawMessage(native.data), Metadata: origin(protocol, "scope", "remote-target", native.field)}}},
						{Role: core.RoleAssistant, Content: []core.Content{{Type: "tool_call", ToolCall: &core.ToolCall{ID: "call", Name: "lookup", Arguments: json.RawMessage(`{}`)}}}, FinishReason: core.FinishToolCalls},
						{Role: core.RoleTool, ToolCallID: "call", Content: []core.Content{{Type: "text", Text: toolText}, image}},
					})
					request.Tools = []core.ToolDefinition{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}
					before, _ := json.Marshal(request)
					body, publicErr := model.RequestBody(request)
					execution, preparedErr := model.(core.RequestExecutionPreparer).PrepareExecution(context.Background(), request)
					if (publicErr == nil) != (preparedErr == nil) || publicErr != nil && (reflect.TypeOf(publicErr) != reflect.TypeOf(preparedErr) || publicErr.Error() != preparedErr.Error()) {
						t.Fatalf("execution changed the public protocol error: old=%v new=%v", publicErr, preparedErr)
					}
					if preparedErr == nil {
						want, err := json.Marshal(body)
						if err != nil {
							t.Fatal(err)
						}
						if name == "UTF8 MIME" || name == "invalid UTF8 text" {
							// Public RequestBody normalizes malformed UTF-8 in its
							// ownership copy; legacy Stream marshals it directly.
							// Preserve the latter's existing execution bytes.
							_, _ = model.Stream(context.Background(), request, nil)
							want = bytes.Clone(received)
							if len(want) == 0 {
								t.Fatal("legacy UTF8 oracle did not reach HTTP")
							}
						}
						// This oracle only needs the actual body; {} intentionally
						// does not fabricate a successful provider response.
						_, _ = execution.Stream(context.Background(), nil)
						if !bytes.Equal(received, want) {
							i := 0
							for i < min(len(received), len(want)) && received[i] == want[i] {
								i++
							}
							t.Fatalf("prepared HTTP body differs from public JSON bytes at %d (%d/%d): got=%q want=%q", i, len(received), len(want), received[i:min(i+70, len(received))], want[i:min(i+70, len(want))])
						}
					}
					after, _ := json.Marshal(request)
					if !bytes.Equal(before, after) {
						t.Fatal("private execution projection mutated its source request")
					}
				})
			}
		})
	}
}
