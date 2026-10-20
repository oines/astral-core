package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	astral "github.com/oines/astral-core"
	sessions "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/store/memory"
)

func TestParallelResultsArePresentedInCallOrder(t *testing.T) {
	bothStarted := make(chan struct{}, 2)
	release := make(chan struct{})
	var effects atomic.Int32
	parallel := tool("parallel", func(ctx context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolExecution, error) {
		effects.Add(1)
		bothStarted <- struct{}{}
		select {
		case <-ctx.Done():
			return astral.ToolExecution{Result: &astral.ToolResult{}}, ctx.Err()
		case <-release:
		}
		return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text(inv.Call.ID)}}, nil
	})
	m := model(func(_ context.Context, n int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("first", "parallel", `{"value":"1"}`), call("second", "parallel", `{"value":"2"}`)), nil
		}
		if got := texts(r.Messages, astral.RoleTool); len(got) != 2 || got[0] != "first" || got[1] != "second" {
			return astral.Reply{}, errors.New("tool results reordered")
		}
		return answer("done"), nil
	})
	c := testConfig(m, memory.New())
	c.Tools = []astral.Tool{parallel}
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
	r := start(t, s, "work")
	signal(t, bothStarted)
	signal(t, bothStarted)
	close(release)
	waitOK(t, r)
	if effects.Load() != 2 {
		t.Fatal("calls not executed exactly once")
	}
}

func TestValidationAndMissingToolsProducePairedResults(t *testing.T) {
	var executed, hooks atomic.Int32
	m := model(func(_ context.Context, n int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("bad", "echo", `{"unexpected":4}`), call("missing", "missing", `{}`)), nil
		}
		for _, message := range r.Messages {
			if message.Role == astral.RoleTool && !message.IsError {
				return astral.Reply{}, errors.New("invalid call was not rejected")
			}
		}
		return answer("handled"), nil
	})
	c := testConfig(m, memory.New())
	c.Tools = []astral.Tool{echoTool(&executed)}
	c.BeforeToolCall = func(context.Context, sessions.ToolContext) (astral.ToolDecision, error) {
		hooks.Add(1)
		return astral.ToolDecision{}, nil
	}
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
	waitOK(t, start(t, s, "work"))
	if executed.Load() != 0 || hooks.Load() != 0 {
		t.Fatal("invalid call reached execution/hook")
	}
	results := toolResults(s)
	if len(results) != 2 {
		t.Fatal("missing terminal results")
	}
	for _, r := range results {
		if r.Outcome != astral.OutcomeNotExecuted {
			t.Fatal("wrong execution fact")
		}
	}
}

func TestTerminateRequiresEveryResultAndStillDrainsSteer(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(map[bool]string{false: "mixed", true: "all"}[all], func(t *testing.T) {
			m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
				if n == 1 {
					return calls(call("a", "echo", `{"value":"a"}`), call("b", "echo", `{"value":"b"}`)), nil
				}
				return answer("next"), nil
			})
			c := testConfig(m, memory.New())
			c.Tools = []astral.Tool{echoTool(nil)}
			c.AfterToolCall = func(_ context.Context, inv astral.Invocation, r astral.ToolResult) (astral.ToolResultPatch, error) {
				terminate := all || inv.Call.ID == "a"
				return astral.ToolResultPatch{Content: astral.Text("processed"), Terminate: &terminate}, nil
			}
			s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
			waitOK(t, start(t, s, "go"))
			want := 2
			if all {
				want = 1
			}
			if len(m.observed()) != want {
				t.Fatal("incorrect terminate aggregation")
			}
			for _, r := range toolResults(s) {
				if r.Content[0].Text != "processed" {
					t.Fatal("after hook not committed")
				}
			}
		})
	}
}

func TestSchemaReferencesCannotReadHostFiles(t *testing.T) {
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("unused"), nil
	})
	c := testConfig(m, memory.New())
	c.Tools = []astral.Tool{astral.ToolFunc{Spec: astral.ToolDefinition{Name: "bad", Parameters: json.RawMessage(`{"$ref":"file:///etc/passwd"}`)}}}
	if _, err := sessions.New(c); err == nil {
		t.Fatal("external schema accepted")
	}
}
