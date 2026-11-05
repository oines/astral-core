package client

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models"
)

func TestNonVisionProjectionKeepsSourceAndOnlyCopiesChangedContainers(t *testing.T) {
	c := &Client{Config: models.Config{Images: "unsupported"}}
	image := core.Content{Type: "image", MediaType: "image/png", Data: json.RawMessage(`"eA=="`), URL: "host-file://source", Metadata: json.RawMessage(`{"source":"pixels"}`)}
	request := core.Request{Messages: []core.Message{
		core.User("unchanged"),
		{Role: core.RoleUser, Content: []core.Content{image, image, {Type: "text", Text: "between"}, image}},
		{Role: core.RoleTool, ToolCallID: "call", Content: []core.Content{image, image}},
		{Role: core.RoleAssistant, Content: []core.Content{image}},
	}}
	before, _ := json.Marshal(request)
	projected, err := c.ProjectRequest(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]core.Content{
		core.Text("unchanged"),
		{{Type: "text", Text: nonVisionImagePlaceholder}, {Type: "text", Text: "between"}, {Type: "text", Text: nonVisionImagePlaceholder}},
		core.Text(nonVisionToolImagePlaceholder),
		core.Text(nonVisionImagePlaceholder),
	}
	for i, message := range projected.Messages {
		if !reflect.DeepEqual(message.Content, want[i]) {
			t.Fatalf("message %d: %+v", i, message.Content)
		}
	}
	after, _ := json.Marshal(request)
	if !bytes.Equal(before, after) || &projected.Messages[0].Content[0] != &request.Messages[0].Content[0] {
		t.Fatal("projection mutated source history or copied an unchanged content slice")
	}
	twice, err := c.ProjectRequest(context.Background(), projected)
	if err != nil || &twice.Messages[0] != &projected.Messages[0] {
		t.Fatal("idempotent projection recopied its containers")
	}
	for _, status := range []string{"", "unknown", "supported"} {
		c.Config.Images = status
		visual, err := c.ProjectRequest(context.Background(), request)
		if err != nil || &visual.Messages[1].Content[0].Data[0] != &request.Messages[1].Content[0].Data[0] {
			t.Fatalf("%q changed or copied frozen image data: %v", status, err)
		}
	}
}

func TestNonVisionProjectionDoesNotInterpretOpaqueNativeBlocks(t *testing.T) {
	for protocol, field := range map[string]string{
		"openai-completions": "chat.image", "openai-responses": "responses.item",
		"anthropic-messages": "anthropic.block", "google-generative-ai": "google.part",
	} {
		t.Run(protocol, func(t *testing.T) {
			c := &Client{Protocol: protocol, Scope: "scope", Config: models.Config{Model: "remote", Images: "unsupported"}}
			native := core.Content{Type: "provider", Data: json.RawMessage(`{"opaque":"native-image-or-signature"}`), Metadata: c.Metadata(field)}
			image := core.Content{Type: "image", Data: json.RawMessage(`{"native":"image"}`), Metadata: c.Metadata(field)}
			r := core.Request{Messages: []core.Message{{Role: core.RoleAssistant, Content: []core.Content{native, image}}}}
			projected, err := c.ProjectRequest(context.Background(), r)
			if err != nil || !reflect.DeepEqual(projected.Messages[0].Content, []core.Content{native, {Type: "text", Text: nonVisionImagePlaceholder}}) {
				t.Fatalf("typed image replacement changed opaque native replay: %+v %v", projected, err)
			}
			if &projected.Messages[0].Content[0].Data[0] != &native.Data[0] {
				t.Fatal("opaque native bytes were copied")
			}
		})
	}
}

func TestNonVisionEstimateCountsTheSameTextAsWireEncoding(t *testing.T) {
	c := &Client{Protocol: "openai-responses", Config: models.Config{Spec: core.ModelSpec{ID: "text"}, Images: "unsupported", ImageTokenBudget: 50000}}
	r := core.Request{ModelID: "text", Messages: []core.Message{{Role: core.RoleUser, Content: []core.Content{
		{Type: "text", Text: nonVisionImagePlaceholder},
		{Type: "image", Data: json.RawMessage(`"` + strings.Repeat("eHh4", 1<<16) + `"`), MediaType: "image/png"},
	}}}}
	wire, err := c.WireRequest(r, false)
	if err != nil || len(wire.Messages[0].Content) != 1 {
		t.Fatalf("image run was not collapsed after an existing placeholder: %v", err)
	}
	want, err := c.Estimate(wire)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.EstimateRequest(r)
	if err != nil || got != want || got > 100 {
		t.Fatalf("omitted image still charged an image budget: got %d want %d err %v", got, want, err)
	}
}

func BenchmarkNonVisionRequestProjection(b *testing.B) {
	c := &Client{Config: models.Config{Images: "unsupported"}}
	r := core.Request{Messages: []core.Message{{Role: core.RoleTool, ToolCallID: "read", Content: []core.Content{{Type: "image", MediaType: "image/png", Data: json.RawMessage(`"` + strings.Repeat("eHh4", 8<<20) + `"`)}}}}}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := c.ProjectRequest(context.Background(), r); err != nil {
			b.Fatal(err)
		}
	}
}
