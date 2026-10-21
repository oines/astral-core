package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"

	astral "github.com/oines/astral-core"
)

func TestToolControlForksAndCompaction(t *testing.T) {
	eachStore(t, func(t *testing.T, st astral.Store) {
		ctx := context.Background()
		var effects atomic.Int32
		stop := controlTool("host_control", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
			effects.Add(1)
			return astral.ToolExecution{EndRun: &astral.RunEnd{Reason: "done"}}, nil
		})
		m := model(func(_ context.Context, _ int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			users := texts(r.Messages, astral.RoleUser)
			if len(users) > 0 && users[len(users)-1] == "control" {
				return controlReply(stop.Spec.Name), nil
			}
			return answer("ordinary answer"), nil
		})
		summary := model(func(_ context.Context, _ int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			checkControlProjection(t, r.Messages)
			return answer("completed prior inputs"), nil
		})
		summary.spec.ID = "summary"
		c := testConfig(m, st)
		c.Tools = []astral.Tool{stop}
		c.Compaction = astral.CompactionPolicy{ModelID: "summary", KeepRecentTokens: 1}
		c.ResolveModel = func(context.Context, string) (astral.Model, error) { return summary, nil }
		manager := newSessions(t, c)
		parent := newSession(t, manager, astral.SessionOptions{ID: "parent"})
		first := waitOK(t, start(t, parent, "control"))
		controlID := checkControlHistory(t, parent, "done")
		parentBefore := parent.State()
		for _, mode := range []astral.ForkMode{astral.ForkShared, astral.ForkDetached} {
			if _, err := parent.Fork(ctx, first.LastMessageID, astral.ForkOptions{ID: fmt.Sprintf("incomplete-%d", mode), Mode: mode}); !errors.Is(err, astral.ErrIncompleteBatch) {
				t.Fatalf("forked a control without its completion: %v", err)
			}
			child, err := parent.Fork(ctx, controlID, astral.ForkOptions{ID: fmt.Sprintf("child-%d", mode), Mode: mode})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := child.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
				t.Fatalf("fork replayed inherited control: %v", err)
			}
			view, err := child.InspectContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			checkControlProjection(t, view.Request.Messages)
			// Reuse the call ID and name on a new assistant. The inherited
			// completion must not suppress this branch's own execution.
			result := waitOK(t, start(t, child, "control"))
			if result.EndReason != "done" {
				t.Fatalf("child failed to execute its own control: %+v", result)
			}
			waitOK(t, start(t, child, "child input"))
			view, err = child.InspectContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			checkControlProjection(t, view.Request.Messages)
			if !reflect.DeepEqual(parent.State(), parentBefore) {
				t.Fatal("child execution modified its parent's boundary")
			}
		}
		if effects.Load() != 3 {
			t.Fatalf("control facts crossed branch boundaries: %d effects", effects.Load())
		}
		waitOK(t, start(t, parent, "later input"))
		before := entries(parent)
		record, err := parent.Compact(ctx, "retain useful context")
		if err != nil {
			t.Fatal(err)
		}
		if len(summary.observed()) != 1 || record.ThroughID == first.LastMessageID {
			t.Fatal("compaction split a control batch")
		}
		after := entries(parent)
		if !reflect.DeepEqual(before, after[:len(before)]) {
			t.Fatal("compaction rewrote original control history")
		}
		view, err := parent.InspectContext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		checkControlProjection(t, view.Request.Messages)
		if len(view.Request.Messages) == 0 || view.Request.Messages[0].ContextKind != "summary" {
			t.Fatal("missing compaction summary")
		}
		// Rebuild the pruned window from persisted references.
		if err := manager.Close(ctx); err != nil {
			t.Fatal(err)
		}
		reopened, err := newSessions(t, c).OpenSession(ctx, parent.ID())
		if err != nil {
			t.Fatal(err)
		}
		waitOK(t, start(t, reopened, "after compaction"))
		checkControlProjection(t, m.observed()[len(m.observed())-1].Messages)
		if effects.Load() != 3 {
			t.Fatal("compaction or reopen replayed a control")
		}
	})
}

func TestExclusiveOrdinaryToolAllowsAssistantText(t *testing.T) {
	eachControlEngine(t, func(t *testing.T, st astral.Store) {
		var effects atomic.Int32
		work := astral.ToolFunc{
			Spec: astral.ToolDefinition{Name: "exclusive", Version: "1", Exclusive: true, Parameters: json.RawMessage(`{"type":"object"}`)},
			Run: func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
				effects.Add(1)
				return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text("result")}}, nil
			},
		}
		m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			if n == 1 {
				reply := calls(call("work", work.Spec.Name, `{}`))
				reply.Message.Content = append(astral.Text("I will run this alone."), reply.Message.Content...)
				return reply, nil
			}
			return answer("done"), nil
		})
		e, _ := controlEndpoint(t, st, astral.Config{Model: m, Tools: []astral.Tool{work}})
		run, err := prompt(e, context.Background(), astral.User("go"))
		if err != nil {
			t.Fatal(err)
		}
		if result := waitOK(t, run); result.EndReason != "" || effects.Load() != 1 || len(m.observed()) != 2 {
			t.Fatalf("ordinary Exclusive tool lost its result path: %+v", result)
		}
	})
}
