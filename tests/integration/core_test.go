package integration_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
	sessions "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/store/memory"
)

// Deliberately does not implement a token estimator.
type executionOnlyModel struct{ inner *testModel }

type blockedStartDriver struct {
	astral.Driver
	entered chan struct{}
}

func (d blockedStartDriver) Commit(ctx context.Context, _ astral.Transition) error {
	close(d.entered)
	<-ctx.Done()
	return ctx.Err()
}

func TestCoreCloseCancelsAndJoinsStartCommit(t *testing.T) {
	d := blockedStartDriver{entered: make(chan struct{})}
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("unused"), nil
	})
	a, err := astral.New(astral.Config{Model: m, Driver: d})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { _, err := a.Start(context.Background(), astral.User("go")); finished <- err }()
	signal(t, d.entered)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("start: %v", err)
	}
}

func (m executionOnlyModel) Spec() astral.ModelSpec { return m.inner.Spec() }
func (m executionOnlyModel) Stream(c context.Context, r astral.Request, e func(astral.ModelEvent)) (astral.Reply, error) {
	return m.inner.Stream(c, r, e)
}

func TestStandaloneCoreRequiresOnlyModelAndTools(t *testing.T) {
	var executions atomic.Int32
	m := model(func(_ context.Context, n int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("c", "echo", `{"value":"hello"}`)), nil
		}
		if len(r.Messages) != 3 || r.Messages[2].ToolCallID != "c" {
			t.Fatalf("wrong tool context: %+v", r.Messages)
		}
		return answer("done"), nil
	})
	a, err := astral.New(astral.Config{Model: executionOnlyModel{m}, Tools: []astral.Tool{echoTool(&executions)}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	r, err := a.Start(context.Background(), astral.User("start"))
	if err != nil {
		t.Fatal(err)
	}
	got := waitOK(t, r)
	if got.Status != "completed" || executions.Load() != 1 {
		t.Fatalf("result=%+v calls=%d", got, executions.Load())
	}
}

func TestCoreAndPersistentSessionShareExecutionBehavior(t *testing.T) {
	type observed struct {
		requests   []astral.Request
		events     []astral.EventType
		executions int32
		status     string
	}
	run := func(persistent bool) observed {
		var count atomic.Int32
		m := model(func(_ context.Context, n int, _ astral.Request, emit func(astral.ModelEvent)) (astral.Reply, error) {
			if n == 1 {
				emit(astral.ModelEvent{Type: "text_delta", Text: "discarded"})
				return astral.Reply{}, &astral.ModelError{Transient: true, Message: "retry"}
			}
			if n == 2 {
				return calls(call("c", "echo", `{"value":"ok"}`)), nil
			}
			return answer("done"), nil
		})
		m.spec.MaxOutputTokens = 32000
		retry := astral.RetryPolicy{MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}
		var startRun func(context.Context, astral.Message) (*astral.Run, error)
		var sub *astral.Subscription
		if persistent {
			c := testConfig(m, memory.New())
			c.Retry = retry
			c.Tools = []astral.Tool{echoTool(&count)}
			s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
			sub = s.Subscribe(128)
			startRun = s.Start
		} else {
			a, err := astral.New(astral.Config{Model: executionOnlyModel{m}, Tools: []astral.Tool{echoTool(&count)}, Retry: retry})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { a.Close(context.Background()) })
			sub = a.Subscribe(128)
			startRun = a.Start
		}
		r, err := startRun(context.Background(), astral.User("start"))
		if err != nil {
			t.Fatal(err)
		}
		result := waitOK(t, r)
		sub.Close()
		out := observed{requests: m.observed(), executions: count.Load(), status: result.Status}
		for e := range sub.C {
			if e.Type != astral.EventCommitted {
				out.events = append(out.events, e.Type)
			}
		}
		return out
	}
	standalone, persistent := run(false), run(true)
	if !reflect.DeepEqual(standalone, persistent) {
		t.Fatalf("different execution semantics:\ncore=%+v\nsession=%+v", standalone, persistent)
	}
}

