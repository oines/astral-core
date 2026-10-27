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

func TestResponsesCompatibilityDefaultsAndExplicitExtensions(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name           string
		compatibility  *models.ResponsesCompatibility
		include, store bool
	}{
		{"standard", nil, true, true},
		{"unknown", &models.ResponsesCompatibility{}, true, true},
		{"supported", &models.ResponsesCompatibility{SupportsInclude: &yes, SupportsStore: &yes}, true, true},
		{"omit include", &models.ResponsesCompatibility{SupportsInclude: &no}, false, true},
		{"omit store", &models.ResponsesCompatibility{SupportsStore: &no}, true, false},
		{"omit both", &models.ResponsesCompatibility{SupportsInclude: &no, SupportsStore: &no}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config()
			cfg.ResponsesCompatibility = tc.compatibility
			m, err := responses.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			body, err := m.RequestBody(request())
			if err != nil {
				t.Fatal(err)
			}
			if got, present := body["include"]; present != tc.include || (present && !reflect.DeepEqual(got, []string{"reasoning.encrypted_content"})) {
				t.Fatalf("include = %#v, present %v", got, present)
			}
			if got, present := body["store"]; present != tc.store || (present && got != false) {
				t.Fatalf("store = %#v, present %v", got, present)
			}
			for key, supported := range map[string]bool{"include": tc.include, "store": tc.store} {
				values := []string{"null"}
				if key == "store" {
					values = append(values, "false", "true")
				} else {
					values = append(values, "[]", `["reasoning.encrypted_content"]`)
				}
				if supported {
					continue
				}
				for _, value := range values {
					req := request()
					req.Options.ProviderOptions = map[string]json.RawMessage{responses.Protocol: json.RawMessage(fmt.Sprintf(`{%q:%s}`, key, value))}
					if _, err := m.RequestBody(req); !errors.Is(err, astral.ErrInvalid) || !strings.Contains(err.Error(), "unsupported") {
						t.Fatalf("explicit %s=%s should fail: %v", key, value, err)
					}
				}
			}
			req := request()
			req.Options.ProviderOptions = map[string]json.RawMessage{responses.Protocol: json.RawMessage(`{"metadata":{"request":"safe"},"reasoning":{"summary":"auto"}}`)}
			if _, err := m.RequestBody(req); err != nil {
				t.Fatalf("unrelated native options rejected: %v", err)
			}
		})
	}
}

func TestResponsesCompatibilityIsImmutable(t *testing.T) {
	encrypted, store := false, false
	cfg := config()
	cfg.ResponsesCompatibility = &models.ResponsesCompatibility{SupportsEncryptedReasoningInclude: &encrypted, SupportsStore: &store}
	m, err := responses.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, store = true, true
	cfg.ResponsesCompatibility.SupportsStore = nil
	body, err := m.RequestBody(request())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"include", "store"} {
		if _, present := body[key]; present {
			t.Fatalf("changing caller config restored %s", key)
		}
	}
}

func TestResponsesCompatibilityCanOmitOnlyEncryptedReasoningInclude(t *testing.T) {
	no := false
	cfg := config()
	cfg.ResponsesCompatibility = &models.ResponsesCompatibility{SupportsEncryptedReasoningInclude: &no}
	m, err := responses.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"include":["reasoning.encrypted_content"]}`, `{"include":["web_search_call.results","reasoning.encrypted_content"]}`} {
		req := request()
		req.Options.ProviderOptions = map[string]json.RawMessage{responses.Protocol: json.RawMessage(raw)}
		if _, err := m.RequestBody(req); !errors.Is(err, astral.ErrInvalid) || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("unsupported encrypted include accepted: %s, %v", raw, err)
		}
	}
	req := request()
	body, err := m.RequestBody(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := body["include"]; present || body["store"] != false {
		t.Fatalf("incorrect automatic defaults: %v", body)
	}
	req.Options.ProviderOptions = map[string]json.RawMessage{responses.Protocol: json.RawMessage(`{"include":["web_search_call.results","web_search_call.action.sources"]}`)}
	body, err = m.RequestBody(req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(body["include"], []any{"web_search_call.results", "web_search_call.action.sources"}) {
		t.Fatalf("other native include values lost: %v", body["include"])
	}
}

func TestResponsesCompatibilityPreservesEncryptedReplay(t *testing.T) {
	for _, streaming := range []bool{true, false} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				for _, key := range []string{"include", "store"} {
					if _, present := body[key]; present {
						t.Errorf("unsupported %s sent", key)
					}
				}
				output, _ := json.Marshal(ordered)
				result := fmt.Sprintf(`{"id":"resp","status":"completed","output":%s}`, output)
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", result)
				} else {
					fmt.Fprint(w, result)
				}
			}))
			defer server.Close()
			no := false
			cfg := config()
			cfg.BaseURL, cfg.Streaming = server.URL, &streaming
			cfg.ResponsesCompatibility = &models.ResponsesCompatibility{SupportsEncryptedReasoningInclude: &no, SupportsStore: &no}
			m, err := responses.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			reply, err := m.Stream(context.Background(), request(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(reply.Message.Content) != len(ordered) {
				t.Fatalf("native items lost: %+v", reply.Message.Content)
			}
			for i, part := range reply.Message.Content {
				if string(part.Data) != string(ordered[i]) {
					t.Fatalf("native item %d changed: %s", i, part.Data)
				}
			}
			req := request()
			req.Messages = append(req.Messages, reply.Message)
			body, err := m.RequestBody(req)
			if err != nil {
				t.Fatal(err)
			}
			input := body["input"].([]any)
			for i, raw := range ordered {
				if !reflect.DeepEqual(input[i+1], raw) {
					t.Fatalf("replayed item %d changed: %s", i, input[i+1])
				}
			}
		})
	}
}
