package client

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models"
)

func TestProjectRequestCopiesEditsButBorrowsUnchangedPayloads(t *testing.T) {
	ctx := context.Background()
	c := &Client{Protocol: "openai-responses", Scope: "target-scope", Config: models.Config{Model: "target"}}
	foreign := json.RawMessage(`{"astral_models":{"protocol":"openai-responses","scope":"foreign-scope","model":"foreign","field":"responses.item"}}`)
	request := core.Request{Messages: []core.Message{
		{Role: core.RoleUser, Content: []core.Content{{Type: "image", MediaType: "image/png", Data: json.RawMessage(`"eA=="`)}}},
		{Role: core.RoleAssistant, Content: []core.Content{
			{Type: "reasoning", Data: json.RawMessage(`{"encrypted_content":"foreign"}`), Metadata: foreign},
			{Type: "text", Text: "portable", Data: json.RawMessage(`{"type":"message"}`), Metadata: foreign},
			{Type: "tool_call", Data: json.RawMessage(`{"type":"function_call"}`), Metadata: foreign, ToolCall: &core.ToolCall{ID: "unsafe|call", Name: "lookup", Arguments: json.RawMessage(`{}`)}},
		}},
		{Role: core.RoleTool, ToolCallID: "unsafe|call", Content: core.Text("result")},
	}}
	before, _ := json.Marshal(request)
	projected, err := c.ProjectRequest(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(request)
	if !bytes.Equal(before, after) {
		t.Fatal("projection mutated shared source history")
	}
	if len(projected.Messages[1].Content) != 2 || projected.Messages[1].Content[1].ToolCall.ID == "unsafe|call" || projected.Messages[2].ToolCallID != projected.Messages[1].Content[1].ToolCall.ID {
		t.Fatal("foreign replay projection or call/result identity changed")
	}
	if projected.Messages[1].Content[1].ToolCall == request.Messages[1].Content[2].ToolCall {
		t.Fatal("rewritten tool ID retained the source ToolCall pointer")
	}
	if &projected.Messages[0].Content[0].Data[0] != &request.Messages[0].Content[0].Data[0] || &projected.Messages[1].Content[1].ToolCall.Arguments[0] != &request.Messages[1].Content[2].ToolCall.Arguments[0] {
		t.Fatal("readonly projection copied unchanged raw payloads")
	}
	owned, err := c.PrepareRequest(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	projectJSON, _ := json.Marshal(projected)
	ownedJSON, _ := json.Marshal(owned)
	if !bytes.Equal(projectJSON, ownedJSON) {
		t.Fatal("readonly projection differs from public preparation")
	}
	owned.Messages[0].Content[0].Data[1] = 'Z'
	if string(request.Messages[0].Content[0].Data) != `"eA=="` {
		t.Fatal("public preparation lost independent ownership")
	}
}

func TestProjectRequestLeavesUnchangedContainersBorrowed(t *testing.T) {
	c := &Client{}
	request := core.Request{Messages: []core.Message{core.User("unchanged")}}
	projected, err := c.ProjectRequest(context.Background(), request)
	if err != nil || &projected.Messages[0] != &request.Messages[0] || &projected.Messages[0].Content[0] != &request.Messages[0].Content[0] {
		t.Fatalf("unchanged projection materialized history: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ProjectRequest(ctx, request); err != context.Canceled {
		t.Fatalf("projection ignored cancellation: %v", err)
	}
}
