package astral_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	astral "github.com/oines/astral-core"
)

func messageFixture() astral.Message {
	opaque := json.RawMessage(`{"signature":"proof","counter":900719925474099312345,"tool_calls":"opaque"}`)
	return astral.Message{Role: astral.RoleAssistant, Metadata: opaque, Content: []astral.Content{
		{Type: "reasoning", Text: "first reasoning", Data: json.RawMessage(`{"encrypted":"AA=="}`), Metadata: opaque},
		{Type: "text", Text: "first text", Metadata: opaque},
		{Type: "tool_call", ToolCall: &astral.ToolCall{ID: "one", Name: "lookup", Arguments: json.RawMessage(`{"value":1}`), Metadata: opaque}, Metadata: opaque},
		{Type: "reasoning", Text: "second reasoning", Metadata: opaque},
		{Type: "text", Text: "second text"},
		{Type: "tool_call", ToolCall: &astral.ToolCall{ID: "two", Name: "lookup", Arguments: json.RawMessage(`{"value":2}`), Metadata: opaque}},
		{Type: "provider_extension", Data: opaque, Metadata: opaque},
	}}
}

func TestOrderedContentIsTheOnlyToolCallSource(t *testing.T) {
	want := messageFixture()
	if err := want.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["tool_calls"]; present {
		t.Fatal("message serialized a second tool call list")
	}
	var got astral.Message
	if err := json.Unmarshal(raw, &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered payload did not round trip: %s %v", raw, err)
	}
	calls := got.ToolCalls()
	if len(calls) != 2 || calls[0].ID != "one" || calls[1].ID != "two" {
		t.Fatalf("calls lost content order: %+v", calls)
	}
	calls[0].ID = "derived value"
	if got.Content[2].ToolCall.ID != "one" {
		t.Fatal("derived slice replaced the content source")
	}
}

func TestMessageValidatesRolesBlocksAndCalls(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*astral.Message)
	}{
		{"unknown-role", func(m *astral.Message) { m.Role = "system" }},
		{"empty-content", func(m *astral.Message) { m.Content = nil }},
		{"missing-type", func(m *astral.Message) { m.Content[0].Type = "" }},
		{"blank-type", func(m *astral.Message) { m.Content[0].Type = " \t" }},
		{"missing-call", func(m *astral.Message) { m.Content[2].ToolCall = nil }},
		{"call-in-text", func(m *astral.Message) { m.Content[2].Type = "text" }},
		{"call-in-extension", func(m *astral.Message) { m.Content[2].Type = "provider_extension" }},
		{"user-call", func(m *astral.Message) { m.Role = astral.RoleUser }},
		{"context-call", func(m *astral.Message) { m.Role = astral.RoleContext }},
		{"tool-call", func(m *astral.Message) { m.Role, m.ToolCallID = astral.RoleTool, "result" }},
		{"duplicate-id", func(m *astral.Message) { m.Content[5].ToolCall.ID = "one" }},
		{"missing-id", func(m *astral.Message) { m.Content[2].ToolCall.ID = "" }},
		{"missing-name", func(m *astral.Message) { m.Content[2].ToolCall.Name = "" }},
		{"missing-arguments", func(m *astral.Message) { m.Content[2].ToolCall.Arguments = nil }},
		{"invalid-arguments", func(m *astral.Message) { m.Content[2].ToolCall.Arguments = json.RawMessage(`{"value":`) }},
		{"multiple-json-values", func(m *astral.Message) { m.Content[2].ToolCall.Arguments = json.RawMessage(`{} {}`) }},
		{"invalid-call-metadata", func(m *astral.Message) { m.Content[2].ToolCall.Metadata = json.RawMessage(`{`) }},
		{"invalid-part-data", func(m *astral.Message) { m.Content[0].Data = json.RawMessage(`{`) }},
		{"invalid-part-metadata", func(m *astral.Message) { m.Content[0].Metadata = json.RawMessage(`{`) }},
		{"invalid-message-metadata", func(m *astral.Message) { m.Metadata = json.RawMessage(`{`) }},
		{"unexpected-result-id", func(m *astral.Message) { m.ToolCallID = "result" }},
		{"missing-result-id", func(m *astral.Message) { m.Role, m.Content = astral.RoleTool, astral.Text("result") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := messageFixture()
			test.change(&message)
			if err := message.Validate(); !errors.Is(err, astral.ErrInvalid) {
				t.Fatalf("invalid message accepted: %v", err)
			}
		})
	}
}

func TestToolArgumentsRetainAnySingleJSONValue(t *testing.T) {
	for _, raw := range []string{`{"large":900719925474099312345}`, `[1,"two"]`, `"value"`, `42`, `false`, `null`} {
		t.Run(raw, func(t *testing.T) {
			message := messageFixture()
			message.Content[2].ToolCall.Arguments = json.RawMessage(raw)
			if err := message.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, role := range []astral.Role{astral.RoleUser, astral.RoleAssistant, astral.RoleTool, astral.RoleContext} {
		message := astral.Message{Role: role, Content: []astral.Content{{Type: "provider_extension", Data: json.RawMessage(`{"opaque":true}`)}}}
		if role == astral.RoleTool {
			message.ToolCallID = "result"
		}
		if err := message.Validate(); err != nil || len(message.ToolCalls()) != 0 {
			t.Fatalf("opaque block changed role or acquired execution semantics: %v", err)
		}
	}
}

func TestLegacyMessageCallListsAreRejectedRatherThanIgnored(t *testing.T) {
	for _, raw := range []string{
		`{"role":"assistant","content":[{"type":"text","text":"before"}],"tool_calls":[]}`,
		`{"role":"assistant","tool_calls":[{"id":"one","name":"work","arguments":{}}]}`,
		`{"role":"assistant","content":[{"type":"text"}],"tool_calls":null}`,
		`{"role":"assistant","content":[{"type":"text"}],"TOOL_CALLS":[]}`,
	} {
		message := astral.User("unchanged on failure")
		if err := json.Unmarshal([]byte(raw), &message); !errors.Is(err, astral.ErrInvalid) {
			t.Fatalf("legacy call list silently ignored: %v", err)
		}
		if !reflect.DeepEqual(message, astral.User("unchanged on failure")) {
			t.Fatal("failed decode partially changed its destination")
		}
	}
}
