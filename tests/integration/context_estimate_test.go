package integration_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/memory"
)

func TestUsageAnchorSurvivesReopenForkAndPassiveTail(t *testing.T) {
	eachStore(t, func(t *testing.T, store core.Store) {
		ctx := context.Background()
		cached, written := 600, 100
		m := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
			reply := answer("answer")
			reply.Usage = core.Usage{Available: true, InputTokens: 1000, OutputTokens: 75, CachedInputTokens: &cached, CacheWriteTokens: &written}
			return reply, nil
		})
		var rejectOld atomic.Bool
		m.estimator = func(r core.Request) (int, error) {
			if rejectOld.Load() && strings.Contains(requestText(r), "old-history") {
				return 0, errors.New("old covered messages were estimated again")
			}
			return contentEstimate(r)
		}
		c := testConfig(m, store)
		c.SystemPrompt = "rules already included in reported input"
		s := newSession(t, newSessions(t, c), core.SessionOptions{})
		waitOK(t, start(t, s, strings.Repeat("old-history", 1000)))
		rejectOld.Store(true)
		check := func(session *core.Session, want int) {
			t.Helper()
			view, err := session.InspectContext(ctx)
			if err != nil || view.EstimatedInput != want {
				t.Fatalf("input=%d want=%d err=%v", view.EstimatedInput, want, err)
			}
		}
		check(s, 1075) // Cache breakdowns must not be added again.
		entry, err := s.History().Get(ctx, s.Head())
		if err != nil || entry.Generation == nil {
			t.Fatalf("missing successful attempt provenance: %+v %v", entry, err)
		}
		request, err := s.Attempts().Get(ctx, entry.Generation.AttemptID)
		if err != nil || request.Purpose != "generation" || request.Outcome == nil || request.Outcome.EntryID != entry.ID || request.Outcome.Reply != nil {
			t.Fatalf("assistant refers to the wrong request: %+v %v", request, err)
		}
		for _, mode := range []core.ForkMode{core.ForkShared, core.ForkDetached} {
			id := "shared-usage"
			if mode == core.ForkDetached {
				id = "detached-usage"
			}
			child, err := s.Fork(ctx, s.Head(), core.ForkOptions{ID: id, Mode: mode})
			if err != nil {
				t.Fatal(err)
			}
			check(child, 1075)
			child.Close(ctx)
		}
		if err := s.Close(ctx); err != nil {
			t.Fatal(err)
		}
		reopened, err := core.OpenSession(ctx, c, s.ID())
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close(ctx)
		check(reopened, 1075)
		tail := snapshotMessage("新增观察")
		if _, err := inject(reopened, ctx, []core.Message{tail}); err != nil {
			t.Fatal(err)
		}
		count, _ := contentEstimate(core.Request{Messages: []core.Message{tail}})
		check(reopened, 1075+count-10)
		if len(m.observed()) != 1 {
			t.Fatal("passive maintenance generated an answer")
		}
	})
}

func TestUsageAnchorInvalidatesForConfigurationModelAndCompaction(t *testing.T) {
	for _, change := range []string{"configuration", "revision", "model", "compaction"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			m := model(func(_ context.Context, _ int, r core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
				reply := answer("answer")
				reply.Usage = core.Usage{Available: true, InputTokens: 1000, OutputTokens: 75}
				if r.SystemPrompt == "summary" {
					reply = answer("compressed")
					reply.Usage = core.Usage{Available: true, InputTokens: 90000, OutputTokens: 10}
				}
				return reply, nil
			})
			m.estimator = contentEstimate
			other := model(m.generate)
			other.spec.ID = "other"
			other.estimator = contentEstimate
			c := testConfig(m, memory.New())
			c.ResolveModel = func(context.Context, string) (core.Model, error) { return other, nil }
			c.Compaction = core.CompactionPolicy{Disabled: true, SystemPrompt: "summary", KeepRecentTokens: 1}
			s := newSession(t, newSessions(t, c), core.SessionOptions{})
			waitOK(t, start(t, s, strings.Repeat("old", 1000)))
			switch change {
			case "configuration":
				config := s.Configuration()
				config.SystemPrompt = "changed rules"
				if err := s.Configure(ctx, config); err != nil {
					t.Fatal(err)
				}
			case "revision":
				m.spec.Revision = "new-wire-contract"
			case "model":
				if err := s.SelectModel(ctx, "other"); err != nil {
					t.Fatal(err)
				}
			case "compaction":
				if _, err := s.Compact(ctx, ""); err != nil {
					t.Fatal(err)
				}
			}
			view, err := s.InspectContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := contentEstimate(view.Request)
			if view.EstimatedInput != want {
				t.Fatalf("stale usage retained: got %d want %d", view.EstimatedInput, want)
			}
		})
	}
}

