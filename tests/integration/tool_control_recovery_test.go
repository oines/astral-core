package integration_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	astral "github.com/oines/astral-core"
	sessions "github.com/oines/astral-core/session"
)

type controlFaultStore struct {
	sessions.Store
	stage  string
	fired  atomic.Bool
	cancel context.CancelFunc
}

func (s *controlFaultStore) Commit(ctx context.Context, id string, c sessions.Commit) (sessions.Receipt, error) {
	target := sessions.EntryToolControl
	if s.stage == "before-start" || s.stage == "started" {
		target = sessions.EntryToolStarted
	}
	fail := false
	for _, e := range c.Entries {
		if e.Kind == target && !s.fired.Swap(true) {
			fail = true
		}
	}
	if fail && (s.stage == "before-start" || s.stage == "before-control") {
		return sessions.Receipt{}, errors.New("injected commit failure")
	}
	receipt, err := s.Store.Commit(ctx, id, c)
	if err != nil {
		return receipt, err
	}
	if fail {
		if s.stage == "cancel" {
			s.cancel()
		} else {
			return sessions.Receipt{}, errors.New("lost commit acknowledgement")
		}
	}
	return receipt, nil
}

func (s *controlFaultStore) LookupCommit(ctx context.Context, id, key string) (sessions.Receipt, error) {
	if s.fired.Load() && (s.stage == "started" || s.stage == "unconfirmed-control") {
		return sessions.Receipt{}, errors.New("commit query unavailable")
	}
	return s.Store.LookupCommit(ctx, id, key)
}

func TestToolControlCommitFailuresAndCancellation(t *testing.T) {
	for _, stage := range []string{"before-start", "started", "before-control", "lost-ack", "unconfirmed-control", "cancel"} {
		t.Run(stage, func(t *testing.T) {
			eachStore(t, func(t *testing.T, base sessions.Store) {
				ctx := context.Background()
				runCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				st := &controlFaultStore{Store: base, stage: stage, cancel: cancel}
				var effects atomic.Int32
				stop := controlTool("host_stop", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
					effects.Add(1)
					return astral.ToolExecution{EndRun: &astral.RunEnd{Reason: "host_stop"}}, nil
				})
				m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
					if n == 1 {
						return controlReply(stop.Spec.Name), nil
					}
					return answer("new input"), nil
				})
				c := testConfig(m, st)
				c.Tools = []astral.Tool{stop}
				manager := newAgent(t, c)
				s := newSession(t, manager, sessions.SessionOptions{})
				run, err := s.Start(runCtx, astral.User("work"))
				if err != nil {
					t.Fatal(err)
				}
				result, err := wait(t, run)
				confirmed := stage == "lost-ack" || stage == "cancel"
				if confirmed {
					if err != nil || result.Status != "completed" || result.EndReason != "host_stop" {
						t.Fatalf("committed completion lost: %+v %v", result, err)
					}
				} else if err == nil {
					t.Fatal("unconfirmed commit reported success")
				}
				if err := manager.Close(ctx); err != nil {
					t.Fatal(err)
				}
				c.Store = base
				reopened, err := newAgent(t, c).OpenSession(ctx, s.ID())
				if err != nil {
					t.Fatal(err)
				}
				switch stage {
				case "before-start":
					if effects.Load() != 0 {
						t.Fatal("tool ran before Started was confirmed")
					}
					run, err := reopened.Continue(ctx)
					if err != nil {
						t.Fatal(err)
					}
					waitOK(t, run)
				case "started", "before-control":
					for i := 0; i < 2; i++ {
						if _, err := reopened.Continue(ctx); !errors.Is(err, astral.ErrOutcomeUnknown) {
							t.Fatalf("unknown execution was not blocked: %v", err)
						}
					}
					unknown := reopened.UnresolvedTools()
					if len(unknown) != 1 || unknown[0].Result != nil || unknown[0].Control != nil || !unknown[0].NeedsResolution {
						t.Fatalf("not an internal unresolved record: %+v", unknown)
					}
					count := 0
					for _, e := range entries(reopened) {
						if e.Kind == sessions.EntryToolResult {
							t.Fatal("recovery synthesized an ordinary result")
						}
						if e.Kind == sessions.EntryToolUnresolved {
							count++
						}
					}
					if count != 1 {
						t.Fatalf("repeated recovery appended %d unresolved facts", count)
					}
					if err := reopened.ResolveTool(ctx, unknown[0].Invocation.ExecutionID, astral.ToolExecution{EndRun: &astral.RunEnd{Reason: "host_stop"}}); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := reopened.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
					t.Fatalf("completed control was continuable: %v", err)
				}
				state := reopened.State()
				if state.LastRun == nil || state.LastRun.Status != "completed" || state.LastRun.EndReason != "host_stop" || len(reopened.UnresolvedTools()) != 0 {
					t.Fatalf("terminal state was overwritten: %+v", state.LastRun)
				}
				wantEffects := int32(1)
				if stage == "started" {
					wantEffects = 0
				}
				if effects.Load() != wantEffects || len(m.observed()) != 1 {
					t.Fatalf("recovery replayed work: effects=%d requests=%d", effects.Load(), len(m.observed()))
				}
				checkControlHistory(t, reopened, "host_stop")
				view, err := reopened.InspectContext(ctx)
				if err != nil {
					t.Fatal(err)
				}
				checkControlProjection(t, view.Request.Messages)
			})
		})
	}
}

