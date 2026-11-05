package standardtools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/memory"
)

func TestPiReadResultsReachNextModelUnchanged(t *testing.T) {
	var fixtures []struct {
		Name, Base64, Error string
		Input               json.RawMessage
		Content             []astral.Content
	}
	loadFixture(t, "pi_reads.json", &fixtures)
	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) {
			data, err := base64.StdEncoding.DecodeString(f.Base64)
			if err != nil {
				t.Fatal(err)
			}
			result := fileSessionResult(t, fixtureTool(t, "read", &fixtureFileSystem{data}), f.Input)
			want := f.Content
			if f.Error != "" {
				want = astral.Text(f.Error)
			}
			if result.IsError != (f.Error != "") || !reflect.DeepEqual(result.Content, want) {
				t.Fatalf("next model read Content differs: %+v, want %+v", result, want)
			}
		})
	}
}

// Verify what the next model actually receives, including Core's error
// conversion; direct Execute error assertions alone cannot prove this contract.
func TestPiEditErrorsReachNextModelUnchanged(t *testing.T) {
	var fixtures []struct {
		Name, Original, Error string
		Edits                 []Edit
	}
	loadFixture(t, "pi_edits.json", &fixtures)
	for _, f := range fixtures {
		if f.Error == "" {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			fs := &fixtureFileSystem{[]byte(f.Original)}
			tool := fixtureTool(t, "edit", fs)
			input, _ := json.Marshal(EditRequest{Path: "fixture.txt", Edits: f.Edits})
			result := fileSessionResult(t, tool, input)
			if !result.IsError || !reflect.DeepEqual(result.Content, astral.Text(f.Error)) || string(fs.data) != f.Original {
				t.Fatalf("next model got %+v, want exact error %q and no file changes", result, f.Error)
			}
		})
	}
}

func TestPiEditCompatibilityFormsRunThroughCoreSchema(t *testing.T) {
	var fixtures []struct{ Input, Expected json.RawMessage }
	loadFixture(t, "pi_edit_arguments.json", &fixtures)
	for _, f := range fixtures {
		var expected EditRequest
		if json.Unmarshal(f.Expected, &expected) != nil || len(expected.Edits) == 0 {
			continue
		}
		t.Run(string(f.Input), func(t *testing.T) {
			fs := &fixtureFileSystem{[]byte("a\nc\n")}
			tool := fixtureTool(t, "edit", fs)
			result := fileSessionResult(t, tool, f.Input)
			want := astral.Text(fmt.Sprintf("Successfully replaced %d block(s) in f.", len(expected.Edits)))
			if result.IsError || !reflect.DeepEqual(result.Content, want) {
				t.Fatalf("compatibility form did not cross schema/execute boundary: %+v", result)
			}
		})
	}
}

func fileSessionResult(t *testing.T, tool astral.Tool, input json.RawMessage) astral.Message {
	t.Helper()
	var result astral.Message
	var calls int
	model := astral.ModelFunc{
		Specification: astral.ModelSpec{ID: "file-parity", ContextWindow: 128000, MaxOutputTokens: 1024},
		Generate: func(_ context.Context, request astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			calls++
			if calls == 1 {
				return astral.Reply{Message: astral.Message{Role: astral.RoleAssistant, FinishReason: astral.FinishToolCalls, Content: []astral.Content{{Type: "tool_call", ToolCall: &astral.ToolCall{ID: "file-call", Name: tool.Definition().Name, Arguments: input}}}}}, nil
			}
			for _, message := range request.Messages {
				if message.Role == astral.RoleTool && message.ToolCallID == "file-call" {
					result = message
				}
			}
			return astral.Reply{Message: astral.Message{Role: astral.RoleAssistant, Content: astral.Text("done"), FinishReason: astral.FinishStop}}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := astral.NewSession(ctx, astral.Config{Model: model, Store: memory.New(), Tools: []astral.Tool{tool}, EstimateTokens: func(context.Context, astral.Model, astral.Request) (int, error) { return 1, nil }}, astral.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	run, err := session.Prompt(ctx, astral.Input{ID: "file-input", Message: astral.User("Run the file tool.")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Run.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || result.Role != astral.RoleTool {
		t.Fatalf("missing next-model tool result: calls=%d result=%+v", calls, result)
	}
	return result
}
