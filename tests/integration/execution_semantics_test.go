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
	sessions "github.com/oines/astral-core/session"
)

func TestContinueAdmissionIgnoresPassiveRolesAndEmptyQueues(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprintf("persistent=%v", persistent), func(t *testing.T) {
			ctx := context.Background()
			m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
				return answer("done"), nil
			})
			e := harness(t, persistent, m, nil, nil)
			sub := e.Subscribe(64)
			defer sub.Close()
			if _, err := e.FollowUp(ctx, astral.User("queued")); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
				t.Fatal("empty history accepted queued-only Continue")
			}
			if _, err := e.InjectContext(ctx, []astral.Message{{Role: astral.RoleDeveloper, Content: astral.Text("context")}, astral.User("observation")}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
				t.Fatal("passive user role became executable input")
			}
			q, err := e.PeekQueuedInputs(ctx)
			if err != nil || len(q) != 1 || len(m.observed()) != 0 {
				t.Fatal("invalid Continue consumed input")
			}
			for len(sub.C) > 0 {
				if event := <-sub.C; event.Type == astral.EventRunStart {
					t.Fatal("invalid Continue created a Run")
				}
			}
			if err := e.ClearFollowUps(ctx); err != nil {
				t.Fatal(err)
			}
			run, err := e.Start(ctx, astral.User("real input"))
			if err != nil {
				t.Fatal(err)
			}
			waitOK(t, run)
			if _, err := e.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
				t.Fatal("completed assistant created a no-op Run")
			}
		})
	}
}

func TestFinishTurnDecisionsControlOnlyExecution(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		for _, test := range []struct {
			name     string
			decision astral.TurnDecision
			queued   bool
			fail     bool
			want     int
		}{
			{"end", astral.TurnEnd, true, false, 1}, {"continue-with-queue", astral.TurnContinue, true, false, 2}, {"continue-once", astral.TurnContinue, false, false, 2}, {"default", astral.TurnDefault, true, false, 2}, {"hook-error", astral.TurnDefault, true, true, 1},
		} {
			t.Run(fmt.Sprintf("persistent=%v/%s", persistent, test.name), func(t *testing.T) {
				ctx := context.Background()
				entered, release := make(chan struct{}), make(chan struct{})
				var hooks atomic.Int32
				m := model(func(ctx context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
					if n == 1 {
						close(entered)
						select {
						case <-release:
						case <-ctx.Done():
							return astral.Reply{}, ctx.Err()
						}
					}
					return answer(fmt.Sprintf("reply-%d", n)), nil
				})
				finish := func(_ context.Context, turn astral.TurnContext) (astral.TurnDecision, error) {
					if turn.TurnID == "" || turn.Assistant.Role != astral.RoleAssistant {
						return "", errors.New("missing turn identity")
					}
					if hooks.Add(1) == 1 {
						if test.fail {
							return "", errors.New("hook failure")
						}
						return test.decision, nil
					}
					return astral.TurnDefault, nil
				}
				e := harness(t, persistent, m, nil, finish)
				run, err := e.Start(ctx, astral.User("task"))
				if err != nil {
					t.Fatal(err)
				}
				signal(t, entered)
				if test.queued {
					if _, err := e.FollowUp(ctx, astral.User("next")); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := e.InjectContext(ctx, []astral.Message{astral.User("passive")}); err != nil {
					t.Fatal(err)
				}
				close(release)
				_, err = wait(t, run)
				if (err != nil) != test.fail || len(m.observed()) != test.want {
					t.Fatalf("decision requested wrong execution: calls=%d err=%v", len(m.observed()), err)
				}
				pending, err := e.HasQueuedInputs(ctx)
				if err != nil || pending != (test.decision == astral.TurnEnd || test.fail) {
					t.Fatal("decision consumed or retained the wrong queue")
				}
				if pending {
					run, err = e.Continue(ctx)
					if err != nil {
						t.Fatal(err)
					}
					waitOK(t, run)
					if len(m.observed()) != 2 {
						t.Fatal("explicit Continue did not resume queued work")
					}
				}
				if _, err = e.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
					t.Fatal(err)
				}
			})
		}
	}
}

