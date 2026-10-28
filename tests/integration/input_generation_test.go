package integration_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	core "github.com/oines/astral-core"
)

func passiveInput(id string) core.Input {
	return core.Input{ID: id, Contexts: []core.Message{snapshotMessage("observation " + id)}}
}

func requireInputGeneration(t *testing.T, s *core.Session, id string, wanted bool) core.InputReceipt {
	t.Helper()
	receipt, err := s.LookupInput(context.Background(), id)
	if err != nil || (receipt.Generation != nil) != wanted {
		t.Fatalf("input %s generation=%+v want=%v: %v", id, receipt.Generation, wanted, err)
	}
	return receipt
}

func TestInputGenerationDistinguishesAppliedContextFromFrozenRequest(t *testing.T) {
	eachStore(t, func(t *testing.T, store core.Store) {
		ctx := context.Background()
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		defer once.Do(func() { close(release) })
		m := model(func(ctx context.Context, n int, _ core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
			if n == 1 {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return core.Reply{}, ctx.Err()
				}
			}
			return answer("done"), nil
		})
		c := testConfig(m, store)
		s := newSession(t, newSessions(t, c), core.SessionOptions{})
		if _, err := s.InjectContext(ctx, passiveInput("before")); err != nil {
			t.Fatal(err)
		}
		before := requireInputGeneration(t, s, "before", false)
		if before.State != core.InputApplied || before.AcceptedRunID != "" || !s.IsIdle() {
			t.Fatalf("idle application is not a generation: %+v", before)
		}
		run := start(t, s, "work")
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("generation did not start")
		}
		first := requireInputGeneration(t, s, "before", true)
		if first.Generation.RunID != run.ID() || first.Generation.AttemptID == "" || first.Generation.TurnID == "" {
			t.Fatalf("missing actual request owner: %+v", first.Generation)
		}
		if s.IsIdle() {
			t.Fatal("in-flight model request reported idle")
		}
		if _, err := s.InjectContext(ctx, passiveInput("after")); err != nil {
			t.Fatal(err)
		}
		after := requireInputGeneration(t, s, "after", false)
		if after.AcceptedRunID != run.ID() {
			t.Fatalf("lost admission run: %+v", after)
		}
		wake := core.Input{ID: "wake-after", Message: core.User("handle after")}
		admission, err := s.SteerIfIdle(ctx, wake, []string{"after"})
		if err != nil || admission.Status != core.InputBusy {
			t.Fatalf("wake raced an active request: %+v %v", admission, err)
		}
		once.Do(func() { close(release) })
		waitOK(t, run)
		after = requireInputGeneration(t, s, "after", false)
		if after.State != core.InputApplied || !s.IsIdle() {
			t.Fatalf("terminal passive maintenance stole ownership: %+v", after)
		}
		admission, err = s.SteerIfIdle(ctx, wake, []string{"after"})
		if err != nil || admission.Status != core.InputAccepted || admission.Receipt.ID != wake.ID || s.IsIdle() {
			t.Fatalf("idle handoff failed: %+v %v", admission, err)
		}
		again, err := s.SteerIfIdle(ctx, wake, []string{"after"})
		if err != nil || again.Status != core.InputExisting || !reflect.DeepEqual(again.Receipt, admission.Receipt) {
			t.Fatalf("conditional admission was not idempotent: %+v %v", again, err)
		}
		next, err := s.Continue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		waitOK(t, next)
		owned := requireInputGeneration(t, s, "after", true)
		if owned.Generation.RunID != next.ID() || owned.AcceptedRunID != run.ID() {
			t.Fatalf("admission and generation ownership conflated: %+v", owned)
		}
		if got := len(m.observed()); got != 2 {
			t.Fatalf("handoff generated %d times", got)
		}
		if err := s.Close(ctx); err != nil {
			t.Fatal(err)
		}
		reopened, err := core.OpenSession(ctx, c, s.ID())
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close(ctx)
		if restored := requireInputGeneration(t, reopened, "after", true); !reflect.DeepEqual(restored.Generation, owned.Generation) || restored.AcceptedRunID != owned.AcceptedRunID {
			t.Fatalf("reopen lost durable ownership: %+v", restored)
		}
		stale, err := reopened.SteerIfIdle(ctx, core.Input{ID: "stale", Message: core.User("late probe")}, []string{"after"})
		if err != nil || stale.Status != core.InputCovered || !reopened.IsIdle() {
			t.Fatalf("late decision was not discarded: %+v %v", stale, err)
		}
	})
}

