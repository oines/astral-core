package integration_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	astral "github.com/oines/astral-core"

	"github.com/oines/astral-core/store/memory"
)

func TestCloseCancelsManualCompactionAndClosesLateSubscribers(t *testing.T) {
	entered := make(chan struct{})
	m := model(func(ctx context.Context, _ int, request astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if request.SystemPrompt == "summary" {
			close(entered)
			<-ctx.Done()
			return astral.Reply{}, ctx.Err()
		}
		return answer("done"), nil
	})
	c := testConfig(m, memory.New())
	c.Compaction = astral.CompactionPolicy{SystemPrompt: "summary", KeepRecentTokens: 1}
	a := newSessions(t, c)
	s := newSession(t, a, astral.SessionOptions{})
	waitOK(t, start(t, s, "hello"))
	finished := make(chan error, 1)
	go func() { _, err := s.Compact(context.Background(), ""); finished <- err }()
	signal(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("compaction: %v", err)
	}
	for _, entry := range entries(s) {
		if entry.Kind == astral.EntryCompaction {
			t.Fatal("cancelled summary committed")
		}
	}
	sub := s.Subscribe(8)
	if _, ok := <-sub.C; ok || !errors.Is(sub.Err(), astral.ErrClosed) {
		t.Fatal("late subscriber remained open")
	}
}

func TestSessionCloseUnloadsContextAndAllowsFreshOpen(t *testing.T) {
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("done"), nil
	})
	st := memory.New()
	a := newSessions(t, testConfig(m, st))
	s := newSession(t, a, astral.SessionOptions{ID: "unload"})
	waitOK(t, start(t, s, "history"))
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Stats().ContextEntries != 0 {
		t.Fatal("closed session retained context")
	}
	opened, err := a.OpenSession(context.Background(), "unload")
	if err != nil || opened == s {
		t.Fatalf("open returned closed cache entry: %v", err)
	}
	view, err := opened.InspectContext(context.Background())
	if err != nil || len(view.Request.Messages) != 2 {
		t.Fatalf("host-owned store was lost: %v", err)
	}
}

func TestCancellationPreservesUnknownEffectAndCancelledRunStatus(t *testing.T) {
	entered := make(chan struct{})
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return calls(call("effect", "effect", `{"value":"x"}`)), nil
	})
	c := testConfig(m, memory.New())
	c.Tools = []astral.Tool{tool("effect", func(ctx context.Context, _ astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolExecution, error) {
		close(entered)
		<-ctx.Done()
		return astral.ToolExecution{Result: &astral.ToolResult{}}, ctx.Err()
	})}
	s := newSession(t, newSessions(t, c), astral.SessionOptions{})
	r := start(t, s, "go")
	signal(t, entered)
	r.Cancel()
	result, err := wait(t, r)
	if result.Status != "cancelled" || !errors.Is(err, context.Canceled) || !errors.Is(err, astral.ErrOutcomeUnknown) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(s.UnresolvedTools()) != 1 {
		t.Fatal("unknown effect lost")
	}
}

func TestRequestTimeoutRetriesAndExhaustionIsFailure(t *testing.T) {
	for _, succeed := range []bool{true, false} {
		t.Run(fmt.Sprint(succeed), func(t *testing.T) {
			m := model(func(ctx context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
				if succeed && n == 2 {
					return answer("retried"), nil
				}
				<-ctx.Done()
				return astral.Reply{}, ctx.Err()
			})
			c := testConfig(m, memory.New())
			c.Retry = astral.RetryPolicy{MaxAttempts: 2, RequestTimeout: 10 * time.Millisecond, InitialBackoff: time.Millisecond}
			s := newSession(t, newSessions(t, c), astral.SessionOptions{})
			result, err := wait(t, start(t, s, "go"))
			if len(m.observed()) != 2 {
				t.Fatalf("attempt count: %d", len(m.observed()))
			}
			if succeed && (err != nil || result.Status != "completed") {
				t.Fatalf("success: %v %+v", err, result)
			}
			var classified *astral.ModelError
			if !succeed && (result.Status != "failed" || !errors.As(err, &classified) || classified.Code != "request_timeout") {
				t.Fatalf("exhaustion: %v %+v", err, result)
			}
		})
	}
}

func TestToolConcurrencyLimit(t *testing.T) {
	var current, peak, executed atomic.Int32
	m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n > 1 {
			return answer("done"), nil
		}
		var batch []astral.ToolCall
		for i := 0; i < 12; i++ {
			batch = append(batch, call(fmt.Sprint(i), "work", `{"value":"x"}`))
		}
		return calls(batch...), nil
	})
	c := testConfig(m, memory.New())
	c.MaxParallelTools = 2
	c.Tools = []astral.Tool{tool("work", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
		n := current.Add(1)
		for previous := peak.Load(); n > previous; previous = peak.Load() {
			if peak.CompareAndSwap(previous, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		current.Add(-1)
		executed.Add(1)
		return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text("done")}}, nil
	})}
	s := newSession(t, newSessions(t, c), astral.SessionOptions{})
	waitOK(t, start(t, s, "go"))
	if executed.Load() != 12 || peak.Load() > 2 {
		t.Fatalf("executed=%d peak=%d", executed.Load(), peak.Load())
	}
}

