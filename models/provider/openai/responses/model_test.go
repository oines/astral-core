package responses_test

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

	astral "github.com/oines/astral-core"
	models "github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/provider/openai/responses"
)

func config() models.Config {
	return models.Config{Spec: astral.ModelSpec{ID: "profile:1", Revision: "1", ContextWindow: 100000, MaxOutputTokens: 8192}, Model: "remote", ReplayScope: "connection:1", Reasoning: &models.ReasoningCapabilities{Efforts: []string{"max"}}}
}
func request() astral.Request {
	return astral.Request{ModelID: "profile:1", MaxOutputTokens: 1000, Messages: []astral.Message{astral.User("go")}}
}

var ordered = []json.RawMessage{
	json.RawMessage(`{"type":"reasoning","id":"r1","summary":[],"encrypted_content":"opaque-1","vendor_extra":{"counter":9007199254740993}}`),
	json.RawMessage(`{"type":"function_call","id":"fc1","call_id":"c1","name":"lookup","arguments":"{\"q\":\"one\"}","status":"completed"}`),
	json.RawMessage(`{"type":"reasoning","id":"r2","summary":[{"type":"summary_text","text":"Then"}],"encrypted_content":"opaque-2"}`),
	json.RawMessage(`{"type":"function_call","id":"fc2","call_id":"c2","name":"lookup","arguments":"{\"q\":\"two\"}","status":"completed"}`),
	json.RawMessage(`{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Looking.","annotations":[{"type":"vendor_annotation","opaque":true}]}]}`),
}

func TestOrderedNativeItemsSurviveRoundTrip(t *testing.T) {
	for _, streaming := range []bool{true, false} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					for i, raw := range ordered {
						add := raw
						if i == 0 {
							add = json.RawMessage(`{"type":"reasoning","id":"r1","summary":[],"encrypted_content":""}`)
						}
						fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":%d,\"item\":%s}\n\n", i, add)
						if i == 1 {
							fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":1,\"item_id\":\"fc1\",\"delta\":\"{}\"}\n\n")
						}
						if i == 4 {
							fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":4,\"item_id\":\"m\",\"delta\":\"Looking.\"}\n\n")
						}
						fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":%d,\"item\":%s}\n\n", i, raw)
					}
				}
				out, _ := json.Marshal(ordered)
				result := fmt.Sprintf(`{"id":"resp","model":"remote","status":"completed","output":%s,"usage":{"input_tokens":5,"output_tokens":3,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":2}}}`, out)
				if streaming {
					fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", result)
				} else {
					fmt.Fprint(w, result)
				}
			}))
			defer server.Close()
			cfg := config()
			cfg.BaseURL, cfg.Streaming = server.URL, &streaming
			m, err := responses.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var events []astral.ModelEvent
			reply, err := m.Stream(context.Background(), request(), func(e astral.ModelEvent) { events = append(events, e) })
			if err != nil {
				t.Fatal(err)
			}
			if !streaming && len(events) != 0 {
				t.Fatal("fake nonstream progress")
			}
			if reply.Message.FinishReason != astral.FinishToolCalls || len(reply.Message.Content) != len(ordered) {
				t.Fatalf("reply: %+v", reply)
			}
			if !reply.Usage.Available || reply.Usage.ReasoningTokens == nil || *reply.Usage.ReasoningTokens != 2 {
				t.Fatal("usage missing")
			}
			for i, part := range reply.Message.Content {
				if string(part.Data) != string(ordered[i]) {
					t.Fatalf("native item %d changed: %s", i, part.Data)
				}
			}
			for _, e := range events {
				if e.Type == "text_delta" && (*e.ContentIndex != 4 || reply.Message.Content[*e.ContentIndex].Type != "text") {
					t.Fatal("text event index drift")
				}
				if e.Type == "tool_call_delta" && (*e.ContentIndex != 1 || e.ToolCallID != "c1") {
					t.Fatal("call event index drift")
				}
			}
			req := request()
			req.Messages = append(req.Messages, reply.Message,
				astral.Message{Role: astral.RoleTool, ToolCallID: "c1", Content: astral.Text("1")},
				astral.Message{Role: astral.RoleTool, ToolCallID: "c2", Content: []astral.Content{{Type: "image", URL: "data:image/png;base64,eA=="}}})
			body, err := m.RequestBody(req)
			if err != nil {
				t.Fatal(err)
			}
			input := body["input"].([]any)
			for i, want := range ordered {
				if !reflect.DeepEqual(input[i+1], want) {
					t.Fatalf("item %d regrouped or changed: %s", i, input[i+1])
				}
			}
			if body["store"] != false {
				t.Fatal("provider history persistence was enabled")
			}
			raw, _ := json.Marshal(body)
			if !strings.Contains(string(raw), `"type":"input_image"`) || !strings.Contains(string(raw), "opaque-1") {
				t.Fatal("lost multimodal tool output or ciphertext")
			}
			cfg.ReplayScope = "other"
			other, _ := responses.New(cfg)
			if _, err := other.RequestBody(req); !errors.Is(err, astral.ErrInvalid) {
				t.Fatal("opaque state crossed route")
			}
		})
	}
}

