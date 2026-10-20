package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	astral "github.com/oines/astral-core"
	sessions "github.com/oines/astral-core/session"
)

func eachControlEngine(t *testing.T, test func(*testing.T, sessions.Store)) {
	t.Helper()
	t.Run("standalone", func(t *testing.T) { test(t, nil) })
	eachStore(t, test)
}

func controlEndpoint(t *testing.T, store sessions.Store, c astral.Config) (contextEndpoint, *sessions.Session) {
	t.Helper()
	c.Retry.MaxAttempts = 1
	if store == nil {
		a, err := astral.New(c)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close(context.Background()) })
		return a, nil
	}
	config := testConfig(c.Model, store)
	config.Tools, config.AfterToolCall, config.FinishTurn = c.Tools, c.AfterToolCall, c.FinishTurn
	if c.BeforeToolCall != nil {
		config.BeforeToolCall = func(ctx context.Context, tc sessions.ToolContext) (astral.ToolDecision, error) {
			return c.BeforeToolCall(ctx, tc.Invocation)
		}
	}
	s := newSession(t, newAgent(t, config), sessions.SessionOptions{})
	return s, s
}

func controlTool(name string, fn func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error)) astral.ToolFunc {
	return astral.ToolFunc{
		Spec: astral.ToolDefinition{Name: name, Version: "1", MayEndRun: true, Parameters: json.RawMessage(`{"type":"object","additionalProperties":false}`)},
		Run:  fn,
	}
}

func controlReply(name string) astral.Reply {
	reply := calls(call("control-call", name, `{}`))
	reply.Message.Content = append([]astral.Content{{Type: "reasoning", Text: "private control reasoning", Data: json.RawMessage(`{"encrypted_content":"opaque"}`)}}, reply.Message.Content...)
	return reply
}

func checkControlProjection(t *testing.T, messages []astral.Message) {
	t.Helper()
	for _, message := range messages {
		if len(message.ToolCalls()) > 0 || message.Role == astral.RoleTool {
			t.Fatalf("control leaked into provider protocol: %+v", message)
		}
		for _, part := range message.Content {
			if part.Type == "reasoning" || part.Text == "private control reasoning" {
				t.Fatalf("control reasoning leaked into request: %+v", part)
			}
		}
	}
}

func checkControlHistory(t *testing.T, s *sessions.Session, reason string) string {
	t.Helper()
	all := entries(s)
	controlID := ""
	for i, e := range all {
		if e.Kind == sessions.EntryToolResult {
			t.Fatal("control created an ordinary ToolResult")
		}
		if e.Kind != sessions.EntryToolControl {
			continue
		}
		if controlID != "" || e.Tool.Result != nil || e.Tool.Control == nil || e.Tool.Control.Reason != reason {
			t.Fatalf("invalid control fact: %+v", e)
		}
		controlID = e.ID
		if i+2 >= len(all) || all[i+1].Kind != sessions.EntryTurn || all[i+2].Kind != sessions.EntryRun ||
			all[i+1].Version != e.Version || all[i+2].Version != e.Version ||
			all[i+2].Run.Status != "completed" || all[i+2].Run.EndReason != reason {
			t.Fatal("tool completion and Run termination were not one atomic commit")
		}
	}
	if controlID == "" {
		t.Fatal("missing durable control fact")
	}
	return controlID
}