func TestWorkingToolBoundaryClaimsObservationWithoutSteering(t *testing.T) {
	eachStore(t, func(t *testing.T, store core.Store) {
		ctx := context.Background()
		toolEntered, toolRelease := make(chan struct{}), make(chan struct{})
		modelEntered, modelRelease := make(chan struct{}), make(chan struct{})
		var toolOnce, modelOnce sync.Once
		defer toolOnce.Do(func() { close(toolRelease) })
		defer modelOnce.Do(func() { close(modelRelease) })
		m := model(func(ctx context.Context, n int, _ core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
			if n == 1 {
				return calls(call("work-call", "work", `{"value":"work"}`)), nil
			}
			close(modelEntered)
			select {
			case <-modelRelease:
				return answer("done"), nil
			case <-ctx.Done():
				return core.Reply{}, ctx.Err()
			}
		})
		c := testConfig(m, store)
		c.Tools = []core.Tool{tool("work", func(ctx context.Context, _ core.Invocation, _ func(core.ToolProgress)) (core.ToolExecution, error) {
			close(toolEntered)
			select {
			case <-toolRelease:
				return core.ToolExecution{Result: &core.ToolResult{Content: core.Text("worked")}}, nil
			case <-ctx.Done():
				return core.ToolExecution{}, ctx.Err()
			}
		})}
		s := newSession(t, newSessions(t, c), core.SessionOptions{})
		run := start(t, s, "start")
		select {
		case <-toolEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("tool did not start")
		}
		for _, id := range []string{"mention", "ordinary"} {
			if _, err := s.InjectContext(ctx, passiveInput(id)); err != nil {
				t.Fatal(err)
			}
			requireInputGeneration(t, s, id, false)
		}
		toolOnce.Do(func() { close(toolRelease) })
		select {
		case <-modelEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("next model request did not start")
		}
		for _, id := range []string{"mention", "ordinary"} {
			receipt := requireInputGeneration(t, s, id, true)
			if receipt.AcceptedRunID != run.ID() || receipt.Generation.RunID != run.ID() {
				t.Fatalf("observation was not claimed by current work: %+v", receipt)
			}
		}
		stale, err := s.SteerIfIdle(ctx, core.Input{ID: "late-probe", Message: core.User("wake")}, []string{"ordinary"})
		if err != nil || stale.Status != core.InputCovered {
			t.Fatalf("already claimed observation accepted a wake: %+v %v", stale, err)
		}
		modelOnce.Do(func() { close(modelRelease) })
		waitOK(t, run)
		if len(m.observed()) != 2 || !s.IsIdle() {
			t.Fatal("observation created an extra driving turn")
		}
		if queued, err := s.HasQueuedInputs(ctx); err != nil || queued {
			t.Fatalf("late wake entered the queue: %v %v", queued, err)
		}
	})
}

func TestInputGenerationOnlyClaimsSourcesIncludedByModelPreparation(t *testing.T) {
	eachStore(t, func(t *testing.T, store core.Store) {
		ctx := context.Background()
		base := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
			return answer("done"), nil
		})
		prepared := preparedModel{testModel: base, prepare: func(_ context.Context, request core.Request) (core.Request, error) {
			var kept []core.Message
			for _, message := range request.Messages {
				if message.ContextKind == "environment" {
					continue
				}
				kept = append(kept, message)
			}
			request.Messages = kept
			return request, nil
		}}
		c := testConfig(prepared, store)
		s := newSession(t, newSessions(t, c), core.SessionOptions{})
		dropped := passiveInput("dropped")
		dropped.Contexts[0].ContextKind = "environment"
		if _, err := s.InjectContext(ctx, dropped); err != nil {
			t.Fatal(err)
		}
		if _, err := s.InjectContext(ctx, passiveInput("visible")); err != nil {
			t.Fatal(err)
		}
		waitOK(t, start(t, s, "work"))
		requireInputGeneration(t, s, "dropped", false)
		requireInputGeneration(t, s, "visible", true)
		admission, err := s.SteerIfIdle(ctx, core.Input{ID: "mixed", Message: core.User("wake")}, []string{"dropped", "visible"})
		if err != nil || admission.Status != core.InputCovered {
			t.Fatalf("mixed stale batch was admitted: %+v %v", admission, err)
		}
		if _, err := s.LookupInput(ctx, "mixed"); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("rejected wake acquired a stable ID: %v", err)
		}
	})
}

func TestInputGenerationDoesNotClaimReplacementObservation(t *testing.T) {
	eachStore(t, func(t *testing.T, store core.Store) {
		ctx := context.Background()
		base := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
			return answer("done"), nil
		})
		prepared := preparedModel{testModel: base, prepare: func(_ context.Context, request core.Request) (core.Request, error) {
			for i := range request.Messages {
				if request.Messages[i].ContextKind == "im.observation" {
					request.Messages[i].Content = core.Text("unrelated replacement")
					request.Messages[i].Metadata = []byte(`{"replaced":true}`)
				}
			}
			return request, nil
		}}
		s := newSession(t, newSessions(t, testConfig(prepared, store)), core.SessionOptions{})
		input := passiveInput("replaced")
		input.Contexts[0].ContextKind = "im.observation"
		if _, err := s.InjectContext(ctx, input); err != nil {
			t.Fatal(err)
		}
		waitOK(t, start(t, s, "work"))
		requireInputGeneration(t, s, input.ID, false)
	})
}

