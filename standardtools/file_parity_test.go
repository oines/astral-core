package standardtools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	astral "github.com/oines/astral-core"
)

type fixtureFileSystem struct{ data []byte }

func (*fixtureFileSystem) ResolvePath(_ context.Context, path string, _ bool) (ResolvedPath, error) {
	return ResolvedPath{Path: path, Parent: ".", Key: "fixture:" + path}, nil
}
func (f *fixtureFileSystem) ReadFile(context.Context, string) ([]byte, error) {
	return bytes.Clone(f.data), nil
}
func (f *fixtureFileSystem) WriteFile(_ context.Context, _ string, data []byte) error {
	f.data = bytes.Clone(data)
	return nil
}
func (*fixtureFileSystem) MkdirAll(context.Context, string) error { return nil }

type fixtureFileBackend struct{ fs FileSystem }

func (b fixtureFileBackend) FileSystem(context.Context, astral.Invocation) (FileSystem, error) {
	return b.fs, nil
}
func (fixtureFileBackend) StartProcess(context.Context, astral.Invocation, ProcessRequest) (Process, error) {
	return nil, errors.New("unused")
}

func fixtureTool(t *testing.T, name string, fs FileSystem) astral.Tool {
	t.Helper()
	set, err := New(fixtureFileBackend{fs})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })
	for _, tool := range set.Tools() {
		if tool.Definition().Name == name {
			return tool
		}
	}
	t.Fatal("missing tool", name)
	return nil
}

func loadFixture(t *testing.T, name string, target any) {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}

func sameJSON(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

func TestStandardToolArgumentPreparationCapabilities(t *testing.T) {
	set, err := New(fixtureFileBackend{})
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close(context.Background())
	tools := set.Tools()
	if len(tools) != 5 {
		t.Fatalf("unexpected standard-tool count: %d", len(tools))
	}
	for _, tool := range tools {
		name := tool.Definition().Name
		_, prepares := tool.(astral.ToolArgumentPreparer)
		if prepares != (name == "edit") {
			t.Errorf("%s advertises PrepareArguments=%v; only edit needs compatibility preparation", name, prepares)
		}
		if _, targets := tool.(astral.TargetedTool); !targets {
			t.Errorf("%s lost its binding capability", name)
		}
	}
}

func TestPiFileDefinitions(t *testing.T) {
	var fixtures []struct {
		Name, Description string
		Parameters        json.RawMessage
		Guidelines        []string
	}
	loadFixture(t, "pi_file_definitions.json", &fixtures)
	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) {
			got := fixtureTool(t, f.Name, nil).Definition()
			if !sameJSON(got.Parameters, f.Parameters) || !strings.HasPrefix(got.Description, f.Description) {
				t.Fatalf("Pi description/schema differs: %+v", got)
			}
			// Core has no Pi prompt-contribution protocol. These instructions must
			// still reach the model through the portable tool declaration.
			for _, guideline := range f.Guidelines {
				if !strings.Contains(got.Description, guideline) {
					t.Errorf("missing model-visible guideline %q", guideline)
				}
			}
		})
	}
}

func TestPiReadInvocations(t *testing.T) {
	var fixtures []struct {
		Name, Base64, Error string
		Input, Details      json.RawMessage
		Content             []astral.Content
	}
	loadFixture(t, "pi_reads.json", &fixtures)
	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) {
			data, err := base64.StdEncoding.DecodeString(f.Base64)
			if err != nil {
				t.Fatal(err)
			}
			tool := fixtureTool(t, "read", &fixtureFileSystem{data})
			got, err := tool.Execute(context.Background(), astral.Invocation{Call: astral.ToolCall{Arguments: f.Input}}, nil)
			if f.Error != "" {
				if err == nil || err.Error() != f.Error {
					t.Fatalf("got %v, want exact Pi error %q", err, f.Error)
				}
				return
			}
			if err != nil || got.Result == nil || !reflect.DeepEqual(got.Result.Content, f.Content) || !sameJSON(got.Result.Details, f.Details) {
				t.Fatalf("read Content/Details differs: %+v %v", got.Result, err)
			}
		})
	}
}

func TestPiEditInvocations(t *testing.T) {
	var fixtures []struct {
		Name, Original, Expected, Error string
		Edits                           []Edit
	}
	loadFixture(t, "pi_edits.json", &fixtures)
	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) {
			fs := &fixtureFileSystem{[]byte(f.Original)}
			tool := fixtureTool(t, "edit", fs)
			input, _ := json.Marshal(EditRequest{Path: "fixture.txt", Edits: f.Edits})
			got, err := tool.Execute(context.Background(), astral.Invocation{Call: astral.ToolCall{Arguments: input}}, nil)
			if f.Error != "" {
				var typed *astral.ToolError
				if err == nil || err.Error() != f.Error || !errors.As(err, &typed) || typed.Outcome != astral.OutcomeNotExecuted || string(fs.data) != f.Original {
					t.Fatalf("error/no-write differs: %+v %v, file %q; want %q", got, err, fs.data, f.Error)
				}
				return
			}
			want := astral.Text("Successfully replaced " + fileNumber(float64(len(f.Edits))) + " block(s) in fixture.txt.")
			if err != nil || got.Result == nil || !reflect.DeepEqual(got.Result.Content, want) || string(fs.data) != f.Expected {
				t.Fatalf("edit Content/bytes differs: %+v %v, %q; want %q", got.Result, err, fs.data, f.Expected)
			}
		})
	}
}

func TestPiEditArgumentPreparation(t *testing.T) {
	var fixtures []struct{ Input, Expected json.RawMessage }
	loadFixture(t, "pi_edit_arguments.json", &fixtures)
	tool := fixtureTool(t, "edit", &fixtureFileSystem{})
	prepare := tool.(astral.ToolArgumentPreparer)
	for _, f := range fixtures {
		t.Run(string(f.Input), func(t *testing.T) {
			original := bytes.Clone(f.Input)
			got, err := prepare.PrepareArguments(f.Input)
			if err != nil || !sameJSON(got, f.Expected) || !bytes.Equal(f.Input, original) {
				t.Fatalf("got %s, %v; want %s, input unchanged", got, err, f.Expected)
			}
			again, err := prepare.PrepareArguments(got)
			if err != nil || !sameJSON(again, got) {
				t.Fatalf("preparation is not idempotent: %s %v", again, err)
			}
			var expected EditRequest
			if json.Unmarshal(f.Expected, &expected) != nil || len(expected.Edits) == 0 {
				return // Malformed normalized forms are Core schema errors.
			}
			fs := &fixtureFileSystem{[]byte("a\nc\n")}
			execute := fixtureTool(t, "edit", fs)
			gotCall, err := execute.Execute(context.Background(), astral.Invocation{Call: astral.ToolCall{Arguments: f.Input}}, nil)
			if err != nil || gotCall.Result == nil || gotCall.Result.Content[0].Text != "Successfully replaced "+fileNumber(float64(len(expected.Edits)))+" block(s) in f." {
				t.Fatalf("direct Execute did not prepare arguments: %+v %v", gotCall, err)
			}
		})
	}
}