type interruptedTurnStore struct {
	sessions.Store
	fired atomic.Bool
}

func (s *interruptedTurnStore) Commit(ctx context.Context, id string, c sessions.Commit) (sessions.Receipt, error) {
	fail := false
	for _, e := range c.Entries {
		if e.Turn != nil && e.Turn.Decided && e.Turn.Decision == astral.TurnContinue && !s.fired.Swap(true) {
			fail = true
		}
	}
	receipt, err := s.Store.Commit(ctx, id, c)
	if err == nil && fail {
		return sessions.Receipt{}, errors.New("decision committed but acknowledgement lost")
	}
	return receipt, err
}
func (s *interruptedTurnStore) LookupCommit(ctx context.Context, id, key string) (sessions.Receipt, error) {
	if s.fired.Load() {
		return sessions.Receipt{}, errors.New("confirmation unavailable")
	}
	return s.Store.LookupCommit(ctx, id, key)
}
func TestConfirmedFinishTurnDecisionIsNotRecomputed(t *testing.T) {
	eachStore(t, func(t *testing.T, store sessions.Store) {
		ctx := context.Background()
		fault := &interruptedTurnStore{Store: store}
		var hooks atomic.Int32
		m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
			return answer("answer"), nil
		})
		c := testConfig(m, fault)
		c.FinishTurn = func(context.Context, astral.TurnContext) (astral.TurnDecision, error) {
			if hooks.Add(1) == 1 {
				return astral.TurnContinue, nil
			}
			return astral.TurnDefault, nil
		}
		a := newAgent(t, c)
		s := newSession(t, a, sessions.SessionOptions{})
		if _, err := wait(t, start(t, s, "work")); err == nil {
			t.Fatal("interruption was hidden")
		}
		if err := a.Close(ctx); err != nil {
			t.Fatal(err)
		}
		c.Store = store
		opened, err := newAgent(t, c).OpenSession(ctx, s.ID())
		if err != nil {
			t.Fatal(err)
		}
		run, err := opened.Continue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		waitOK(t, run)
		if len(m.observed()) != 2 || hooks.Load() != 2 {
			t.Fatalf("confirmed decision repeated or continue ran twice: models=%d hooks=%d", len(m.observed()), hooks.Load())
		}
	})
}

func TestParallelPreflightPinsTargetsBeforeAnyDispatch(t *testing.T) {
	for _, sequential := range []bool{false, true} {
		t.Run(fmt.Sprintf("sequential=%v", sequential), func(t *testing.T) {
			eachStore(t, func(t *testing.T, store sessions.Store) {
				var target atomic.Value
				target.Store("target-a")
				var effects, preflights atomic.Int32
				bound := make(chan string, 2)
				base := astral.ToolFunc{Spec: astral.ToolDefinition{Name: "work", Version: "1", Sequential: sequential, Parameters: json.RawMessage(valueSchema)}, Run: func(_ context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolExecution, error) {
					effects.Add(1)
					bound <- inv.Call.ID + ":" + bindingOf(inv.Binding).Target
					if inv.Call.ID == "first" {
						target.Store("target-b")
					}
					return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text("done")}}, nil
				}}
				wrapped := hostTargetTool{Tool: base, target: func() json.RawMessage {
					raw, _ := json.Marshal(hostBinding{Target: target.Load().(string)})
					return raw
				}}
				m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
					if n == 1 {
						return calls(call("first", "work", `{"value":"a"}`), call("second", "work", `{"value":"b"}`)), nil
					}
					return answer("done"), nil
				})
				c := testConfig(m, store)
				c.Tools = []astral.Tool{wrapped}
				c.BeforeToolCall = func(_ context.Context, _ sessions.ToolContext) (astral.ToolDecision, error) {
					preflights.Add(1)
					if !sequential && effects.Load() != 0 {
						return astral.ToolDecision{}, errors.New("parallel effect started before all preflights")
					}
					return astral.ToolDecision{}, nil
				}
				s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
				waitOK(t, start(t, s, "work"))
				got := map[string]bool{<-bound: true, <-bound: true}
				second := "target-a"
				if sequential {
					second = "target-b"
				}
				if preflights.Load() != 2 || !got["first:target-a"] || !got["second:"+second] {
					t.Fatalf("target selection escaped the preflight boundary: %+v", got)
				}
			})
		})
	}
}

