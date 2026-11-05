package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	core "github.com/oines/astral-core"
)

type preparingArgumentTool struct {
	core.Tool
	prepare func(json.RawMessage) (json.RawMessage, error)
	bind    func(context.Context, core.Invocation) (json.RawMessage, error)
}

func (t preparingArgumentTool) PrepareArguments(raw json.RawMessage) (json.RawMessage, error) {
	return t.prepare(raw)
}

func (t preparingArgumentTool) Bind(ctx context.Context, inv core.Invocation) (json.RawMessage, error) {
	return t.bind(ctx, inv)
}

func assertRawPreparedHistory(t *testing.T, s *core.Session, requests []core.Request, raw string) {
	t.Helper()
	var calls, records, projected int
	for _, entry := range entries(s) {
		if entry.Message != nil {
			for _, call := range entry.Message.ToolCalls() {
				calls++
				if string(call.Arguments) != raw {
					t.Fatalf("model call was rewritten in history: %s", call.Arguments)
				}
			}
		}
		if entry.Tool != nil {
			records++
			if string(entry.Tool.Invocation.Call.Arguments) != raw {
				t.Fatalf("%s ledger invocation lost original model arguments: %s", entry.Kind, entry.Tool.Invocation.Call.Arguments)
			}
		}
	}
	for _, request := range requests {
		for _, message := range request.Messages {
			for _, call := range message.ToolCalls() {
				projected++
				if string(call.Arguments) != raw {
					t.Fatalf("next model request saw rewritten arguments: %s", call.Arguments)
				}
			}
		}
	}
	if calls != 1 || records == 0 || projected == 0 {
		t.Fatalf("missing original call evidence: history=%d ledger=%d next-request=%d", calls, records, projected)
	}
}

func TestToolArgumentPreparationUsesEffectiveCallbacksAndPreservesOriginalCall(t *testing.T) {
	for _, sequential := range []bool{false, true} {
		t.Run(fmt.Sprintf("sequential=%v", sequential), func(t *testing.T) {
			eachStore(t, func(t *testing.T, store core.Store) {
				const raw, effective = `{"legacy":"normalized"}`, `{"value":"normalized"}`
				const binding = `{"target":"approved"}`
				var prepared, bound, before, executed, after atomic.Int32
				check := func(phase string, inv core.Invocation, wantBinding string) error {
					if string(inv.Call.Arguments) != effective || string(inv.Binding) != wantBinding {
						return fmt.Errorf("%s received arguments=%s binding=%s", phase, inv.Call.Arguments, inv.Binding)
					}
					return nil
				}
				base := core.ToolFunc{Spec: core.ToolDefinition{Name: "normalize", Version: "prepared-v1", Sequential: sequential, Parameters: json.RawMessage(valueSchema)}, Run: func(_ context.Context, inv core.Invocation, _ func(core.ToolProgress)) (core.ToolExecution, error) {
					executed.Add(1)
					if err := check("execute", inv, binding); err != nil {
						return core.ToolExecution{}, err
					}
					return core.ToolExecution{Result: &core.ToolResult{Content: core.Text("normalized result")}}, nil
				}}
				wrapped := preparingArgumentTool{Tool: base, prepare: func(arguments json.RawMessage) (json.RawMessage, error) {
					prepared.Add(1)
					if string(arguments) != raw {
						return nil, fmt.Errorf("preflight/recovery normalized an already changed call: %s", arguments)
					}
					// The preparer owns its input buffer. Mutating it must not change
					// the model's reply or any durable invocation record.
					for i := range arguments {
						arguments[i] = '!'
					}
					return json.RawMessage(effective), nil
				}, bind: func(_ context.Context, inv core.Invocation) (json.RawMessage, error) {
					bound.Add(1)
					if err := check("bind", inv, ""); err != nil {
						return nil, err
					}
					return json.RawMessage(binding), nil
				}}
				m := model(func(_ context.Context, n int, _ core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
					if n == 1 {
						return calls(call("normalize-call", "normalize", raw)), nil
					}
					return answer("done"), nil
				})
				config := testConfig(m, store)
				config.Tools = []core.Tool{wrapped}
				config.BeforeToolCall = func(ctx context.Context, inv core.ToolContext) (core.ToolDecision, error) {
					before.Add(1)
					if err := check("before", inv.Invocation, binding); err != nil {
						return core.ToolDecision{}, err
					}
					decision, err := inv.AwaitApproval(ctx, core.ApprovalRequest{Prompt: "Approve the normalized action?", Details: inv.Call.Arguments})
					return core.ToolDecision{Block: !decision.Approved}, err
				}
				config.AfterToolCall = func(_ context.Context, inv core.Invocation, _ core.ToolResult) (core.ToolResultPatch, error) {
					after.Add(1)
					return core.ToolResultPatch{}, check("after", inv, binding)
				}
				s := newSession(t, newSessions(t, config), core.SessionOptions{})
				sub := s.Subscribe(128)
				defer sub.Close()
				run := start(t, s, "normalize")
				approvalID := waitApproval(t, sub)
				pending := s.PendingApprovals()
				if len(pending) != 1 || string(pending[0].Request.Details) != effective || string(pending[0].Invocation.Call.Arguments) != raw || executed.Load() != 0 {
					t.Fatalf("approval did not retain original provenance and effective action: %+v, executions=%d", pending, executed.Load())
				}
				if err := s.DecideApproval(context.Background(), approvalID, core.ApprovalDecision{Approved: true}); err != nil {
					t.Fatal(err)
				}
				waitOK(t, run)
				if prepared.Load() == 0 || bound.Load() != 1 || before.Load() != 1 || executed.Load() != 1 || after.Load() != 1 {
					t.Fatalf("pipeline counts: prepare=%d bind=%d before=%d execute=%d after=%d", prepared.Load(), bound.Load(), before.Load(), executed.Load(), after.Load())
				}
				results := toolResults(s)
				if len(results) != 1 || results[0].IsError {
					t.Fatalf("effective callbacks failed: %+v", results)
				}
				assertRawPreparedHistory(t, s, m.observed(), raw)
			})
		})
	}
}

