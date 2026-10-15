package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/environment"
)

func TestStandaloneSnapshotSeparatesExecutionBindingFromModelContext(t *testing.T) {
	bindings := []string{`{"private":"ONLY-EXECUTION-A"}`, `{"private":"ONLY-EXECUTION-B"}`, `{"private":"ONLY-EXECUTION-C"}`}
	var observed []string
	work := astral.ToolFunc{Spec: astral.ToolDefinition{Name: "environment_work", Version: "1", Sequential: true, Parameters: json.RawMessage(valueSchema)}, Run: func(_ context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolResult, error) {
		observed = append(observed, string(inv.Binding))
		result := astral.ToolResult{Content: []astral.Content{{Type: "text", Text: inv.Call.ID, Metadata: json.RawMessage(`{"public_result":true}`)}}}
		if inv.Call.ID == "switch-b" || inv.Call.ID == "switch-c" {
			next, text := 1, "public environment B"
			if inv.Call.ID == "switch-c" {
				next, text = 2, "public environment C"
			}
			result.Context, _ = json.Marshal(environment.Snapshot{Binding: json.RawMessage(bindings[next]), Context: text})
		}
		inv.Binding[0] = '!'
		return result, nil
	}}
	m := model(func(_ context.Context, n int, request astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		raw, _ := json.Marshal(request)
		if strings.Contains(string(raw), "ONLY-EXECUTION-") || request.SystemPrompt != "stable system" {
			t.Fatal("private binding leaked into request or system changed")
		}
		if n == 1 {
			return calls(call("switch-b", "environment_work", `{"value":"b"}`), call("use-b", "environment_work", `{"value":"b"}`), call("switch-c", "environment_work", `{"value":"c"}`), call("use-c", "environment_work", `{"value":"c"}`)), nil
		}
		var environments, resultIDs []string
		for _, message := range request.Messages {
			if message.Role == astral.RoleContext {
				if message.ContextKind != "environment" {
					t.Fatal("environment context kind changed")
				}
				environments = append(environments, message.Content[0].Text)
			}
			if message.Role == astral.RoleTool {
				resultIDs = append(resultIDs, message.ToolCallID)
				if message.Content[0].Text != message.ToolCallID || string(message.Content[0].Metadata) != `{"public_result":true}` {
					t.Fatal("tool result content changed")
				}
			}
		}
		if !reflect.DeepEqual(environments, []string{"public environment B", "public environment C"}) {
			t.Fatalf("public context missing or reordered: %v", environments)
		}
		want := []string{"switch-b", "use-b", "switch-c", "use-c"}
		if n == 3 {
			want = append(want, "later-c")
		}
		if !reflect.DeepEqual(resultIDs, want) || request.Messages[6].ContextKind != "environment" {
			t.Fatalf("result batch or environment placement changed: %v", resultIDs)
		}
		if n == 2 {
			return calls(call("later-c", "environment_work", `{"value":"c"}`)), nil
		}
		return answer("done"), nil
	})
	a, err := astral.New(astral.Config{Model: executionOnlyModel{m}, Binding: json.RawMessage(bindings[0]), SystemPrompt: "stable system", Tools: []astral.Tool{work}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	run, err := a.Start(context.Background(), astral.User("start"))
	if err != nil {
		t.Fatal(err)
	}
	waitOK(t, run)
	if !reflect.DeepEqual(observed, []string{bindings[0], bindings[1], bindings[1], bindings[2], bindings[2]}) {
		t.Fatalf("sequential calls used the wrong bindings: %v", observed)
	}
}

func TestStandaloneRejectsInvalidEnvironmentBeforeAnotherToolOrRequest(t *testing.T) {
	for _, payload := range []string{`null`, `[]`, `42`, `{"binding":"PRIVATE-INVALID","context":false}`} {
		t.Run(payload, func(t *testing.T) {
			count := 0
			work := astral.ToolFunc{Spec: astral.ToolDefinition{Name: "bad_environment", Sequential: true, Parameters: json.RawMessage(valueSchema)}, Run: func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolResult, error) {
				count++
				return astral.ToolResult{Content: astral.Text("public"), Context: json.RawMessage(payload)}, nil
			}}
			m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
				return calls(call("bad", "bad_environment", `{"value":"x"}`), call("later", "bad_environment", `{"value":"x"}`)), nil
			})
			a, err := astral.New(astral.Config{Model: m, Binding: json.RawMessage(`"PRIVATE-ORIGINAL"`), Tools: []astral.Tool{work}})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close(context.Background())
			run, err := a.Start(context.Background(), astral.User("start"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = wait(t, run)
			if !errors.Is(err, astral.ErrInvalid) || strings.Contains(err.Error(), "PRIVATE-") || count != 1 || len(m.observed()) != 1 {
				t.Fatalf("invalid environment continued or leaked: calls=%d error=%v", count, err)
			}
		})
	}
}
