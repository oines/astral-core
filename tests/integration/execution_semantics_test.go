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

func TestContinueAdmissionIgnoresPassiveRolesAndDrivesQueuedInput(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprintf("persistent=%v", persistent), func(t *testing.T) {
			ctx := context.Background()
			m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
				return answer("done"), nil
			})
			e := harness(t, persistent, m, nil, nil)
			if _, err := e.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
				t.Fatal(err)
			}
			if _, err := inject(e, ctx, []astral.Message{{Role: astral.RoleDeveloper, Content: astral.Text("context")}, astral.User("observation")}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
				t.Fatal("passive user role became executable input")
			}
			if _, err := followUp(e, ctx, astral.User("queued")); err != nil {
				t.Fatal(err)
			}
			if len(m.observed()) != 0 {
				t.Fatal("enqueue activated the model")
			}
			run, err := e.Continue(ctx)
			if err != nil {
				t.Fatal(err)
			}
			waitOK(t, run)
			if len(m.observed()) != 1 {
				t.Fatal("queued first input was not processed")
			}
			if _, err := e.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
				t.Fatal("completed assistant created a no-op Run")
			}
		})
	}
}

func TestParallelPreflightPinsTargetsBeforeAnyDispatch(t *testing.T) {
	for _, sequential := range []bool{false, true} {
		t.Run(fmt.Sprintf("sequential=%v", sequential), func(t *testing.T) {
			eachStore(t, func(t *testing.T, store astral.Store) {
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
				c.BeforeToolCall = func(_ context.Context, _ astral.ToolContext) (astral.ToolDecision, error) {
					preflights.Add(1)
					if !sequential && effects.Load() != 0 {
						return astral.ToolDecision{}, errors.New("parallel effect started before all preflights")
					}
					return astral.ToolDecision{}, nil
				}
				s := newSession(t, newSessions(t, c), astral.SessionOptions{})
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
	eachStore(t, func(t *testing.T, store astral.Store) {
		ctx := context.Background()
		fault := &faultStore{Store: store, kind: astral.EntryToolStarted}
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
		a := newSessions(t, c)
		s := newSession(t, a, astral.SessionOptions{})
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
		opened, err := newSessions(t, c).OpenSession(ctx, s.ID())
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
				run, err := prompt(e, ctx, astral.User("go"))
				if err != nil {
					t.Fatal(err)
				}
				signal(t, entered)
				if cancelled {
					run.Cancel()
				}
				result, err := wait(t, run)
				if err == nil || result.LastMessage != nil {
					t.Fatal("terminal snapshot missing")
				}
				counts := map[astral.EventType]int{}
				var sequence []astral.EventType
				for len(sub.C) > 0 {
					event := <-sub.C
					counts[event.Type]++
					if event.Type == astral.EventMessageEnd && (event.Message != nil || event.EntryID != "" || event.Error == "") {
						t.Fatal("failed stream became a conversation message")
					}
					if event.Type == astral.EventRunStart || event.Type == astral.EventRunEnd || event.Type == astral.EventTurnStart || event.Type == astral.EventTurnEnd || event.Type == astral.EventMessageStart || event.Type == astral.EventMessageEnd {
						sequence = append(sequence, event.Type)
					}
				}
				want := []astral.EventType{astral.EventRunStart, astral.EventTurnStart, astral.EventMessageStart, astral.EventMessageEnd, astral.EventTurnEnd, astral.EventRunEnd}
				if !reflect.DeepEqual(sequence, want) || counts[astral.EventAttemptStart] != 1 {
					t.Fatalf("unbalanced logical lifecycle: %v", sequence)
				}
				if len(m.observed()) != 1 {
					t.Fatal("failed request automatically restarted")
				}
			})
		}
	}
}

type cancelStartStore struct {
	astral.Store
	cancel context.CancelFunc
	fired  atomic.Bool
}

func (s *cancelStartStore) Commit(ctx context.Context, id string, c astral.Commit) (astral.Receipt, error) {
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
func TestRequestCancellationAtAcceptanceDoesNotCancelTheRun(t *testing.T) {
	eachStore(t, func(t *testing.T, store astral.Store) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fault := &cancelStartStore{Store: store, cancel: cancel}
		m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
			return answer("accepted"), nil
		})
		s := newSession(t, newSessions(t, testConfig(m, fault)), astral.SessionOptions{})
		result, err := s.Prompt(ctx, astral.Input{ID: "accepted", Message: astral.User("finish despite disconnected caller")})
		if err != nil {
			t.Fatal(err)
		}
		if ctx.Err() == nil {
			t.Fatal("injection did not cancel the request")
		}
		waitOK(t, result.Run)
		receipt, err := s.LookupInput(context.Background(), "accepted")
		if err != nil || receipt.State != astral.InputApplied || len(m.observed()) != 1 {
			t.Fatalf("accepted work lost: %+v %v", receipt, err)
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
			run, err := prompt(e, ctx, astral.User("wait"))
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

func TestBridgeTerminalStatusRemainsInAttemptAudit(t *testing.T) {
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
				run, err := prompt(e, context.Background(), astral.User("go"))
				if err != nil {
					t.Fatal(err)
				}
				result, err := wait(t, run)
				if err == nil || result.LastMessage != nil {
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
	eachStore(t, func(t *testing.T, store astral.Store) {
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
				c.BeforeToolCall = func(_ context.Context, inv astral.ToolContext) (astral.ToolDecision, error) {
					if !reflect.DeepEqual(inv.Binding, want) {
						return astral.ToolDecision{}, errors.New("approval received changed target")
					}
					if len(inv.Binding) > 0 {
						inv.Binding[0] = '!'
					}
					return astral.ToolDecision{}, nil
				}
				s := newSession(t, newSessions(t, c), astral.SessionOptions{})
				waitOK(t, start(t, s, "work"))
				if _, err := s.Compact(context.Background(), ""); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, entry := range entries(s) {
					if entry.Kind == astral.EntryToolPrepared {
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
			run, err := prompt(e, context.Background(), astral.User("task"))
			if err != nil {
				t.Fatal(err)
			}
			result, err := wait(t, run)
			if !errors.Is(err, astral.ErrInvalid) || result.LastMessage != nil {
				t.Fatalf("invalid length stream was adopted: %+v %v", result, err)
			}
		})
	}
}