func TestPreparedInvocationKeepsOriginalOpaqueTargetAfterReopen(t *testing.T) {
	eachStore(t, func(t *testing.T, store sessions.Store) {
		ctx := context.Background()
		fault := &faultStore{Store: store, kind: sessions.EntryToolStarted}
		var binds, effects atomic.Int32
		target := json.RawMessage(`{"target":"original","revision":900719925474099312345}`)
		want := append(json.RawMessage(nil), target...)
		base := tool("work", func(_ context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolExecution, error) {
			effects.Add(1)
			if !reflect.DeepEqual(inv.Binding, want) {
				return astral.ToolExecution{Result: &astral.ToolResult{}}, errors.New("prepared target was rebound")
			}
			return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text("done")}}, nil
		})
		wrapped := hostTargetTool{Tool: base, target: func() json.RawMessage { binds.Add(1); return target }}
		m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			if n == 1 {
				return calls(call("c", "work", `{"value":"x"}`)), nil
			}
			return answer("done"), nil
		})
		c := testConfig(m, fault)
		c.Tools = []astral.Tool{wrapped}
		a := newAgent(t, c)
		s := newSession(t, a, sessions.SessionOptions{})
		if _, err := wait(t, start(t, s, "work")); err == nil {
			t.Fatal("dispatch failure hidden")
		}
		if effects.Load() != 0 {
			t.Fatal("tool executed before Started acknowledgement")
		}
		target[11] = 'X'
		if err := a.Close(ctx); err != nil {
			t.Fatal(err)
		}
		c.Store = store
		opened, err := newAgent(t, c).OpenSession(ctx, s.ID())
		if err != nil {
			t.Fatal(err)
		}
		run, err := opened.Continue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		waitOK(t, run)
		if binds.Load() != 1 || effects.Load() != 1 {
			t.Fatal("prepared binding or external effect was replayed")
		}
	})
}

func TestTerminalEventsCloseLogicalMessagesAndTurns(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		for _, cancelled := range []bool{false, true} {
			t.Run(fmt.Sprintf("persistent=%v/cancel=%v", persistent, cancelled), func(t *testing.T) {
				ctx := context.Background()
				entered := make(chan struct{})
				m := model(func(ctx context.Context, _ int, _ astral.Request, emit func(astral.ModelEvent)) (astral.Reply, error) {
					emit(astral.ModelEvent{Type: "text_delta", ContentIndex: contentIndex(0), Text: "partial"})
					close(entered)
					if cancelled {
						<-ctx.Done()
						return answer("partial"), ctx.Err()
					}
					return answer("partial"), errors.New("final stream failed")
				})
				e := harness(t, persistent, m, nil, nil)
				sub := e.Subscribe(256)
				defer sub.Close()
				run, err := e.Start(ctx, astral.User("go"))
				if err != nil {
					t.Fatal(err)
				}
				signal(t, entered)
				if cancelled {
					run.Cancel()
				}
				result, err := wait(t, run)
				if err == nil || result.LastMessage == nil || !result.LastMessage.Failed() || result.LastMessage.Content[0].Text != "partial" {
					t.Fatal("terminal snapshot missing")
				}
				counts := map[astral.EventType]int{}
				var sequence []astral.EventType
				for len(sub.C) > 0 {
					event := <-sub.C
					counts[event.Type]++
					if event.Type == astral.EventMessageEnd && event.Message == nil {
						t.Fatal("terminal event omitted committed message")
					}
					if event.Type == astral.EventRunStart || event.Type == astral.EventRunEnd || event.Type == astral.EventTurnStart || event.Type == astral.EventTurnEnd || event.Type == astral.EventMessageStart || event.Type == astral.EventMessageEnd {
						sequence = append(sequence, event.Type)
					}
				}
				want := []astral.EventType{astral.EventRunStart, astral.EventTurnStart, astral.EventMessageStart, astral.EventMessageEnd, astral.EventTurnEnd, astral.EventRunEnd}
				if !reflect.DeepEqual(sequence, want) || counts[astral.EventAttemptStart] != 1 {
					t.Fatalf("unbalanced logical lifecycle: %v", sequence)
				}
				if _, err = e.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
					t.Fatal("terminal reply implicitly retried old prompt")
				}
			})
		}
	}
}