func TestUnresolvedControlCanBeResolvedToOrdinaryResult(t *testing.T) {
	eachStore(t, func(t *testing.T, base sessions.Store) {
		ctx := context.Background()
		st := &controlFaultStore{Store: base, stage: "before-control"}
		var effects atomic.Int32
		stop := controlTool("host_decision", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
			effects.Add(1)
			return astral.ToolExecution{EndRun: &astral.RunEnd{Reason: "unconfirmed"}}, nil
		})
		m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			if n == 1 {
				return controlReply(stop.Spec.Name), nil
			}
			return answer("continued with host result"), nil
		})
		c := testConfig(m, st)
		c.Tools = []astral.Tool{stop}
		s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
		if _, err := wait(t, start(t, s, "go")); err == nil {
			t.Fatal("commit failure hidden")
		}
		if _, err := s.Continue(ctx); !errors.Is(err, astral.ErrOutcomeUnknown) {
			t.Fatal(err)
		}
		unknown := s.UnresolvedTools()
		if len(unknown) != 1 {
			t.Fatal("missing unresolved execution")
		}
		id := unknown[0].Invocation.ExecutionID
		if err := s.ResolveTool(ctx, id, astral.ToolExecution{}); !errors.Is(err, astral.ErrInvalid) {
			t.Fatalf("invalid resolution accepted: %v", err)
		}
		if err := s.ResolveTool(ctx, id, astral.ToolExecution{Result: &astral.ToolResult{Outcome: astral.OutcomeSucceeded, Content: astral.Text("host reconciled")}}); err != nil {
			t.Fatal(err)
		}
		run, err := s.Continue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		result := waitOK(t, run)
		if effects.Load() != 1 || len(m.observed()) != 2 || result.EndReason != "" {
			t.Fatalf("resolution replayed or ended work: %+v", result)
		}
		req := m.observed()[1]
		if got := texts(req.Messages, astral.RoleTool); len(got) != 1 || got[0] != "host reconciled" {
			t.Fatalf("host result missing: %v", got)
		}
		for _, e := range entries(s) {
			if e.Kind == sessions.EntryToolControl || e.Kind == sessions.EntryToolResult {
				t.Fatalf("unexpected completion fact: %s", e.Kind)
			}
		}
	})
}

func TestCompletedToolControlSurvivesColdStoreReopen(t *testing.T) {
	for _, backend := range []string{"jsonl", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			st, closeStore := durableStore(t, backend, dir)
			var effects atomic.Int32
			stop := controlTool("no_reply", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
				effects.Add(1)
				return astral.ToolExecution{EndRun: &astral.RunEnd{Reason: "nothing_to_add"}}, nil
			})
			m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
				if n == 1 {
					return controlReply(stop.Spec.Name), nil
				}
				return answer("next input"), nil
			})
			c := testConfig(m, st)
			c.Tools = []astral.Tool{stop}
			a := newAgent(t, c)
			s := newSession(t, a, sessions.SessionOptions{})
			waitOK(t, start(t, s, "initial"))
			if err := a.Close(ctx); err != nil {
				t.Fatal(err)
			}
			closeStore()
			c.Store, closeStore = durableStore(t, backend, dir)
			defer closeStore()
			reopened, err := newAgent(t, c).OpenSession(ctx, s.ID())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reopened.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
				t.Fatalf("reopen continued control: %v", err)
			}
			checkControlHistory(t, reopened, "nothing_to_add")
			waitOK(t, start(t, reopened, "new input"))
			checkControlProjection(t, m.observed()[1].Messages)
			if effects.Load() != 1 || len(m.observed()) != 2 {
				t.Fatal("cold reopen replayed a completed control")
			}
		})
	}
}
