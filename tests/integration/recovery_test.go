package integration_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
	sessions "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/store/memory"
)

func waitApproval(t *testing.T, sub *astral.Subscription) string {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-sub.C:
			if !ok {
				t.Fatalf("subscription ended: %v", sub.Err())
			}
			if event.Type == astral.EventApproval {
				return event.ApprovalID
			}
		case <-timeout:
			t.Fatal("approval did not arrive")
		}
	}
}
func seedStore(t *testing.T, id string, rollout historySnapshot) *memory.Store {
	t.Helper()
	store := memory.New()
	seedHistory(t, store, id, rollout)
	return store
}

func seedHistory(t *testing.T, store sessions.Store, id string, rollout historySnapshot) {
	t.Helper()
	for start := 0; start < len(rollout.Entries); {
		end := start + 1
		for end < len(rollout.Entries) && rollout.Entries[end].Version == rollout.Entries[start].Version {
			end++
		}
		if err := appendStore(context.Background(), store, id, rollout.Entries[start].Version-1, rollout.Entries[start:end]); err != nil {
			t.Fatal(err)
		}
		start = end
	}
}

func TestApprovalDecisionSurvivesRestartAndHookReentry(t *testing.T) {
	var executed, hookCalls atomic.Int32
	m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("change", "echo", `{"value":"approved action"}`)), nil
		}
		return answer("done"), nil
	})
	store := memory.New()
	c := testConfig(m, store)
	c.Tools = []astral.Tool{echoTool(&executed)}
	c.BeforeToolCall = func(ctx context.Context, inv sessions.ToolContext) (astral.ToolDecision, error) {
		hookCalls.Add(1)
		decision, err := inv.AwaitApproval(ctx, sessions.ApprovalRequest{Prompt: "Allow this action?"})
		return astral.ToolDecision{Block: !decision.Approved, Reason: decision.Reason}, err
	}
	a := newAgent(t, c)
	s := newSession(t, a, sessions.SessionOptions{ID: "approval"})
	sub := s.Subscribe(128)
	defer sub.Close()
	run := start(t, s, "go")
	approvalID := waitApproval(t, sub)
	if executed.Load() != 0 {
		t.Fatal("approval bypassed")
	}
	snapshot, err := readStore(context.Background(), store, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	run.Cancel()
	_, _ = wait(t, run)
	c.Store = seedStore(t, s.ID(), snapshot)
	a2 := newAgent(t, c)
	resumed, err := a2.OpenSession(context.Background(), s.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.PendingApprovals()) != 1 {
		t.Fatal("approval was not reconstructed")
	}
	if err = resumed.DecideApproval(context.Background(), approvalID, sessions.ApprovalDecision{Approved: true}); err != nil {
		t.Fatal(err)
	}
	r2, err := resumed.Continue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitOK(t, r2)
	if hookCalls.Load() != 2 || executed.Load() != 1 {
		t.Fatalf("hook=%d execution=%d", hookCalls.Load(), executed.Load())
	}
	if err = resumed.DecideApproval(context.Background(), approvalID, sessions.ApprovalDecision{Approved: true}); err != nil {
		t.Fatal("idempotent approval rejected")
	}
	if err = resumed.DecideApproval(context.Background(), approvalID, sessions.ApprovalDecision{Approved: false}); !errors.Is(err, sessions.ErrAlreadyResolved) {
		t.Fatalf("conflicting decision: %v", err)
	}
}

func TestInterruptedToolBecomesUnknownAndIsNeverReplayed(t *testing.T) {
	entered := make(chan struct{})
	var effects atomic.Int32
	work := tool("effect", func(ctx context.Context, _ astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolExecution, error) {
		effects.Add(1)
		close(entered)
		<-ctx.Done()
		return astral.ToolExecution{Result: &astral.ToolResult{}}, ctx.Err()
	})
	m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("effect-call", "effect", `{"value":"x"}`)), nil
		}
		return answer("reconciled"), nil
	})
	store := memory.New()
	c := testConfig(m, store)
	c.Tools = []astral.Tool{work}
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{ID: "unknown"})
	r := start(t, s, "go")
	signal(t, entered)
	snapshot, err := readStore(context.Background(), store, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	r.Cancel()
	_, _ = wait(t, r)
	c.Store = seedStore(t, s.ID(), snapshot)
	reopened, err := newAgent(t, c).OpenSession(context.Background(), s.ID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.Continue(context.Background()); !errors.Is(err, astral.ErrOutcomeUnknown) {
		t.Fatalf("recovery: %v", err)
	}
	unknown := reopened.UnresolvedTools()
	if len(unknown) != 1 || effects.Load() != 1 {
		t.Fatal("unknown operation replayed or hidden")
	}
	if err = reopened.ResolveTool(context.Background(), unknown[0].Invocation.ExecutionID, astral.ToolExecution{Result: &astral.ToolResult{Outcome: astral.OutcomeSucceeded, Content: astral.Text("Host confirmed the operation completed.")}}); err != nil {
		t.Fatal(err)
	}
	r2, err := reopened.Continue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitOK(t, r2)
	if effects.Load() != 1 || len(reopened.UnresolvedTools()) != 0 {
		t.Fatal("resolved operation replayed")
	}
	view, err := reopened.InspectContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	results := texts(view.Request.Messages, astral.RoleTool)
	if len(results) != 1 || !strings.Contains(results[0], "Host confirmed") {
		t.Fatalf("resolution not projected: %v", results)
	}
	if len(toolResults(reopened)) != 1 {
		t.Fatal("unknown history was rewritten instead of appended")
	}
}

type faultStore struct {
	sessions.Store
	kind  sessions.EntryKind
	after bool
	fired atomic.Bool
}

func (s *faultStore) Commit(ctx context.Context, id string, c sessions.Commit) (sessions.Receipt, error) {
	fail := false
	for _, e := range c.Entries {
		if e.Kind == s.kind && !s.fired.Swap(true) {
			fail = true
		}
	}
	if fail && !s.after {
		return sessions.Receipt{}, errors.New("injected store failure")
	}
	receipt, err := s.Store.Commit(ctx, id, c)
	if err != nil {
		return receipt, err
	}
	if fail {
		return sessions.Receipt{}, errors.New("acknowledgement lost")
	}
	return receipt, nil
}

func TestExecutionFactSurvivesMissingFinalResult(t *testing.T) {
	var effects, afterHooks atomic.Int32
	storage := &faultStore{Store: memory.New(), kind: sessions.EntryToolResult}
	m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("c", "echo", `{"value":"output"}`)), nil
		}
		return answer("continued"), nil
	})
	c := testConfig(m, storage)
	c.Tools = []astral.Tool{echoTool(&effects)}
	c.AfterToolCall = func(_ context.Context, _ astral.Invocation, result astral.ToolResult) (astral.ToolResultPatch, error) {
		afterHooks.Add(1)
		return astral.ToolResultPatch{}, nil
	}
	a := newAgent(t, c)
	s := newSession(t, a, sessions.SessionOptions{})
	if _, err := wait(t, start(t, s, "go")); err == nil {
		t.Fatal("store failure was hidden")
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	opened, err := newAgent(t, c).OpenSession(context.Background(), s.ID())
	if err != nil {
		t.Fatal(err)
	}
	r, err := opened.Continue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitOK(t, r)
	if effects.Load() != 1 || afterHooks.Load() != 1 {
		t.Fatal("completed effect or hook replayed")
	}
	results := toolResults(opened)
	if len(results) != 1 || results[0].Outcome != astral.OutcomeSucceeded || results[0].Code != "result_unavailable" {
		t.Fatalf("recovery result: %+v", results)
	}
}

