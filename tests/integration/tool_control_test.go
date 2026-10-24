package integration_test

import (
	"context"
	"encoding/json"
	"errors"

	"sync/atomic"
	"testing"

	astral "github.com/oines/astral-core"

	"github.com/oines/astral-core/store/memory"
)

func eachControlEngine(t *testing.T, test func(*testing.T, astral.Store)) {
	t.Helper()
	eachStore(t, test)
}
func controlEndpoint(t *testing.T, store astral.Store, c astral.Config) (contextEndpoint, *astral.Session) {
	t.Helper()
	if store == nil {
		store = memory.New()
	}
	config := testConfig(c.Model, store)
	config.Tools, config.AfterToolCall, config.BeforeToolCall = c.Tools, c.AfterToolCall, c.BeforeToolCall
	s := newSession(t, newSessions(t, config), astral.SessionOptions{})
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

func checkControlHistory(t *testing.T, s *astral.Session, reason string) string {
	t.Helper()
	all := entries(s)
	controlID := ""
	for i, e := range all {
		if e.Kind == astral.EntryToolResult {
			t.Fatal("control created an ordinary ToolResult")
		}
		if e.Kind != astral.EntryToolControl {
			continue
		}
		if controlID != "" || e.Tool.Result != nil || e.Tool.Control == nil || e.Tool.Control.Reason != reason {
			t.Fatalf("invalid control fact: %+v", e)
		}
		controlID = e.ID
		if i+1 >= len(all) || all[i+1].Kind != astral.EntryRun || all[i+1].Version != e.Version || all[i+1].Run.Status != "completed" || all[i+1].Run.EndReason != reason {
			t.Fatal("tool completion and activation termination were not atomic")
		}

	}
	if controlID == "" {
		t.Fatal("missing durable control fact")
	}
	return controlID
}

func TestToolEndRunDrainsIndependentInputsAndSkipsResultHooks(t *testing.T) {
	eachStore(t, func(t *testing.T, store astral.Store) {
		ctx := context.Background()
		entered, release := make(chan struct{}), make(chan struct{})
		var effects, after atomic.Int32
		stop := controlTool("no_reply", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
			effects.Add(1)
			close(entered)
			<-release
			return astral.ToolExecution{EndRun: &astral.RunEnd{Reason: "no_reply"}}, nil
		})
		m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			if n == 1 {
				return controlReply(stop.Spec.Name), nil
			}
			return answer("next activation"), nil
		})
		e, s := controlEndpoint(t, store, astral.Config{Model: m, Tools: []astral.Tool{stop}, AfterToolCall: func(context.Context, astral.Invocation, astral.ToolResult) (astral.ToolResultPatch, error) {
			after.Add(1)
			return astral.ToolResultPatch{}, nil
		}})
		run, err := prompt(e, ctx, astral.User("initial"))
		if err != nil {
			t.Fatal(err)
		}
		signal(t, entered)
		if _, err = steer(e, ctx, astral.User("steer")); err != nil {
			t.Fatal(err)
		}
		if _, err = followUp(e, ctx, astral.User("follow up")); err != nil {
			t.Fatal(err)
		}
		close(release)
		result := waitOK(t, run)
		waitSessionIdle(t, s)
		if result.Status != "completed" || result.EndReason != "no_reply" || len(m.observed()) != 3 || effects.Load() != 1 || after.Load() != 0 {
			t.Fatalf("terminal/queued path: %+v models=%d effects=%d hooks=%d", result, len(m.observed()), effects.Load(), after.Load())
		}
		checkControlHistory(t, s, "no_reply")
		for _, request := range m.observed()[1:] {
			checkControlProjection(t, request.Messages)
		}
		if _, err := s.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
			t.Fatal(err)
		}
	})
}

func TestControlBatchValidationPrecedesEveryToolEffect(t *testing.T) {
	for _, kind := range []string{"control-first", "control-last", "two-controls", "text", "image", "extension", "exclusive"} {
		t.Run(kind, func(t *testing.T) {
			eachControlEngine(t, func(t *testing.T, store astral.Store) {
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
					BeforeToolCall: func(context.Context, astral.ToolContext) (astral.ToolDecision, error) {
						before.Add(1)
						return astral.ToolDecision{}, nil
					}})
				run, err := prompt(e, context.Background(), astral.User("work"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err = wait(t, run); !errors.Is(err, astral.ErrInvalid) || effects.Load() != 0 || before.Load() != 0 {
					t.Fatalf("invalid batch had effects: err=%v effects=%d before=%d", err, effects.Load(), before.Load())
				}
				if s != nil {
					for _, entry := range entries(s) {
						if entry.Kind == astral.EntryMessage && entry.Message.Role == astral.RoleAssistant && !entry.Message.Failed() {
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
			eachControlEngine(t, func(t *testing.T, store astral.Store) {
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
				run, err := prompt(e, context.Background(), astral.User("work"))
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
						if entry.Kind == astral.EntryToolControl {
							t.Fatal("non-control return created a control fact")
						}
					}
				}
			})
		})
	}
}