func TestFinishTurnEndAfterToolsRequiresNewInput(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprintf("persistent=%v", persistent), func(t *testing.T) {
			ctx := context.Background()
			m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
				return calls(call("c", "echo", `{"value":"x"}`)), nil
			})
			e := harness(t, persistent, m, []astral.Tool{echoTool(nil)}, func(_ context.Context, turn astral.TurnContext) (astral.TurnDecision, error) {
				if len(turn.Results) != 1 || turn.Results[0].Outcome != astral.OutcomeSucceeded {
					return "", errors.New("finish hook missed ordered tool outcomes")
				}
				return astral.TurnEnd, nil
			})
			run, err := e.Start(ctx, astral.User("work"))
			if err != nil {
				t.Fatal(err)
			}
			waitOK(t, run)
			if _, err = e.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
				t.Fatal("ended tool turn resumed without new input")
			}
			if len(m.observed()) != 1 {
				t.Fatal("end decision was ignored")
			}
		})
	}
}

type cancelStartStore struct {
	sessions.Store
	cancel context.CancelFunc
	fired  atomic.Bool
}

func (s *cancelStartStore) Commit(ctx context.Context, id string, c sessions.Commit) (sessions.Receipt, error) {
	r, err := s.Store.Commit(ctx, id, c)
	if err == nil {
		for _, e := range c.Entries {
			if e.Run != nil && e.Run.Status == "started" && !s.fired.Swap(true) {
				s.cancel()
			}
		}
	}
	return r, err
}
func TestCancellationBeforeModelStartDoesNotInventAssistantOrResumeOldPrompt(t *testing.T) {
	eachStore(t, func(t *testing.T, store sessions.Store) {
		ctx := context.Background()
		startCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		fault := &cancelStartStore{Store: store, cancel: cancel}
		m := model(func(_ context.Context, _ int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			users := userMessages(r)
			if len(users) != 2 || users[1].Content[0].Text != "new task" {
				return astral.Reply{}, errors.New("cancelled original prompt resumed before follow-up")
			}
			return answer("done"), nil
		})
		s := newSession(t, newAgent(t, testConfig(m, fault)), sessions.SessionOptions{})
		run, err := s.Start(startCtx, astral.User("cancel before generation"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = wait(t, run); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if len(m.observed()) != 0 {
			t.Fatal("cancelled start called the model")
		}
		for _, e := range entries(s) {
			if e.Message != nil && e.Message.Role == astral.RoleAssistant {
				t.Fatal("pre-model cancellation fabricated assistant")
			}
		}
		if _, err = s.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
			t.Fatal("cancelled start automatically retried")
		}
		if _, err = s.FollowUp(ctx, astral.User("new task")); err != nil {
			t.Fatal(err)
		}
		run, err = s.Continue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		waitOK(t, run)
		if len(m.observed()) != 1 {
			t.Fatal("follow-up required an extra request")
		}
	})
}

func TestClosePublishesTerminalLifecyclesBeforeClosingSubscribers(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprintf("persistent=%v", persistent), func(t *testing.T) {
			ctx := context.Background()
			entered := make(chan struct{})
			m := model(func(ctx context.Context, _ int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
				close(entered)
				<-ctx.Done()
				return astral.Reply{}, ctx.Err()
			})
			e := harness(t, persistent, m, nil, nil)
			sub := e.Subscribe(256)
			run, err := e.Start(ctx, astral.User("wait"))
			if err != nil {
				t.Fatal(err)
			}
			signal(t, entered)
			if err := e.(interface{ Close(context.Context) error }).Close(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := wait(t, run); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			var sequence []astral.EventType
			for event := range sub.C {
				if event.Type == astral.EventMessageEnd || event.Type == astral.EventTurnEnd || event.Type == astral.EventRunEnd {
					sequence = append(sequence, event.Type)
				}
			}
			if !reflect.DeepEqual(sequence, []astral.EventType{astral.EventMessageEnd, astral.EventTurnEnd, astral.EventRunEnd}) || !errors.Is(sub.Err(), astral.ErrClosed) {
				t.Fatalf("Close hid terminal events: %v %v", sequence, sub.Err())
			}
		})
	}
}