func TestToolArgumentPreparationFailuresNeverDispatch(t *testing.T) {
	for _, failure := range []string{"error", "panic", "invalid-json", "invalid-schema"} {
		t.Run(failure, func(t *testing.T) {
			eachStore(t, func(t *testing.T, store core.Store) {
				var binds, hooks, effects atomic.Int32
				wrapped := preparingArgumentTool{Tool: tool("normalize", func(context.Context, core.Invocation, func(core.ToolProgress)) (core.ToolExecution, error) {
					effects.Add(1)
					return core.ToolExecution{Result: &core.ToolResult{Content: core.Text("unexpected")}}, nil
				}), prepare: func(json.RawMessage) (json.RawMessage, error) {
					switch failure {
					case "error":
						return nil, errors.New("unsupported legacy form")
					case "panic":
						panic("invalid preparer")
					case "invalid-json":
						return json.RawMessage(`{`), nil
					default:
						return json.RawMessage(`{"value":42}`), nil
					}
				}, bind: func(context.Context, core.Invocation) (json.RawMessage, error) {
					binds.Add(1)
					return nil, nil
				}}
				m := model(func(_ context.Context, n int, _ core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
					if n == 1 {
						return calls(call("invalid-preparation", "normalize", `{"legacy":"x"}`)), nil
					}
					return answer("handled"), nil
				})
				config := testConfig(m, store)
				config.Tools = []core.Tool{wrapped}
				config.BeforeToolCall = func(context.Context, core.ToolContext) (core.ToolDecision, error) {
					hooks.Add(1)
					return core.ToolDecision{}, nil
				}
				s := newSession(t, newSessions(t, config), core.SessionOptions{})
				waitOK(t, start(t, s, "invalid"))
				results := toolResults(s)
				if binds.Load() != 0 || hooks.Load() != 0 || effects.Load() != 0 || len(results) != 1 || !results[0].IsError || results[0].Code != "invalid_arguments" || results[0].Outcome != core.OutcomeNotExecuted {
					t.Fatalf("bad preparation escaped preflight: bind=%d before=%d effects=%d results=%+v", binds.Load(), hooks.Load(), effects.Load(), results)
				}
				assertRawPreparedHistory(t, s, m.observed(), `{"legacy":"x"}`)
			})
		})
	}
}