func TestResolvedOutputUsesDeliveryBudget(t *testing.T) {
	var effects atomic.Int32
	m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("effect", "effect", `{"value":"x"}`)), nil
		}
		return answer("done"), nil
	})
	m.spec.ContextWindow = 6000
	c := testConfig(m, memory.New())
	c.Tools = []astral.Tool{tool("effect", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
		effects.Add(1)
		return astral.ToolExecution{Result: &astral.ToolResult{Outcome: astral.OutcomeUnknown, Content: astral.Text("uncertain")}}, nil
	})}
	s := newSession(t, newSessions(t, c), astral.SessionOptions{})
	if _, err := wait(t, start(t, s, "go")); !errors.Is(err, astral.ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	id := s.UnresolvedTools()[0].Invocation.ExecutionID
	if err := s.ResolveTool(context.Background(), id, astral.ToolExecution{Result: &astral.ToolResult{Outcome: astral.OutcomeSucceeded, Content: astral.Text(strings.Repeat("large", 10000)), Artifact: "host://output"}}); err != nil {
		t.Fatal(err)
	}
	var resolved *astral.ToolResult
	for _, e := range entries(s) {
		if e.Kind == astral.EntryToolResolution {
			resolved = e.Tool.Result
		}
	}
	if resolved == nil || resolved.Code != "result_too_large" || resolved.Outcome != astral.OutcomeSucceeded || resolved.Artifact != "host://output" {
		t.Fatalf("resolution: %+v", resolved)
	}
	r, err := s.Continue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitOK(t, r)
	if effects.Load() != 1 {
		t.Fatal("effect repeated")
	}
}

func TestAfterHookCannotRewriteExecutionOutcome(t *testing.T) {
	m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("echo", "echo", `{"value":"x"}`)), nil
		}
		return answer("done"), nil
	})
	c := testConfig(m, memory.New())
	c.Tools = []astral.Tool{echoTool(nil)}
	c.AfterToolCall = func(_ context.Context, _ astral.Invocation, result astral.ToolResult) (astral.ToolResultPatch, error) {
		result.Outcome = astral.OutcomeFailed
		return astral.ToolResultPatch{Content: astral.Text("processed")}, nil
	}
	s := newSession(t, newSessions(t, c), astral.SessionOptions{})
	waitOK(t, start(t, s, "switch"))
	if toolResults(s)[0].Outcome != astral.OutcomeSucceeded {
		t.Fatal("execution facts changed by hook")
	}
}

func TestModelSelectionDuringCompactionAppliesToNextGeneration(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	normal := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("old model answer"), nil
	})
	normal.spec.ContextWindow = 1000
	normal.spec.MaxOutputTokens = 100
	normal.estimator = func(r astral.Request) (int, error) { return len(r.Messages) * 100, nil }
	summary := model(func(ctx context.Context, _ int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		close(entered)
		select {
		case <-release:
			return answer("summary"), nil
		case <-ctx.Done():
			return astral.Reply{}, ctx.Err()
		}
	})
	summary.spec.ID = "summary"
	next := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("new model answer"), nil
	})
	next.spec.ID = "next"
	c := testConfig(normal, memory.New())
	c.Budget = astral.BudgetPolicy{AutoCompactAt: 650}
	c.Compaction = astral.CompactionPolicy{ModelID: "summary", KeepRecentTokens: 1}
	c.ResolveModel = func(_ context.Context, id string) (astral.Model, error) {
		if id == "summary" {
			return summary, nil
		}
		return next, nil
	}
	s := newSession(t, newSessions(t, c), astral.SessionOptions{})
	waitOK(t, start(t, s, "first"))
	r := start(t, s, "second")
	signal(t, entered)
	if err := s.SelectModel(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if _, err := wait(t, r); !errors.Is(err, astral.ErrConflict) {
		t.Fatalf("changed configuration committed the stale draft: %v", err)
	}
	if len(next.observed()) != 0 || s.State().PendingInputs[astral.InputSteer] != 1 {
		t.Fatal("conflicted preparation consumed input")
	}
	r, err := s.Continue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := waitOK(t, r)
	if len(normal.observed()) != 1 || len(next.observed()) != 1 || result.LastMessage.Content[0].Text != "new model answer" {
		t.Fatal("model selected during compaction was ignored")
	}
}

func TestAfterHookPatchKeepsUnspecifiedFields(t *testing.T) {
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return calls(call("c", "work", `{"value":"x"}`)), nil
	})
	c := testConfig(m, memory.New())
	c.Tools = []astral.Tool{tool("work", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
		return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text("original"), IsError: true, Code: "original_code", Artifact: "host://artifact"}}, nil
	})}
	c.AfterToolCall = func(context.Context, astral.Invocation, astral.ToolResult) (astral.ToolResultPatch, error) {
		isError, terminate := false, true
		return astral.ToolResultPatch{IsError: &isError, Terminate: &terminate}, nil
	}
	s := newSession(t, newSessions(t, c), astral.SessionOptions{})
	waitOK(t, start(t, s, "go"))
	result := toolResults(s)[0]
	if result.Content[0].Text != "original" || result.Code != "original_code" || result.Artifact != "host://artifact" || result.IsError || !result.Terminate || result.Outcome != astral.OutcomeFailed || len(m.observed()) != 1 {
		t.Fatalf("patch: %+v", result)
	}
}