func TestBridgeTerminalStatusPreservesItsErrorAndStopsFallback(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		for _, reason := range []astral.FinishReason{astral.FinishError, astral.FinishCancelled} {
			t.Run(fmt.Sprintf("persistent=%v/reason=%s", persistent, reason), func(t *testing.T) {
				m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
					r := answer("partial")
					r.Message.FinishReason = reason
					r.Message.ErrorMessage = "provider terminal detail"
					return r, nil
				})
				e := harness(t, persistent, m, nil, nil)
				run, err := e.Start(context.Background(), astral.User("go"))
				if err != nil {
					t.Fatal(err)
				}
				result, err := wait(t, run)
				if err == nil || result.LastMessage == nil || result.LastMessage.FinishReason != reason || result.LastMessage.ErrorMessage != "provider terminal detail" {
					t.Fatalf("terminal bridge state changed: %+v %v", result, err)
				}
				if reason == astral.FinishCancelled && result.Status != "cancelled" {
					t.Fatal("bridge cancellation became a failed run")
				}
			})
		}
	}
}

func TestOpaqueInvocationTargetsRoundTripWithoutEnteringModelContext(t *testing.T) {
	eachStore(t, func(t *testing.T, store sessions.Store) {
		for i, raw := range []string{"", `null`, `true`, `42`, `"private-target"`, `[1,2]`, `{"target":"private","revision":900719925474099312345}`} {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				var target json.RawMessage
				if raw != "" {
					target = json.RawMessage(raw)
				}
				want := append(json.RawMessage(nil), target...)
				work := hostTargetTool{Tool: tool("work", func(_ context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolExecution, error) {
					if !reflect.DeepEqual(inv.Binding, want) {
						return astral.ToolExecution{Result: &astral.ToolResult{}}, errors.New("opaque target changed")
					}
					return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text("done")}}, nil
				}), target: func() json.RawMessage { return target }}
				m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
					if n == 1 {
						return calls(call("c", "work", `{"value":"x"}`)), nil
					}
					return answer("done"), nil
				})
				c := testConfig(m, store)
				c.Tools = []astral.Tool{work}
				c.Compaction.KeepRecentTokens = 1
				c.BeforeToolCall = func(_ context.Context, inv sessions.ToolContext) (astral.ToolDecision, error) {
					if !reflect.DeepEqual(inv.Binding, want) {
						return astral.ToolDecision{}, errors.New("approval received changed target")
					}
					if len(inv.Binding) > 0 {
						inv.Binding[0] = '!'
					}
					return astral.ToolDecision{}, nil
				}
				s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
				waitOK(t, start(t, s, "work"))
				if _, err := s.Compact(context.Background(), ""); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, entry := range entries(s) {
					if entry.Kind == sessions.EntryToolPrepared {
						found = true
						if !reflect.DeepEqual(entry.Tool.Invocation.Binding, want) {
							t.Fatal("prepared target aliased callback bytes")
						}
					}
				}
				if !found {
					t.Fatal("target was not persisted")
				}
				for _, request := range m.observed() {
					data, err := json.Marshal(request)
					if err != nil {
						t.Fatal(err)
					}
					var object map[string]any
					if err := json.Unmarshal(data, &object); err != nil {
						t.Fatal(err)
					}
					if containsBinding(object) {
						t.Fatal("private invocation target entered model or summarizer request")
					}
				}
			})
		}
	})
}
func containsBinding(value any) bool {
	switch x := value.(type) {
	case map[string]any:
		for key, v := range x {
			if key == "binding" || containsBinding(v) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if containsBinding(v) {
				return true
			}
		}
	}
	return false
}

