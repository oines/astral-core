package client

import (
	"encoding/json"
	"strings"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models"
)

func TestImagePayloadValidationKeepsJSONAndBase64Semantics(t *testing.T) {
	for _, test := range []struct {
		name, raw, want string
		valid           bool
	}{
		{"ordinary", `"eA=="`, "eA==", true},
		{"whitespace", " \n\t\"eA==\" \n", "eA==", true},
		{"escaped ASCII", `"\u0065A=="`, "eA==", true},
		{"escaped newline", `"eA==\n"`, "eA==\n", true},
		{"escaped slash", `"\/w=="`, "/w==", true},
		{"invalid raw newline", "\"eA==\n\"", "", false},
		{"invalid base64", `"eA=!"`, "", false},
		{"empty", `""`, "", false},
		{"null", `null`, "", false},
		{"object", `{}`, "", false},
		{"invalid JSON", `"eA==`, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := &Client{}
			part := core.Content{Type: "image", MediaType: "image/png", Data: json.RawMessage(test.raw)}
			data, err := c.imageData(part)
			if (err == nil) != test.valid || test.valid && string(data) != test.want {
				t.Fatalf("data=%q error=%v", data, err)
			}
			if string(part.Data) != test.raw {
				t.Fatal("validation changed frozen input")
			}
		})
	}
}

func TestWireImagesCopyOnlyTheirChangedContainers(t *testing.T) {
	c := &Client{}
	request := core.Request{Messages: []core.Message{core.User("unchanged"), {Role: core.RoleUser, Content: []core.Content{{Type: "image", MediaType: "image/png", Data: json.RawMessage(`"eA=="`), URL: "host-file://original"}}}}}
	wire, err := c.WireRequest(request, false)
	if err != nil {
		t.Fatal(err)
	}
	if wire.Messages[1].Content[0].URL != "data:image/png;base64,eA==" || len(wire.Messages[1].Content[0].Data) != 0 {
		t.Fatal("wire pixels were not resolved")
	}
	if request.Messages[1].Content[0].URL != "host-file://original" || string(request.Messages[1].Content[0].Data) != `"eA=="` {
		t.Fatal("wire preparation mutated its source")
	}
	wire.Messages[1].Content[0].URL = "changed"
	if request.Messages[1].Content[0].URL != "host-file://original" {
		t.Fatal("wire preparation shares the modified content container")
	}
	// Estimate ignores bytes but retains both capability and payload checks.
	c.Config = models.Config{Spec: core.ModelSpec{ID: "target"}}
	c.Protocol = "openai-responses"
	request.ModelID = "target"
	if _, err := c.EstimateRequest(request); err != nil {
		t.Fatal(err)
	}
	c.Config.Images = "unsupported"
	if _, err := c.EstimateRequest(request); err == nil {
		t.Fatal("estimation accepted unsupported images")
	}
	c.Config.Images = "supported"
	request.Messages[1].Content[0].Data = json.RawMessage(`"` + strings.Repeat("A", (20<<20)*4/3+8) + `"`)
	if _, err := c.EstimateRequest(request); err == nil {
		t.Fatal("estimation accepted an oversized image")
	}
}

func TestBorrowedEstimationKeepsLegacyJSONNormalization(t *testing.T) {
	c := &Client{Protocol: "openai-responses", Config: models.Config{Spec: core.ModelSpec{ID: "target"}}}
	request := core.Request{ModelID: "target", Messages: []core.Message{{Role: core.RoleAssistant, Content: []core.Content{{Type: "tool_call", ToolCall: &core.ToolCall{ID: "call", Name: "lookup"}}}}}}
	if _, err := c.EstimateRequest(request); err != nil {
		t.Fatalf("required nil arguments no longer normalize to JSON null: %v", err)
	}
	if request.Messages[0].Content[0].ToolCall.Arguments != nil {
		t.Fatal("normalization mutated caller")
	}
	request.Messages[0].Content = []core.Content{
		{Type: "tool_call", ToolCall: &core.ToolCall{ID: string([]byte{0xff}), Name: "lookup", Arguments: json.RawMessage(`{}`)}},
		{Type: "tool_call", ToolCall: &core.ToolCall{ID: string([]byte{0xfe}), Name: "lookup", Arguments: json.RawMessage(`{}`)}},
	}
	if _, err := c.EstimateRequest(request); err == nil {
		t.Fatal("malformed UTF-8 identities bypassed normalized duplicate-call validation")
	}
}