func TestDurableAuthorizationIsNotRecomputed(t *testing.T) {
	var effects, beforeHooks atomic.Int32
	storage := &faultStore{Store: memory.New(), kind: sessions.EntryToolStarted}
	m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("c", "echo", `{"value":"x"}`)), nil
		}
		return answer("done"), nil
	})
	c := testConfig(m, storage)
	c.Tools = []astral.Tool{echoTool(&effects)}
	c.BeforeToolCall = func(context.Context, sessions.ToolContext) (astral.ToolDecision, error) {
		beforeHooks.Add(1)
		return astral.ToolDecision{}, nil
	}
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
	if _, err := wait(t, start(t, s, "go")); err == nil {
		t.Fatal("expected injected failure")
	}
	if effects.Load() != 0 {
		t.Fatal("executed without committed start")
	}
	r, err := s.Continue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitOK(t, r)
	if effects.Load() != 1 || beforeHooks.Load() != 1 {
		t.Fatal("authorization was recomputed")
	}
}

func TestLostStoreAcknowledgementIsReconciledBeforeExecution(t *testing.T) {
	var effects atomic.Int32
	storage := &faultStore{Store: memory.New(), kind: sessions.EntryToolStarted, after: true}
	m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("c", "echo", `{"value":"x"}`)), nil
		}
		return answer("done"), nil
	})
	c := testConfig(m, storage)
	c.Tools = []astral.Tool{echoTool(&effects)}
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
	waitOK(t, start(t, s, "go"))
	if effects.Load() != 1 {
		t.Fatal("ambiguous commit mishandled")
	}
}

func TestBlockedToolSkipsAfterHookAndMayTerminate(t *testing.T) {
	var effects, after atomic.Int32
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return calls(call("c", "echo", `{"value":"x"}`)), nil
	})
	c := testConfig(m, memory.New())
	c.Tools = []astral.Tool{echoTool(&effects)}
	c.BeforeToolCall = func(context.Context, sessions.ToolContext) (astral.ToolDecision, error) {
		return astral.ToolDecision{Block: true, Reason: "denied", Terminate: true}, nil
	}
	c.AfterToolCall = func(_ context.Context, _ astral.Invocation, r astral.ToolResult) (astral.ToolResultPatch, error) {
		after.Add(1)
		return astral.ToolResultPatch{}, nil
	}
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
	waitOK(t, start(t, s, "go"))
	if effects.Load() != 0 || after.Load() != 0 || len(m.observed()) != 1 {
		t.Fatal("blocked call crossed a boundary")
	}
}

func TestAfterHookFailurePreservesExecutionOutcome(t *testing.T) {
	var effects atomic.Int32
	m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			return calls(call("c", "echo", `{"value":"real output"}`)), nil
		}
		return answer("done"), nil
	})
	c := testConfig(m, memory.New())
	c.Tools = []astral.Tool{echoTool(&effects)}
	c.AfterToolCall = func(context.Context, astral.Invocation, astral.ToolResult) (astral.ToolResultPatch, error) {
		return astral.ToolResultPatch{}, errors.New("formatting failed")
	}
	s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
	waitOK(t, start(t, s, "go"))
	result := toolResults(s)[0]
	if result.Outcome != astral.OutcomeSucceeded || result.Code != "after_tool_failed" || result.Content[0].Text != "real output" || effects.Load() != 1 {
		t.Fatalf("misreported effect: %+v", result)
	}
}
