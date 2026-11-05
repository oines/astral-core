package provider_test

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models/provider"
	"github.com/oines/astral-core/models/provider/openai/completions"
	"github.com/oines/astral-core/standardtools"
	"github.com/oines/astral-core/standardtools/local"
	"github.com/oines/astral-core/store/memory"
)

func TestSessionReadImageContinuesWithoutVisionAndRetainsPixelsForModelSwitch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir := t.TempDir()
	var pixels bytes.Buffer
	if err := png.Encode(&pixels, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "image.png"), pixels.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	backend, err := local.New(local.Options{CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	set, err := standardtools.New(backend)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close(context.Background())
	var tools []core.Tool
	for _, tool := range set.Tools() {
		if tool.Definition().Name == "read" {
			tools = append(tools, tool)
		}
	}
	var received [][]byte
	transport := executionTransport(func(request *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		received = append(received, raw)
		response := `{"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`
		if len(received) == 1 {
			response = `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"read-image","type":"function","function":{"name":"read","arguments":"{\"path\":\"image.png\"}"}}]},"finish_reason":"tool_calls"}]}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(response)), Request: request}, nil
	})
	bound := make(map[string]provider.Model)
	for _, id := range []string{"text", "visual"} {
		cfg := config(id, "scope")
		cfg.Images = "unsupported"
		if id == "visual" {
			cfg.Images = "supported"
		}
		cfg.HTTPClient = &http.Client{Transport: transport}
		bound[id], err = provider.New(completions.Protocol, cfg)
		if err != nil {
			t.Fatal(err)
		}
	}
	session, err := core.NewSession(ctx, core.Config{
		Store: memory.New(), Selection: core.ModelSelection{ID: "text"}, Tools: tools,
		ResolveModel: func(_ context.Context, selection core.ModelSelection) (core.ResolvedModel, error) {
			m := bound[selection.ID]
			if m == nil {
				return core.ResolvedModel{}, fmt.Errorf("unexpected selection %q", selection.ID)
			}
			return core.ResolvedModel{Model: m}, nil
		},
		EstimateTokensReadOnly: func(ctx context.Context, _ core.Model, request core.Request) (int, error) {
			m := bound[request.ModelID]
			if m == nil {
				return 0, fmt.Errorf("unexpected estimate target %q", request.ModelID)
			}
			return m.EstimateTokens(ctx, request)
		},
		Compaction: core.CompactionPolicy{Disabled: true},
	}, core.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	prompt := func(id, text string) {
		t.Helper()
		result, err := session.Prompt(ctx, core.Input{ID: id, Message: core.User(text)})
		if err != nil {
			t.Fatal(err)
		}
		if result.Run == nil {
			t.Fatal("prompt did not start a run")
		}
		if _, err := result.Run.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	prompt("read", "Read image.png")
	if len(received) != 2 || !bytes.Contains(received[1], []byte(omittedToolImage)) || bytes.Contains(received[1], []byte("data:image/")) {
		t.Fatalf("read image prevented the next text-only request: calls=%d", len(received))
	}
	view, err := session.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundPixels := false
	for _, message := range view.Request.Messages {
		if message.Role == core.RoleTool {
			for _, part := range message.Content {
				foundPixels = foundPixels || part.Type == "image" && len(part.Data) > 0
			}
		}
	}
	if !foundPixels {
		t.Fatal("request downgrade removed pixels from durable read history")
	}
	if err := session.SelectModel(ctx, core.ModelSelection{ID: "visual"}); err != nil {
		t.Fatal(err)
	}
	prompt("inspect", "Inspect the previously read image")
	if len(received) != 3 || !bytes.Contains(received[2], []byte("data:image/png;base64,")) || bytes.Contains(received[2], []byte(omittedToolImage)) {
		t.Fatal("switching to a visual model did not recover the source image")
	}
}