func TestBridgeCancellationDoesNotInvokeModelFallback(t *testing.T) {
	var fallbacks atomic.Int32
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		r := answer("partial")
		r.Message.FinishReason = astral.FinishCancelled
		return r, nil
	})
	replacement := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("must not run"), nil
	})
	replacement.spec.ID = "replacement"
	a, err := astral.New(astral.Config{Model: m, Fallback: func(context.Context, astral.ModelFailure) (astral.Fallback, error) {
		fallbacks.Add(1)
		return astral.Fallback{ModelID: "replacement"}, nil
	}, ResolveModel: func(context.Context, string) (astral.Model, error) { return replacement, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	run, err := a.Start(context.Background(), astral.User("task"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = wait(t, run); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if fallbacks.Load() != 0 || len(replacement.observed()) != 0 {
		t.Fatal("cancellation caused fallback generation")
	}
}

func TestFinishTurnContinuationSurvivesRequestPreparationFailure(t *testing.T) {
	eachStore(t, func(t *testing.T, store sessions.Store) {
		ctx := context.Background()
		var unavailable atomic.Bool
		unavailable.Store(true)
		var hooks atomic.Int32
		m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
			return answer("done"), nil
		})
		m.estimator = func(r astral.Request) (int, error) {
			for _, message := range r.Messages {
				if message.Role == astral.RoleAssistant && unavailable.Load() {
					return 0, errors.New("estimator unavailable before next request")
				}
			}
			return 100, nil
		}
		c := testConfig(m, store)
		c.FinishTurn = func(context.Context, astral.TurnContext) (astral.TurnDecision, error) {
			if hooks.Add(1) == 1 {
				return astral.TurnContinue, nil
			}
			return astral.TurnDefault, nil
		}
		a := newAgent(t, c)
		s := newSession(t, a, sessions.SessionOptions{})
		if _, err := wait(t, start(t, s, "task")); err == nil {
			t.Fatal("preparation failure was hidden")
		}
		if len(m.observed()) != 1 || s.State().Turn == nil || s.State().Turn.Advanced {
			t.Fatal("preparation consumed the durable continuation")
		}
		if err := a.Close(ctx); err != nil {
			t.Fatal(err)
		}
		unavailable.Store(false)
		opened, err := newAgent(t, c).OpenSession(ctx, s.ID())
		if err != nil {
			t.Fatal(err)
		}
		run, err := opened.Continue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		waitOK(t, run)
		if len(m.observed()) != 2 || hooks.Load() != 2 {
			t.Fatal("recovery repeated the decision or lost its continuation")
		}
	})
}

func TestLengthReplyStillValidatesStreamContentReferences(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprintf("persistent=%v", persistent), func(t *testing.T) {
			m := model(func(_ context.Context, _ int, _ astral.Request, emit func(astral.ModelEvent)) (astral.Reply, error) {
				emit(astral.ModelEvent{Type: "text_delta", ContentIndex: contentIndex(7), Text: "partial"})
				r := answer("partial")
				r.Message.FinishReason = astral.FinishLength
				return r, nil
			})
			e := harness(t, persistent, m, nil, nil)
			run, err := e.Start(context.Background(), astral.User("task"))
			if err != nil {
				t.Fatal(err)
			}
			result, err := wait(t, run)
			if !errors.Is(err, astral.ErrInvalid) || result.LastMessage == nil || result.LastMessage.FinishReason != astral.FinishError {
				t.Fatalf("invalid length stream was adopted: %+v %v", result, err)
			}
		})
	}
}