func TestNativeFieldsAndUnsupportedOptions(t *testing.T) {
	m, _ := responses.New(config())
	req := request()
	req.Options.Reasoning.Effort = "max"
	req.Options.ProviderOptions = map[string]json.RawMessage{responses.Protocol: json.RawMessage(`{"reasoning":{"summary":"auto"},"text":{"verbosity":"low"}}`)}
	body, err := m.RequestBody(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), `"reasoning":{"effort":"max","summary":"auto"}`) {
		t.Fatalf("native extension or effort changed: %s", raw)
	}
	req.Options.ProviderOptions[responses.Protocol] = json.RawMessage(`{"reasoning":{"effort":"xhigh"}}`)
	if _, err := m.RequestBody(req); !errors.Is(err, astral.ErrInvalid) {
		t.Fatal("extension overrode selected effort")
	}
	req.Options.ProviderOptions = nil
	req.Options.Stop = []string{"stop"}
	if _, err := m.RequestBody(req); !errors.Is(err, astral.ErrInvalid) {
		t.Fatal("unsupported option silently disappeared")
	}
}

func TestResponsesTerminalFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		transient  bool
		kind       models.ErrorKind
	}{
		{"eof", "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"m\",\"role\":\"assistant\",\"content\":[]}}\n\n", true, ""},
		{"failed", "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"busy\"}}}\n\n", true, models.KindServerError},
		{"error", "data: {\"type\":\"error\",\"code\":\"invalid_api_key\",\"message\":\"bad\"}\n\n", false, models.KindAuthentication},
		{"invalid arguments", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"c\",\"name\":\"x\",\"arguments\":\"{\"}]}}\n\n", false, ""},
		{"nonterminal", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"in_progress\",\"output\":[]}}\n\n", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }))
			defer server.Close()
			cfg := config()
			cfg.BaseURL = server.URL
			m, _ := responses.New(cfg)
			reply, err := m.Stream(context.Background(), request(), nil)
			if err == nil || reply.Message.FinishReason != astral.FinishError || len(reply.Message.ToolCalls()) != 0 {
				t.Fatalf("invalid stream executed: %+v %v", reply, err)
			}
			var retry *astral.ModelError
			if tc.transient && (!errors.As(err, &retry) || !retry.Transient) {
				t.Fatalf("lost transient classification: %v", err)
			}
			if tc.kind != "" {
				var api *models.APIError
				if !errors.As(err, &api) || api.Kind != tc.kind {
					t.Fatalf("lost API error: %v", err)
				}
			}
		})
	}
}

func TestEncryptedReasoningRecoveryKeepsToolResultsAndPlainHistory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		output, _ := json.Marshal(ordered)
		fmt.Fprintf(w, `{"id":"resp","status":"completed","output":%s}`, output)
	}))
	defer server.Close()
	cfg := config()
	streaming := false
	cfg.Streaming = &streaming
	cfg.BaseURL = server.URL
	m, err := responses.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	req := request()
	reply, err := m.Stream(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Messages = append(req.Messages, reply.Message, astral.Message{Role: astral.RoleTool, ToolCallID: "c1", Content: astral.Text("one")}, astral.Message{Role: astral.RoleTool, ToolCallID: "c2", Content: astral.Text("two")})
	before, _ := json.Marshal(req)
	repaired, changed, err := m.RecoverRequest(context.Background(), req, &models.APIError{StatusCode: 400, Message: "The encrypted content could not be verified."})
	if err != nil || !changed {
		t.Fatalf("expected recovery: %v %v", changed, err)
	}
	body, err := m.RequestBody(repaired)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "opaque-") || !strings.Contains(string(raw), "Looking.") || !strings.Contains(string(raw), `"output":"one"`) || !strings.Contains(string(raw), `"output":"two"`) {
		t.Fatalf("incorrect recovery: %s", raw)
	}
	if len(repaired.Messages[1].ToolCalls()) != 2 {
		t.Fatal("tool calls were dropped")
	}
	after, _ := json.Marshal(req)
	if string(before) != string(after) {
		t.Fatal("original history was mutated")
	}
	for _, cause := range []error{context.DeadlineExceeded, &models.APIError{StatusCode: 401, Message: "encrypted content could not be verified"}, &models.APIError{StatusCode: 400, Message: "invalid JSON"}, &models.APIError{StatusCode: 500, Code: "invalid_encrypted_content"}} {
		if _, changed, err := m.RecoverRequest(context.Background(), req, cause); err != nil || changed {
			t.Fatalf("unrelated error discarded state: %v", cause)
		}
	}
}
