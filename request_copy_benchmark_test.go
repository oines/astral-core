package astral

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"strings"
	"testing"
)

type requestCopyModel struct{ ModelFunc }

func (requestCopyModel) PrepareRequest(_ context.Context, request Request) (Request, error) {
	return request, nil
}

func (requestCopyModel) ValidateRequest(_ context.Context, request Request) error { return nil }

func requestCopyFixture(tb testing.TB, kind string) Request {
	tb.Helper()
	result := Text(strings.Repeat("Synthetic read result. ", 8))
	if kind == "images" {
		pixels := image.NewNRGBA(image.Rect(0, 0, 512, 512))
		for at := 0; at < len(pixels.Pix); at += 4 {
			pixels.Pix[at], pixels.Pix[at+1], pixels.Pix[at+2], pixels.Pix[at+3] = byte(at), byte(at>>8), byte(at>>16), 255
		}
		var output bytes.Buffer
		encoder := png.Encoder{CompressionLevel: png.NoCompression}
		if err := encoder.Encode(&output, pixels); err != nil {
			tb.Fatal(err)
		}
		data, _ := json.Marshal(base64.StdEncoding.EncodeToString(output.Bytes()))
		for range 4 {
			result = append(result, Content{Type: "image", MediaType: "image/png", Data: data, Metadata: json.RawMessage(`{"source":"synthetic"}`)})
		}
	} else {
		result = Text(strings.Repeat("Synthetic large tool result with text and newlines.\n", 170000))
	}
	return Request{ModelID: "copy-benchmark", ModelRevision: "fixed", SystemPrompt: "Fixed synthetic instructions.", MaxOutputTokens: 4096,
		Tools: []ToolDefinition{{Name: "read", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
		Messages: []Message{User("Read the synthetic input."),
			{Role: RoleAssistant, Content: []Content{{Type: "tool_call", ToolCall: &ToolCall{ID: "read-1", Name: "read", Arguments: json.RawMessage(`{"path":"/synthetic/input"}`)}}}, FinishReason: FinishToolCalls},
			{Role: RoleTool, ToolCallID: "read-1", Content: result},
			{Role: RoleAssistant, Content: Text("Read completed."), FinishReason: FinishStop}, User("A small new passive observation.")}}
}

func BenchmarkRequestCopyBoundary(b *testing.B) {
	for _, kind := range []string{"images", "tool-text"} {
		request := requestCopyFixture(b, kind)
		b.Run(kind, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				prepared, err := PrepareRequest(context.Background(), requestCopyModel{}, request)
				if err != nil {
					b.Fatal(err)
				}
				if err := ValidateRequest(context.Background(), requestCopyModel{}, prepared); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