func TestToolEndRunPreservesInputsAndSkipsResultHooks(t *testing.T) {
	for _, mode := range []string{"empty", "passive", "driving"} {
		t.Run(mode, func(t *testing.T) {
			queued, passive := mode == "driving", mode != "empty"
			eachControlEngine(t, func(t *testing.T, store sessions.Store) {
				ctx := context.Background()
				entered, release := make(chan struct{}), make(chan struct{})
				var effects, after, finish atomic.Int32
				stop := controlTool("host_decides_to_stop", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
					effects.Add(1)
					close(entered)
					<-release
					return astral.ToolExecution{EndRun: &astral.RunEnd{Reason: "host_choice"}}, nil
				})
				m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
					if n == 1 {
						return controlReply(stop.Spec.Name), nil
					}
					return answer("next activation"), nil
				})
				e, s := controlEndpoint(t, store, astral.Config{Model: m, Tools: []astral.Tool{stop},
					AfterToolCall: func(context.Context, astral.Invocation, astral.ToolResult) (astral.ToolResultPatch, error) {
						after.Add(1)
						return astral.ToolResultPatch{}, nil
					},
					FinishTurn: func(context.Context, astral.TurnContext) (astral.TurnDecision, error) {
						finish.Add(1)
						return astral.TurnDefault, nil
					}})
				sub := e.Subscribe(256)
				defer sub.Close()
				run, err := e.Start(ctx, astral.User("initial"))
				if err != nil {
					t.Fatal(err)
				}
				signal(t, entered)
				if queued {
					if _, err = e.Steer(ctx, astral.User("steer")); err != nil {
						t.Fatal(err)
					}
					if _, err = e.FollowUp(ctx, astral.User("follow up")); err != nil {
						t.Fatal(err)
					}
				}
				if passive {
					if _, err = e.InjectContext(ctx, []astral.Message{astral.User("passive observation")}); err != nil {
						t.Fatal(err)
					}
				}
				close(release)
				result := waitOK(t, run)
				if result.Status != "completed" || result.EndReason != "host_choice" || len(m.observed()) != 1 || effects.Load() != 1 || after.Load() != 0 || finish.Load() != 0 {
					t.Fatalf("incorrect terminal path: result=%+v models=%d effects=%d after=%d finish=%d", result, len(m.observed()), effects.Load(), after.Load(), finish.Load())
				}
				var terminal []astral.EventType
				for len(sub.C) > 0 {
					event := <-sub.C
					if event.Type == astral.EventContextApplied {
						t.Fatal("terminal path drained passive input")
					}
					if event.Type == astral.EventToolEnd || event.Type == astral.EventTurnEnd || event.Type == astral.EventRunEnd {
						terminal = append(terminal, event.Type)
					}
					if event.Type == astral.EventRunEnd && (event.Status != "completed" || event.EndReason != "host_choice" || event.Error != "") {
						t.Fatalf("wrong RunEnd event: %+v", event)
					}
				}
				if !reflect.DeepEqual(terminal, []astral.EventType{astral.EventToolEnd, astral.EventTurnEnd, astral.EventRunEnd}) {
					t.Fatalf("terminal events: %v", terminal)
				}
				pending, err := e.PeekQueuedInputs(ctx)
				if err != nil || len(pending) != map[bool]int{false: 0, true: 2}[queued] {
					t.Fatalf("driving queue changed: %+v %v", pending, err)
				}
				if s != nil {
					checkControlHistory(t, s, "host_choice")
					if passive && s.State().PendingInputs[sessions.InputContext] != 1 {
						t.Fatal("passive queue changed")
					}
					view, err := s.InspectContext(ctx)
					if err != nil {
						t.Fatal(err)
					}
					checkControlProjection(t, view.Request.Messages)
				}
				if queued {
					run, err = e.Continue(ctx)
					if err != nil {
						t.Fatal(err)
					}
					next := waitOK(t, run)
					if next.RunID == result.RunID || next.EndReason != "" || len(m.observed()) != 3 {
						t.Fatalf("queued activation did not start a new Run: %+v", next)
					}
					if !hasMessage(m.observed()[1].Messages, astral.User("passive observation")) {
						t.Fatal("passive observation was lost")
					}
				}
				if _, err = e.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
					t.Fatalf("empty continuation: %v", err)
				}
				run, err = e.Start(ctx, astral.User("new prompt"))
				if err != nil {
					t.Fatal(err)
				}
				waitOK(t, run)
				if passive && !hasMessage(m.observed()[1].Messages, astral.User("passive observation")) {
					t.Fatal("next activation lost pending passive context")
				}
				for _, request := range m.observed()[1:] {
					checkControlProjection(t, request.Messages)
				}
				if effects.Load() != 1 {
					t.Fatal("completed control executed again")
				}
			})
		})
	}
}

