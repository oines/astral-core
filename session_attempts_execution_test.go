package astral_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/memory"
)

type resumedExecutionModel struct {
	prepare func(context.Context, core.Request) (core.RequestExecution, error)
	stream  func(context.Context, core.Request, func(core.ModelEvent)) (core.Reply, error)
}

func (*resumedExecutionModel) Spec() core.ModelSpec {
	return core.ModelSpec{ID: "resume-execution", ContextWindow: 64000, MaxOutputTokens: 1024}
}

func (m *resumedExecutionModel) PrepareExecution(ctx context.Context, request core.Request) (core.RequestExecution, error) {
	return m.prepare(ctx, request)
}

func (m *resumedExecutionModel) Stream(ctx context.Context, request core.Request, emit func(core.ModelEvent)) (core.Reply, error) {
	return m.stream(ctx, request, emit)
}

func TestResumedExecutionPreparationPrecedesAttemptCommit(t *testing.T) {
	for _, mode := range []string{"failure", "execution", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store := memory.New()
			model := &resumedExecutionModel{}
			const sessionID = "resume-execution"
			message := core.User("already committed frozen input")
			entries := []core.Entry{
				{ID: "created", Kind: core.EntryCreated, Created: &core.CreationRecord{FormatVersion: core.FormatVersion, SessionID: sessionID, RolloutID: sessionID}},
				{ID: "config", Kind: core.EntryConfiguration, Configuration: &core.Configuration{Selection: core.ModelSelection{ID: model.Spec().ID}}},
				{ID: "input", ParentID: "config", Kind: core.EntryMessage, Message: &message},
			}
			for i := range entries {
				entries[i].SessionID, entries[i].Version, entries[i].Time = sessionID, 1, time.Now().UTC()
			}
			if _, err := store.Commit(ctx, sessionID, core.Commit{ID: "history", Entries: entries}); err != nil {
				t.Fatal(err)
			}
			base, err := core.ReadContext(ctx, store, sessionID, "input", core.DefaultContextBytes)
			if err != nil {
				t.Fatal(err)
			}
			frozen := base.Request
			frozen.MaxOutputTokens = model.Spec().MaxOutputTokens
			input, err := core.FreezeRequest(sessionID, base, frozen, model.Spec())
			if err != nil {
				t.Fatal(err)
			}
			previous := core.ModelAttempt{ID: "interrupted", SessionID: sessionID, Purpose: "generation", Number: 1, Version: 2, StartedAt: time.Now().UTC(), Input: input}
			if _, err = store.Commit(ctx, sessionID, core.Commit{ID: "frozen-attempt", ExpectedVersion: 1, Attempts: []core.AttemptMutation{{ID: previous.ID, Create: &previous}}}); err != nil {
				t.Fatal(err)
			}
			s, err := core.OpenSession(ctx, core.Config{
				Model: model, Store: store, Compaction: core.CompactionPolicy{Disabled: true}, Retry: core.RetryPolicy{MaxAttempts: 1},
				EstimateTokens: func(context.Context, core.Model, core.Request) (int, error) { return 100, nil },
			}, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.Close(ctx); err != nil {
					t.Error(err)
				}
			})
			late := core.Message{Role: core.RoleDeveloper, Content: core.Text("later observation"), ContextKind: "environment"}
			if _, err = s.InjectContext(ctx, core.Input{ID: "late-context", Contexts: []core.Message{late}}); err != nil {
				t.Fatal(err)
			}
			beforeHead := s.Head()
			failure := errors.New("cannot encode resumed request")
			prepares, streams, legacy := 0, 0, 0
			model.prepare = func(ctx context.Context, request core.Request) (core.RequestExecution, error) {
				prepares++
				page, err := store.ScanAttempts(ctx, sessionID, core.AttemptQuery{Limit: 10})
				if err != nil || len(page.Attempts) != 1 || page.Attempts[0].ID != previous.ID || page.Attempts[0].Outcome != nil {
					t.Errorf("resumed attempt committed before encoding: %+v %v", page.Attempts, err)
				}
				if !reflect.DeepEqual(request, frozen) {
					t.Error("resumed encoding changed the frozen request")
				}
				if mode == "failure" {
					return nil, failure
				}
				if mode == "legacy" {
					return nil, nil
				}
				return core.RequestExecutionFunc(func(context.Context, func(core.ModelEvent)) (core.Reply, error) {
					streams++
					return core.Reply{Message: core.Message{Role: core.RoleAssistant, Content: core.Text("resumed"), FinishReason: core.FinishStop}}, nil
				}), nil
			}
			model.stream = func(_ context.Context, request core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
				legacy++
				if !reflect.DeepEqual(request, frozen) {
					t.Error("legacy fallback changed the frozen request")
				}
				return core.Reply{Message: core.Message{Role: core.RoleAssistant, Content: core.Text("resumed"), FinishReason: core.FinishStop}}, nil
			}
			run, err := s.Continue(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, runErr := run.Wait(ctx)
			page, err := store.ScanAttempts(ctx, sessionID, core.AttemptQuery{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if prepares != 1 {
				t.Fatalf("execution factory invoked %d times", prepares)
			}
			if mode == "failure" {
				if !errors.Is(runErr, failure) || len(page.Attempts) != 1 || page.Attempts[0].Outcome != nil || s.Head() != beforeHead || streams != 0 || legacy != 0 {
					t.Fatalf("failed encoding changed the frozen boundary: attempts=%+v head=%s streams=%d legacy=%d err=%v", page.Attempts, s.Head(), streams, legacy, runErr)
				}
				receipt, err := s.LookupInput(ctx, "late-context")
				if err != nil || receipt.State != core.InputPending {
					t.Fatalf("failed resumed encoding consumed late context: %+v %v", receipt, err)
				}
				return
			}
			if runErr != nil || len(page.Attempts) != 2 || mode == "execution" && (streams != 1 || legacy != 0) || mode == "legacy" && (streams != 0 || legacy != 1) {
				t.Fatalf("resumed execution failed: attempts=%+v streams=%d legacy=%d err=%v", page.Attempts, streams, legacy, runErr)
			}
			for _, attempt := range page.Attempts {
				if attempt.ID == previous.ID {
					if attempt.Outcome == nil || attempt.Outcome.Status != "interrupted" {
						t.Fatal("previous attempt was not marked interrupted")
					}
				} else if attempt.RetryOf != previous.ID || attempt.Input.Digest != input.Digest || attempt.Outcome == nil || attempt.Outcome.Status != "completed" {
					t.Fatalf("wrong resumed attempt: %+v", attempt)
				}
			}
		})
	}
}
