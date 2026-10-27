package tests_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	core "github.com/oines/astral-core"
	models "github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/provider/anthropic"
	"github.com/oines/astral-core/models/provider/google"
	"github.com/oines/astral-core/models/provider/openai/completions"
	"github.com/oines/astral-core/models/provider/openai/responses"
)

type requestEncoder interface {
	RequestBody(core.Request) (map[string]any, error)
}

func TestEnvironmentContextPreservesNativeRequestPrefix(t *testing.T) {
	for _, protocol := range []string{"completions", "responses", "anthropic", "google"} {
		t.Run(protocol, func(t *testing.T) {
			cfg := models.Config{Spec: core.ModelSpec{ID: "profile", ContextWindow: 128000, MaxOutputTokens: 4096}, Model: "remote"}
			var adapter requestEncoder
			var err error
			switch protocol {
			case "completions":
				adapter, err = completions.New(cfg)
			case "responses":
				adapter, err = responses.New(cfg)
			case "anthropic":
				adapter, err = anthropic.New(cfg)
			case "google":
				adapter, err = google.New(cfg)
			}
			if err != nil {
				t.Fatal(err)
			}
			snapshot := func(text string) core.Message {
				return core.Message{Role: core.RoleUser, ContextKind: "astral.environment", Metadata: json.RawMessage(`{"fingerprint":"host-owned"}`), Content: core.Text(text)}
			}
			req := core.Request{ModelID: "profile", SystemPrompt: "Fixed host instructions.", MaxOutputTokens: 1024, Messages: []core.Message{
				snapshot("environment-alpha"), core.User("perform two tools"),
				{Role: core.RoleAssistant, Content: []core.Content{
					{Type: "tool_call", ToolCall: &core.ToolCall{ID: "one", Name: "lookup", Arguments: json.RawMessage(`{}`)}},
					{Type: "tool_call", ToolCall: &core.ToolCall{ID: "two", Name: "lookup", Arguments: json.RawMessage(`{}`)}},
				}},
				{Role: core.RoleTool, Name: "lookup", ToolCallID: "one", Content: core.Text("result-one")},
				{Role: core.RoleTool, Name: "lookup", ToolCallID: "two", Content: core.Text("result-two")},
			}}
			before, err := adapter.RequestBody(req)
			if err != nil {
				t.Fatal(err)
			}
			prefix, systemBefore := nativeRequestParts(protocol, before)
			req.Messages = append(req.Messages, snapshot("environment-beta"))
			after, err := adapter.RequestBody(req)
			if err != nil {
				t.Fatal(err)
			}
			parts, systemAfter := nativeRequestParts(protocol, after)
			if !reflect.DeepEqual(systemBefore, systemAfter) || strings.Contains(string(systemAfter), "environment-") {
				t.Fatalf("snapshot changed native system instructions: %s", systemAfter)
			}
			// Messages/Gemini combine neighboring user turns. Compare the ordered
			// blocks inside those turns, where cacheable model input is preserved.
			if len(parts) != len(prefix)+1 || !reflect.DeepEqual(prefix, parts[:len(prefix)]) {
				t.Fatal("appending context rewrote or reordered the native content prefix")
			}
			last := string(parts[len(parts)-1])
			if !strings.Contains(last, `"role":"user"`) || !strings.Contains(last, "environment-beta") || strings.Contains(last, "host-owned") {
				t.Fatalf("snapshot was not trailing user context: %s", last)
			}
			raw, _ := json.Marshal(after)
			if strings.Count(string(raw), "environment-alpha") != 1 || strings.Count(string(raw), "environment-beta") != 1 {
				t.Fatal("native request duplicated an environment snapshot")
			}
			if strings.Index(string(raw), "result-two") > strings.Index(string(raw), "environment-beta") {
				t.Fatal("snapshot moved ahead of the completed tool-result batch")
			}
		})
	}
}

func nativeRequestParts(protocol string, body map[string]any) ([]json.RawMessage, json.RawMessage) {
	var system any
	key, contentKey := "messages", ""
	switch protocol {
	case "responses":
		key, system = "input", body["instructions"]
	case "anthropic":
		contentKey, system = "content", body["system"]
	case "google":
		key, contentKey, system = "contents", "parts", body["systemInstruction"]
	}
	var parts []json.RawMessage
	for _, value := range body[key].([]any) {
		message := value.(map[string]any)
		if message["role"] == "system" {
			system = message
			continue
		}
		if contentKey == "" {
			raw, _ := json.Marshal(message)
			parts = append(parts, raw)
			continue
		}
		for _, part := range message[contentKey].([]any) {
			raw, _ := json.Marshal(map[string]any{"role": message["role"], "block": part})
			parts = append(parts, raw)
		}
	}
	raw, _ := json.Marshal(system)
	return parts, raw
}