func TestControlBatchValidationPrecedesEveryToolEffect(t *testing.T) {
	for _, kind := range []string{"control-first", "control-last", "two-controls", "text", "image", "extension", "exclusive"} {
		t.Run(kind, func(t *testing.T) {
			eachControlEngine(t, func(t *testing.T, store sessions.Store) {
				var effects, before atomic.Int32
				stop := controlTool("halt", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
					effects.Add(1)
					return astral.ToolExecution{EndRun: &astral.RunEnd{}}, nil
				})
				work := echoTool(&effects)
				decl := work.Definition()
				exclusive := astral.ToolFunc{Spec: decl, Run: func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
					effects.Add(1)
					return astral.ToolExecution{Result: &astral.ToolResult{}}, nil
				}}
				exclusive.Spec.Name, exclusive.Spec.Exclusive = "exclusive", true
				reply := controlReply("halt")
				switch kind {
				case "control-first":
					reply = calls(call("halt", "halt", `{}`), call("work", "echo", `{"value":"x"}`))
				case "control-last":
					reply = calls(call("work", "echo", `{"value":"x"}`), call("halt", "halt", `{}`))
				case "two-controls":
					reply = calls(call("one", "halt", `{}`), call("two", "halt", `{}`))
				case "exclusive":
					reply = calls(call("one", "exclusive", `{"value":"x"}`), call("two", "echo", `{"value":"x"}`))
				default:
					part := astral.Content{Type: kind, Text: "visible output"}
					if kind == "extension" {
						part.Type = "provider_extension"
					}
					reply.Message.Content = append(reply.Message.Content, part)
				}
				m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
					return reply, nil
				})
				e, s := controlEndpoint(t, store, astral.Config{Model: m, Tools: []astral.Tool{stop, work, exclusive},
					BeforeToolCall: func(context.Context, astral.Invocation) (astral.ToolDecision, error) {
						before.Add(1)
						return astral.ToolDecision{}, nil
					}})
				run, err := e.Start(context.Background(), astral.User("work"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err = wait(t, run); !errors.Is(err, astral.ErrInvalid) || effects.Load() != 0 || before.Load() != 0 {
					t.Fatalf("invalid batch had effects: err=%v effects=%d before=%d", err, effects.Load(), before.Load())
				}
				if s != nil {
					for _, entry := range entries(s) {
						if entry.Kind == sessions.EntryMessage && entry.Message.Role == astral.RoleAssistant && !entry.Message.Failed() {
							t.Fatal("invalid reply committed as a valid assistant")
						}
					}
				}
			})
		})
	}
}

func TestControlReturnValidationAndOrdinaryResultFallback(t *testing.T) {
	for _, kind := range []string{"empty", "both", "undeclared", "error", "result"} {
		t.Run(kind, func(t *testing.T) {
			eachControlEngine(t, func(t *testing.T, store sessions.Store) {
				stop := controlTool("finish_if_ready", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
					switch kind {
					case "empty":
						return astral.ToolExecution{}, nil
					case "both":
						return astral.ToolExecution{Result: &astral.ToolResult{}, EndRun: &astral.RunEnd{}}, nil
					case "error":
						return astral.ToolExecution{EndRun: &astral.RunEnd{Reason: "must_not_win"}}, errors.New("execution failed")
					case "result":
						return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text("not ready")}}, nil
					default:
						return astral.ToolExecution{EndRun: &astral.RunEnd{Reason: "must_not_win"}}, nil
					}
				})
				if kind == "undeclared" {
					stop.Spec.MayEndRun = false
				}
				m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
					if n == 1 {
						return controlReply(stop.Spec.Name), nil
					}
					return answer("ordinary completion"), nil
				})
				e, s := controlEndpoint(t, store, astral.Config{Model: m, Tools: []astral.Tool{stop}})
				run, err := e.Start(context.Background(), astral.User("work"))
				if err != nil {
					t.Fatal(err)
				}
				result, err := wait(t, run)
				normal := kind == "error" || kind == "result"
				if normal && (err != nil || len(m.observed()) != 2) || !normal && !errors.Is(err, astral.ErrOutcomeUnknown) || result.EndReason != "" {
					t.Fatalf("invalid execution became EndRun: %+v %v", result, err)
				}
				if s != nil {
					for _, entry := range entries(s) {
						if entry.Kind == sessions.EntryToolControl {
							t.Fatal("non-control return created a control fact")
						}
					}
				}
			})
		})
	}
}