func TestPreparedToolArgumentPreparationRecoversWithoutRebindingOrRewriting(t *testing.T) {
	eachStore(t, func(t *testing.T, store core.Store) {
		const raw, effective = `{"legacy":"recovered"}`, `{"value":"recovered"}`
		var preparations, binds, before, effects atomic.Int32
		fault := &faultStore{Store: store, kind: core.EntryToolStarted}
		target := "original"
		wrapped := preparingArgumentTool{Tool: tool("normalize", func(_ context.Context, inv core.Invocation, _ func(core.ToolProgress)) (core.ToolExecution, error) {
			effects.Add(1)
			if string(inv.Call.Arguments) != effective || bindingOf(inv.Binding).Target != "original" {
				return core.ToolExecution{}, fmt.Errorf("recovered dispatch changed arguments or binding: %+v", inv)
			}
			return core.ToolExecution{Result: &core.ToolResult{Content: core.Text("recovered")}}, nil
		}), prepare: func(arguments json.RawMessage) (json.RawMessage, error) {
			preparations.Add(1)
			if string(arguments) != raw {
				return nil, fmt.Errorf("recovery was not normalized from the original call: %s", arguments)
			}
			return json.RawMessage(effective), nil
		}, bind: func(_ context.Context, inv core.Invocation) (json.RawMessage, error) {
			binds.Add(1)
			if string(inv.Call.Arguments) != effective {
				return nil, errors.New("binding received raw legacy arguments")
			}
			return json.Marshal(hostBinding{Target: target})
		}}
		m := model(func(_ context.Context, n int, _ core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
			if n == 1 {
				return calls(call("recover-prepared", "normalize", raw)), nil
			}
			return answer("done"), nil
		})
		config := testConfig(m, fault)
		config.Tools = []core.Tool{wrapped}
		config.BeforeToolCall = func(_ context.Context, inv core.ToolContext) (core.ToolDecision, error) {
			before.Add(1)
			if string(inv.Call.Arguments) != effective {
				return core.ToolDecision{}, errors.New("approval received raw legacy arguments")
			}
			return core.ToolDecision{}, nil
		}
		manager := newSessions(t, config)
		s := newSession(t, manager, core.SessionOptions{})
		if _, err := wait(t, start(t, s, "prepare")); err == nil || !strings.Contains(err.Error(), "injected store failure") {
			t.Fatalf("missing prepared/started failure: %v", err)
		}
		if effects.Load() != 0 || binds.Load() != 1 || before.Load() != 1 {
			t.Fatalf("unexpected effect before durable Started: binds=%d before=%d effects=%d", binds.Load(), before.Load(), effects.Load())
		}
		preparedBeforeReopen := preparations.Load()
		if err := manager.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		target = "replacement"
		config.Store = store
		reopened, err := newSessions(t, config).OpenSession(context.Background(), s.ID())
		if err != nil {
			t.Fatal(err)
		}
		run, err := reopened.Continue(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		waitOK(t, run)
		if preparations.Load() <= preparedBeforeReopen || binds.Load() != 1 || before.Load() != 1 || effects.Load() != 1 {
			t.Fatalf("prepared recovery replayed authorization/binding or missed normalization: prepare=%d bind=%d before=%d effects=%d", preparations.Load(), binds.Load(), before.Load(), effects.Load())
		}
		results := toolResults(reopened)
		if len(results) != 1 || results[0].IsError {
			t.Fatalf("recovered normalized execution failed: %+v", results)
		}
		assertRawPreparedHistory(t, reopened, m.observed(), raw)
	})
}