func TestInputGenerationCompactionAndForkOwnership(t *testing.T) {
	eachStore(t, func(t *testing.T, store core.Store) {
		ctx := context.Background()
		m := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
			return answer("summary or final"), nil
		})
		c := testConfig(m, store)
		c.Compaction.KeepRecentTokens = 1
		s := newSession(t, newSessions(t, c), core.SessionOptions{})
		for i := range 6 {
			if _, err := s.InjectContext(ctx, passiveInput(fmt.Sprint(i))); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Compact(ctx, ""); err != nil {
			t.Fatal(err)
		}
		for i := range 6 {
			requireInputGeneration(t, s, fmt.Sprint(i), false)
		}
		for _, mode := range []core.ForkMode{core.ForkShared, core.ForkDetached} {
			child, err := s.Fork(ctx, s.Head(), core.ForkOptions{ID: fmt.Sprintf("fork-%d", mode), Mode: mode})
			if err != nil {
				t.Fatal(err)
			}
			defer child.Close(ctx)
			waitOK(t, start(t, child, "child work"))
			if _, err := child.LookupInput(ctx, "0"); !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("fork inherited source execution ownership: %v", err)
			}
			for i := range 6 {
				requireInputGeneration(t, s, fmt.Sprint(i), false)
			}
		}
		waitOK(t, start(t, s, "parent work"))
		for i := range 6 {
			requireInputGeneration(t, s, fmt.Sprint(i), true)
		}
	})
}

func TestConditionalSteeringRequiresAppliedContextAndDoesNotRestartCancelledWork(t *testing.T) {
	eachStore(t, func(t *testing.T, store core.Store) {
		ctx := context.Background()
		entered := make(chan struct{})
		m := model(func(ctx context.Context, _ int, _ core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
			close(entered)
			<-ctx.Done()
			return core.Reply{}, ctx.Err()
		})
		s := newSession(t, newSessions(t, testConfig(m, store)), core.SessionOptions{})
		if _, err := s.InjectContext(ctx, passiveInput("already-seen")); err != nil {
			t.Fatal(err)
		}
		run := start(t, s, "work")
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("generation did not start")
		}
		for i := range 65 {
			if _, err := s.InjectContext(ctx, passiveInput(fmt.Sprint("queued-", i))); err != nil {
				t.Fatal(err)
			}
		}
		run.Cancel()
		if _, err := run.Wait(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation was not retained: %v", err)
		}
		if !s.IsIdle() || s.State().LastRun.Status != "cancelled" {
			t.Fatal("cancelled activation did not relinquish execution")
		}
		for i := range 65 {
			receipt := requireInputGeneration(t, s, fmt.Sprint("queued-", i), false)
			if receipt.State != core.InputPending || receipt.AcceptedRunID != run.ID() {
				t.Fatalf("cancellation lost pending admission ownership: %+v", receipt)
			}
		}
		wake := core.Input{ID: "after-stop", Message: core.User("explicit wake")}
		admission, err := s.SteerIfIdle(ctx, wake, []string{"queued-64"})
		if err != nil || admission.Status != core.InputBusy {
			t.Fatalf("wakeup outran its unflushed observation: %+v %v", admission, err)
		}
		mixed, err := s.SteerIfIdle(ctx, wake, []string{"queued-64", "already-seen"})
		if err != nil || mixed.Status != core.InputCovered {
			t.Fatalf("mixed covered/pending batch did not require recomputation: %+v %v", mixed, err)
		}
		if _, err := s.InjectContext(ctx, passiveInput("queued-64")); err != nil {
			t.Fatal(err)
		}
		for i := range 65 {
			receipt := requireInputGeneration(t, s, fmt.Sprint("queued-", i), false)
			if receipt.State != core.InputApplied {
				t.Fatalf("passive readmission did not flush all bounded batches: %+v", receipt)
			}
		}
		if len(m.observed()) != 1 || !s.IsIdle() || s.State().LastRun.Status != "cancelled" {
			t.Fatal("passive reconciliation restarted cancelled execution")
		}
	})
}

func TestInputGenerationCommitAndLostAcknowledgementAreAtomic(t *testing.T) {
	for _, lostAcknowledgement := range []bool{false, true} {
		t.Run(fmt.Sprint("lost-ack-", lostAcknowledgement), func(t *testing.T) {
			eachStore(t, func(t *testing.T, underlying core.Store) {
				ctx := context.Background()
				store := &faultStore{Store: underlying, attemptPhase: "create", after: lostAcknowledgement}
				m := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
					return answer("done"), nil
				})
				c := testConfig(m, store)
				s := newSession(t, newSessions(t, c), core.SessionOptions{})
				if _, err := s.InjectContext(ctx, passiveInput("observation")); err != nil {
					t.Fatal(err)
				}
				run := start(t, s, "work")
				_, err := run.Wait(ctx)
				if lostAcknowledgement && err != nil || !lostAcknowledgement && err == nil {
					t.Fatalf("generation commit result: %v", err)
				}
				receipt := requireInputGeneration(t, s, "observation", lostAcknowledgement)
				wantCalls := 0
				if lostAcknowledgement {
					wantCalls = 1
					if receipt.Generation.RunID != run.ID() {
						t.Fatal("reconciled commit changed generation owner")
					}
				}
				if len(m.observed()) != wantCalls {
					t.Fatal("provider was invoked before the durable ownership commit")
				}
			})
		})
	}
}
