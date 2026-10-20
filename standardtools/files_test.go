package standardtools_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/standardtools"
	"github.com/oines/astral-core/standardtools/local"
)

type harness struct {
	set   *standardtools.Set
	tools map[string]astral.Tool
	dir   string
}

func newHarness(t *testing.T, options ...standardtools.Options) *harness {
	t.Helper()
	dir := t.TempDir()
	b, err := local.New(local.Options{CWD: dir, Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := standardtools.New(b, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	h := &harness{s, map[string]astral.Tool{}, dir}
	for _, tool := range s.Tools() {
		h.tools[tool.Definition().Name] = tool
	}
	return h
}

func (h *harness) invoke(ctx context.Context, name string, args any) (astral.ToolResult, error) {
	data, err := json.Marshal(args)
	if err != nil {
		return astral.ToolResult{}, err
	}
	inv := astral.Invocation{SessionID: "owner", Call: astral.ToolCall{ID: "call", Name: name, Arguments: data}}
	execution, err := h.tools[name].Execute(ctx, inv, nil)
	return *execution.Result, err
}

func (h *harness) run(t *testing.T, name string, args any) astral.ToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := h.invoke(ctx, name, args)
	if err != nil || r.IsError {
		t.Fatalf("%s failed: %v %+v", name, err, r)
	}
	return r
}

func TestFileToolsReadPaginationWriteAndAllOrNothingEdit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.run(t, "write", standardtools.WriteRequest{Path: "nested/file.txt", Content: "\ufeffone\r\ntwo\r\nthree\r\n"})
	r := h.run(t, "read", map[string]any{"path": "nested/file.txt", "offset": 2, "limit": 1})
	if !strings.HasPrefix(r.Content[0].Text, "two\r\n\n[") || !strings.Contains(r.Content[0].Text, "offset=3") {
		t.Fatalf("pagination: %q", r.Content[0].Text)
	}
	r = h.run(t, "edit", standardtools.EditRequest{Path: "nested/file.txt", Edits: []standardtools.Edit{{OldText: "one", NewText: "ONE"}, {OldText: "three", NewText: "THREE"}}})
	var diff standardtools.EditDetails
	if err := json.Unmarshal(r.Details, &diff); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.Patch, "-one\n+ONE") || diff.FirstChangedLine != 1 {
		t.Fatalf("missing patch: %+v", diff)
	}
	path := filepath.Join(h.dir, "nested/file.txt")
	before, _ := os.ReadFile(path)
	if string(before) != "\ufeffONE\r\ntwo\r\nTHREE\r\n" {
		t.Fatalf("BOM or CRLF lost: %q", before)
	}
	_, err := h.invoke(context.Background(), "edit", standardtools.EditRequest{Path: "nested/file.txt", Edits: []standardtools.Edit{{OldText: "ONE", NewText: "changed"}, {OldText: "missing", NewText: "bad"}}})
	after, _ := os.ReadFile(path)
	if err == nil || !bytes.Equal(before, after) {
		t.Fatal("partially applied invalid edit batch")
	}
	if _, err := h.invoke(context.Background(), "read", map[string]any{"path": "nested/file.txt", "offset": 100}); err == nil {
		t.Fatal("out-of-range offset accepted")
	}
}

func TestReadBoundsAndContinuation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.run(t, "write", standardtools.WriteRequest{Path: "many.txt", Content: strings.Repeat("line\n", 2100)})
	r := h.run(t, "read", map[string]any{"path": "many.txt"})
	if !strings.Contains(r.Content[0].Text, "Showing lines 1-2000") || !strings.Contains(r.Content[0].Text, "offset=2001") || len(r.Details) == 0 {
		t.Fatal("line truncation missing")
	}
	r = h.run(t, "read", map[string]any{"path": "many.txt", "offset": 2001})
	if strings.Count(r.Content[0].Text, "line") != 100 {
		t.Fatal("continuation skips or repeats lines")
	}
	h.run(t, "write", standardtools.WriteRequest{Path: "long.txt", Content: strings.Repeat("字", 18000)})
	r = h.run(t, "read", map[string]any{"path": "long.txt"})
	if !strings.Contains(r.Content[0].Text, "exceeds 50.0KB") || len(r.Content[0].Text) > 300 {
		t.Fatal("oversized first line leaked")
	}
}

func TestReadImageAttachmentAndResize(t *testing.T) {
	t.Parallel()
	h := newHarness(t, standardtools.Options{Images: standardtools.ImageOptions{MaxWidth: 64, MaxHeight: 64}})
	canvas := image.NewNRGBA(image.Rect(0, 0, 128, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 128; x++ {
			canvas.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), A: 255})
		}
	}
	var raw bytes.Buffer
	if err := png.Encode(&raw, canvas); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, "image.png"), raw.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	r := h.run(t, "read", map[string]any{"path": "image.png"})
	if len(r.Content) != 2 || r.Content[1].Type != "image" || r.Content[1].MediaType != "image/png" || !strings.Contains(r.Content[0].Text, "displayed at 64x32") {
		t.Fatalf("image result: %+v", r.Content)
	}
	var encoded string
	if err := json.Unmarshal(r.Content[1].Data, &encoded); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width != 64 || config.Height != 32 {
		t.Fatalf("resized image: %+v %v", config, err)
	}
}

func TestLocalRejectsForeignBinding(t *testing.T) {
	b, err := local.New(local.Options{CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.FileSystem(context.Background(), astral.Invocation{Binding: json.RawMessage(`{"target":"remote"}`)})
	if err == nil {
		t.Fatal("foreign target silently routed locally")
	}
}
