package astral

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/oines/astral-core/environment"
)

func memoryTestAgent(t *testing.T, config Config) (*Agent, *memoryDriver) {
	t.Helper()
	config.Model = ModelFunc{Specification: ModelSpec{ID: "memory-test", ContextWindow: 100000, MaxOutputTokens: 4096}}
	a, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	return a, a.driver.(*memoryDriver)
}

func TestMemoryEnvironmentRejectsInvalidSnapshotWithoutAdoptingResult(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `true`, `42`, `"private-B"`, `{"binding":"private-B","context":42}`, `{"binding":"private-B","context":{}}`, `{"binding":`} {
		t.Run(raw, func(t *testing.T) {
			_, d := memoryTestAgent(t, Config{Binding: json.RawMessage(`"private-A"`)})
			call := ToolCall{ID: "switch", Name: "switch", Arguments: json.RawMessage(`{}`)}
			message := Message{Role: RoleAssistant, Content: []Content{{Type: "tool_call", ToolCall: &call}}}
			if err := d.Commit(context.Background(), Transition{Kind: TransitionMessage, ID: "assistant", Message: &message}); err != nil {
				t.Fatal(err)
			}
			before, messages := d.Snapshot(), clone(d.messages)
			result := ToolResult{Outcome: OutcomeSucceeded, Content: Text("public result"), Context: json.RawMessage(raw)}
			transition := Transition{Kind: TransitionResult, Invocation: Invocation{Call: call}, Result: &result}
			err := d.Commit(context.Background(), transition)
			if !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "private-B") {
				t.Fatalf("invalid snapshot accepted or exposed: %v", err)
			}
			if !reflect.DeepEqual(d.Snapshot(), before) || !reflect.DeepEqual(d.messages, messages) {
				t.Fatal("invalid snapshot changed binding, execution facts or context")
			}
			result.Context = json.RawMessage(`{"binding":"private-B","context":"public B"}`)
			if err := d.Commit(context.Background(), transition); err != nil {
				t.Fatal(err)
			}
			result.Context[0] = '!'
			if string(d.Snapshot().Binding) != `"private-B"` {
				t.Fatal("valid binding was enveloped or aliased")
			}
			projected := d.messages[len(d.messages)-1]
			if projected.ContextKind != "environment" || projected.Role != RoleContext || projected.Content[0].Text != "public B" {
				t.Fatalf("wrong public environment projection: %+v", projected)
			}
		})
	}
}

func TestMemoryEnvironmentPreservesOpaqueBindingValues(t *testing.T) {
	for _, binding := range []string{"", `null`, `true`, `42`, `"target"`, `[1,2]`, `{"large":900719925474099312345}`} {
		t.Run(binding, func(t *testing.T) {
			_, d := memoryTestAgent(t, Config{Binding: json.RawMessage(`"old"`)})
			var raw json.RawMessage
			if binding != "" {
				raw = json.RawMessage(binding)
			}
			payload, err := json.Marshal(environment.Snapshot{Binding: raw, Context: "public"})
			if err != nil {
				t.Fatal(err)
			}
			if err := d.Commit(context.Background(), Transition{Kind: TransitionResult, Result: &ToolResult{Context: payload}}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(d.Snapshot().Binding, raw) {
				t.Fatal("opaque binding value changed")
			}
		})
	}
}

func TestMemorySteeringBytePrefixDoesNotMutateQueueOrHistory(t *testing.T) {
	a, d := memoryTestAgent(t, Config{MaxSteeringPerTurn: 16})
	for _, value := range []string{"one", "two", "three"} {
		if _, err := a.Steer(context.Background(), User(value)); err != nil {
			t.Fatal(err)
		}
	}
	request := Request{ModelID: "memory-test", MaxOutputTokens: 16384, Messages: []Message{User("one"), User("two")}}
	raw, _ := json.Marshal(request)
	selected, err := d.steeringPrefix([]int{0, 1, 2}, len(raw))
	if err != nil || !reflect.DeepEqual(selected, []int{0, 1}) || len(d.inputs) != 3 || len(d.messages) != 0 {
		t.Fatalf("wrong or destructive byte prefix: %v %v", selected, err)
	}
	if _, err := d.steeringPrefix([]int{0, 1, 2}, 1); !errors.Is(err, ErrInvalid) || len(d.inputs) != 3 {
		t.Fatalf("oversized head was removed: %v", err)
	}
}

func TestMemorySteeringOversizedBoundaryAndCancelKeepInput(t *testing.T) {
	a, d := memoryTestAgent(t, Config{SystemPrompt: strings.Repeat("x", standaloneSteeringContextBytes), MaxSteeringPerTurn: 16})
	id, err := a.Steer(context.Background(), User("keep me"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Boundary(context.Background(), "run", true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized request admitted: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := d.Boundary(cancelled, "run", true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel ignored: %v", err)
	}
	if len(d.inputs) != 1 || d.inputs[0].ID != id || len(d.messages) != 0 {
		t.Fatal("rejected boundary lost its input")
	}
}