func TestMissingAndZeroUsageUseContentEstimate(t *testing.T) {
	for _, usage := range []core.Usage{{}, {InputTokens: 1234}, {Available: true}, {Available: true, InputTokens: -1, OutputTokens: 5}} {
		m := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
			reply := answer("answer")
			reply.Usage = usage
			return reply, nil
		})
		m.estimator = contentEstimate
		s := newSession(t, newSessions(t, testConfig(m, memory.New())), core.SessionOptions{})
		waitOK(t, start(t, s, "hello"))
		view, err := s.InspectContext(context.Background())
		want, _ := contentEstimate(view.Request)
		if err != nil || view.EstimatedInput != want {
			t.Fatalf("input=%d want=%d err=%v", view.EstimatedInput, want, err)
		}
	}
}

func TestPiThresholdSummaryAllowanceAndDynamicOutput(t *testing.T) {
	var input atomic.Int64
	input.Store(100)
	m := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
		return answer("answer"), nil
	})
	m.spec.ContextWindow, m.spec.MaxOutputTokens = 100000, 80000
	m.estimator = func(core.Request) (int, error) { return int(input.Load()), nil }
	summary := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
		return answer("summary"), nil
	})
	summary.spec.ID, summary.spec.MaxOutputTokens = "summary", 20000
	c := testConfig(m, memory.New())
	c.Budget = core.BudgetPolicy{} // Exercise production defaults.
	c.Compaction = core.CompactionPolicy{ModelID: "summary"}
	c.ResolveModel = func(context.Context, string) (core.Model, error) { return summary, nil }
	s := newSession(t, newSessions(t, c), core.SessionOptions{})
	waitOK(t, start(t, s, "first"))
	if got := m.observed()[0].MaxOutputTokens; got != 80000 {
		t.Fatalf("default output=%d", got)
	}
	input.Store(100000 - 16384)
	waitOK(t, start(t, s, "at threshold"))
	if len(summary.observed()) != 0 {
		t.Fatal("compaction threshold must be strictly greater")
	}
	if got := m.observed()[1].MaxOutputTokens; got != 12288 {
		t.Fatalf("dynamic output=%d", got)
	}
	input.Add(1)
	waitOK(t, start(t, s, "over threshold"))
	if len(summary.observed()) != 1 || summary.observed()[0].MaxOutputTokens != 13107 {
		t.Fatalf("summary allowance: %+v", summary.observed())
	}
	if got := m.observed()[2].MaxOutputTokens; got != 12287 {
		t.Fatalf("dynamic output after compact=%d", got)
	}
}

func TestDefaultCompactionKeepsTwentyThousandTokensInCompleteGroups(t *testing.T) {
	m := model(func(_ context.Context, _ int, request core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
		if request.SystemPrompt == "summary" {
			return answer("summary"), nil
		}
		return answer(strings.Repeat("a", 6000)), nil
	})
	m.estimator = contentEstimate
	c := testConfig(m, memory.New())
	c.Compaction = core.CompactionPolicy{Disabled: true, SystemPrompt: "summary"}
	s := newSession(t, newSessions(t, c), core.SessionOptions{})
	for i := 0; i < 6; i++ {
		waitOK(t, start(t, s, strings.Repeat("u", 6000)))
	}
	if _, err := s.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	view, err := s.InspectContext(context.Background())
	if err != nil || len(view.Request.Messages) != 5 || view.Request.Messages[0].ContextKind != "summary" {
		t.Fatalf("retention did not keep four complete 6000-token groups: %d %v", len(view.Request.Messages), err)
	}
}

type outputValidatedModel struct {
	*testModel
	validatedOutput atomic.Int64
}

func (m *outputValidatedModel) ValidateRequest(_ context.Context, request core.Request) error {
	m.validatedOutput.Store(int64(request.MaxOutputTokens))
	if request.MaxOutputTokens < 500 {
		return errors.New("selected reasoning budget requires at least 500 output tokens")
	}
	return nil
}

func TestFinalOutputValidationPrecedesInputCommit(t *testing.T) {
	m := &outputValidatedModel{testModel: model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
		return answer("must not send"), nil
	})}
	m.spec.ContextWindow, m.spec.MaxOutputTokens = 10000, 8000
	m.estimator = func(core.Request) (int, error) { return 5700, nil }
	c := testConfig(m, memory.New())
	c.Budget = core.BudgetPolicy{}
	s := newSession(t, newSessions(t, c), core.SessionOptions{})
	head := s.Head()
	accepted, err := s.Prompt(context.Background(), core.Input{ID: "pending", Message: core.User("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = accepted.Run.Wait(context.Background())
	if err == nil || m.validatedOutput.Load() != 204 || len(m.observed()) != 0 || s.Head() != head {
		t.Fatalf("final request validation failed: output=%d sent=%d error=%v", m.validatedOutput.Load(), len(m.observed()), err)
	}
	receipt, err := s.LookupInput(context.Background(), "pending")
	if err != nil || receipt.State != core.InputPending {
		t.Fatalf("input consumed before validation: %+v %v", receipt, err)
	}
}