func TestCoreAndSessionShareInputAndParallelToolBoundaries(t *testing.T) {
	type endpoint interface {
		Start(context.Context, astral.Message) (*astral.Run, error)
		Steer(context.Context, astral.Message) (string, error)
		FollowUp(context.Context, astral.Message) (string, error)
		Subscribe(int) *astral.Subscription
	}
	type observation struct {
		requests []astral.Request
		events   map[astral.EventType]int
		status   string
	}
	exercise := func(persistent bool) observation {
		entered := make(chan struct{}, 3)
		release := make(chan struct{})
		var running, peak atomic.Int32
		work := tool("parallel", func(ctx context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolResult, error) {
			n := running.Add(1)
			defer running.Add(-1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return astral.ToolResult{}, ctx.Err()
			}
			return astral.ToolResult{Content: astral.Text(inv.Call.ID)}, nil
		})
		m := model(func(_ context.Context, n int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			if n == 1 {
				return calls(call("one", "parallel", `{"value":"x"}`), call("two", "parallel", `{"value":"x"}`), call("three", "parallel", `{"value":"x"}`)), nil
			}
			return answer(fmt.Sprintf("answer %d", n)), nil
		})
		var e endpoint
		if persistent {
			c := testConfig(m, memory.New())
			c.Tools = []astral.Tool{work}
			c.MaxParallelTools = 2
			e = newSession(t, newAgent(t, c), sessions.SessionOptions{})
		} else {
			a, err := astral.New(astral.Config{Model: m, Tools: []astral.Tool{work}, MaxParallelTools: 2})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { a.Close(context.Background()) })
			e = a
		}
		sub := e.Subscribe(256)
		r, err := e.Start(context.Background(), astral.User("initial"))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("parallel workers not started")
			}
		}
		if _, err = e.Steer(context.Background(), astral.User("steer")); err != nil {
			t.Fatal(err)
		}
		if _, err = e.FollowUp(context.Background(), astral.User("follow-up")); err != nil {
			t.Fatal(err)
		}
		close(release)
		result := waitOK(t, r)
		if peak.Load() != 2 {
			t.Fatalf("concurrency=%d", peak.Load())
		}
		sub.Close()
		out := observation{requests: m.observed(), events: map[astral.EventType]int{}, status: result.Status}
		for event := range sub.C {
			if event.Type != astral.EventCommitted {
				out.events[event.Type]++
			}
		}
		if len(out.requests) != 3 || !reflect.DeepEqual(texts(out.requests[1].Messages, astral.RoleTool), []string{"one", "two", "three"}) || !reflect.DeepEqual(texts(out.requests[2].Messages, astral.RoleUser), []string{"initial", "steer", "follow-up"}) {
			t.Fatalf("boundary requests: %+v", out.requests)
		}
		return out
	}
	if a, b := exercise(false), exercise(true); !reflect.DeepEqual(a, b) {
		t.Fatalf("common behavior diverged:\ncore=%+v\nsession=%+v", a, b)
	}
}

func TestCoreCancelDoesNotReplayTool(t *testing.T) {
	entered := make(chan struct{})
	var count atomic.Int32
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return calls(call("c", "wait", `{"value":"x"}`)), nil
	})
	tool := tool("wait", func(ctx context.Context, _ astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolResult, error) {
		count.Add(1)
		close(entered)
		<-ctx.Done()
		return astral.ToolResult{}, ctx.Err()
	})
	a, err := astral.New(astral.Config{Model: executionOnlyModel{m}, Tools: []astral.Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	r, err := a.Start(context.Background(), astral.User("start"))
	if err != nil {
		t.Fatal(err)
	}
	signal(t, entered)
	r.Cancel()
	_, err = wait(t, r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	if _, err = a.Continue(context.Background()); !errors.Is(err, astral.ErrOutcomeUnknown) {
		t.Fatalf("continue=%v", err)
	}
	if count.Load() != 1 {
		t.Fatalf("replayed %d times", count.Load())
	}
}
