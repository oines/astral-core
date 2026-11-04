package astral_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/memory"
)

type lifecycleExecutionModel struct {
	prepare      func(context.Context, core.Request) (core.RequestExecution, error)
	validate     func(core.Request) error
	recover      func(core.Request, error) (core.Request, bool, error)
	legacyStream func(context.Context, core.Request, func(core.ModelEvent)) (core.Reply, error)
	legacy       atomic.Int32
}

func (*lifecycleExecutionModel) Spec() core.ModelSpec {
	return core.ModelSpec{ID: "execution", ContextWindow: 64000, MaxOutputTokens: 1024}
}
func (m *lifecycleExecutionModel) Stream(ctx context.Context, request core.Request, emit func(core.ModelEvent)) (core.Reply, error) {
	m.legacy.Add(1)
	if m.legacyStream != nil {
		return m.legacyStream(ctx, request, emit)
	}
	return core.Reply{}, errors.New("legacy Stream must not handle a prepared execution")
}

func TestExecutionNilFallbackIsPreparedOnlyOnceBeforeLegacyStream(t *testing.T) {
	ctx := context.Background()
	model := &lifecycleExecutionModel{}
	var prepares atomic.Int32
	model.prepare = func(context.Context, core.Request) (core.RequestExecution, error) {
		if prepares.Add(1) > 1 {
			return nil, errors.New("execution factory invoked again after choosing legacy Stream")
		}
		return nil, nil
	}
	model.legacyStream = func(context.Context, core.Request, func(core.ModelEvent)) (core.Reply, error) {
		return lifecycleExecutionReply(), nil
	}
	s := lifecycleExecutionSession(t, memory.New(), model, func(context.Context, core.Model, core.Request) (int, error) { return 100, nil })
	prompt, err := s.Prompt(ctx, core.Input{ID: "legacy-execution", Message: core.User("use the legacy fallback")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prompt.Run.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if prepares.Load() != 1 || model.legacy.Load() != 1 {
		t.Fatalf("nil fallback repeated preparation: prepare=%d legacy=%d", prepares.Load(), model.legacy.Load())
	}
}
func (m *lifecycleExecutionModel) PrepareExecution(ctx context.Context, request core.Request) (core.RequestExecution, error) {
	return m.prepare(ctx, request)
}
func (m *lifecycleExecutionModel) ValidateRequest(_ context.Context, request core.Request) error {
	if m.validate != nil {
		return m.validate(request)
	}
	return nil
}
func (m *lifecycleExecutionModel) RecoverRequest(_ context.Context, request core.Request, cause error) (core.Request, bool, error) {
	if m.recover != nil {
		return m.recover(request, cause)
	}
	return core.Request{}, false, nil
}

func lifecycleExecutionReply() core.Reply {
	return core.Reply{Message: core.Message{Role: core.RoleAssistant, Content: core.Text("execution complete"), FinishReason: core.FinishStop}}
}

func lifecycleExecutionSession(t *testing.T, store core.Store, model *lifecycleExecutionModel, estimate core.TokenEstimator) *core.Session {
	t.Helper()
	s, err := core.NewSession(context.Background(), core.Config{Model: model, Store: store, EstimateTokens: estimate, Compaction: core.CompactionPolicy{Disabled: true}, Retry: core.RetryPolicy{MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}}, core.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestExecutionPreparationFailurePrecedesAttemptAndInputConsumption(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	model := &lifecycleExecutionModel{}
	s := lifecycleExecutionSession(t, store, model, func(context.Context, core.Model, core.Request) (int, error) { return 100, nil })
	before := s.Head()
	failure := errors.New("cannot encode final request")
	var prepares, streams atomic.Int32
	model.prepare = func(ctx context.Context, request core.Request) (core.RequestExecution, error) {
		prepares.Add(1)
		attempts, err := store.ScanAttempts(ctx, s.ID(), core.AttemptQuery{Limit: 10})
		if err != nil || len(attempts.Attempts) != 0 {
			t.Errorf("execution preparation followed attempt commit: %+v %v", attempts, err)
		}
		receipt, err := s.LookupInput(ctx, "queued-execution")
		if err != nil || receipt.State != core.InputPending || s.Head() != before {
			t.Errorf("input was consumed before execution preparation: %+v %v", receipt, err)
		}
		if prepares.Load() == 1 {
			return nil, failure
		}
		return core.RequestExecutionFunc(func(context.Context, func(core.ModelEvent)) (core.Reply, error) {
			streams.Add(1)
			return lifecycleExecutionReply(), nil
		}), nil
	}
	if _, err := s.FollowUp(ctx, core.Input{ID: "queued-execution", Message: core.User("keep this pending on encode failure")}); err != nil {
		t.Fatal(err)
	}
	run, err := s.Continue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = run.Wait(ctx); !errors.Is(err, failure) {
		t.Fatalf("prepare error lost: %v", err)
	}
	receipt, err := s.LookupInput(ctx, "queued-execution")
	if err != nil || receipt.State != core.InputPending || s.Head() != before || streams.Load() != 0 {
		t.Fatalf("failed preparation changed durable input: %+v %v", receipt, err)
	}
	run, err = s.Continue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = run.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	receipt, err = s.LookupInput(ctx, "queued-execution")
	if err != nil || receipt.State != core.InputApplied || prepares.Load() != 2 || streams.Load() != 1 || model.legacy.Load() != 0 {
		t.Fatalf("repaired encoder did not consume once: %+v %v", receipt, err)
	}
}

func TestExecutionNetworkRetryReusesOnePreparedObjectAndLegacyHooks(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	model := &lifecycleExecutionModel{}
	var prepares, streams, validates, estimates atomic.Int32
	model.validate = func(request core.Request) error {
		validates.Add(1)
		request.Messages[0].Content[0].Text = "validator mutation"
		return nil
	}
	model.prepare = func(_ context.Context, request core.Request) (core.RequestExecution, error) {
		prepares.Add(1)
		if request.Messages[0].Content[0].Text != "original execution input" {
			t.Error("legacy validator/estimator mutated the frozen request")
		}
		if validates.Load() == 0 || estimates.Load() == 0 {
			t.Error("custom validation or estimation was bypassed")
		}
		return core.RequestExecutionFunc(func(context.Context, func(core.ModelEvent)) (core.Reply, error) {
			if streams.Add(1) == 1 {
				return core.Reply{}, &core.ModelError{Code: "network", Transient: true}
			}
			return lifecycleExecutionReply(), nil
		}), nil
	}
	s := lifecycleExecutionSession(t, store, model, func(_ context.Context, _ core.Model, request core.Request) (int, error) {
		estimates.Add(1)
		if len(request.Messages) > 0 {
			request.Messages[0].Content[0].Text = "estimator mutation"
		}
		return 100, nil
	})
	prompt, err := s.Prompt(ctx, core.Input{ID: "retry-execution", Message: core.User("original execution input")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prompt.Run.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	attempts, err := s.Attempts().Scan(ctx, core.AttemptQuery{Purpose: "generation", Limit: 10})
	if err != nil || len(attempts.Attempts) != 2 || prepares.Load() != 1 || streams.Load() != 2 || model.legacy.Load() != 0 {
		t.Fatalf("retry prepared or used legacy Stream: prepare=%d stream=%d attempts=%d %v", prepares.Load(), streams.Load(), len(attempts.Attempts), err)
	}
	if attempts.Attempts[0].Input.Digest != attempts.Attempts[1].Input.Digest {
		t.Fatal("network retry changed frozen request")
	}
}

func TestExecutionRepairBuildsNewObjectAndPreservesOriginalHistory(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	model := &lifecycleExecutionModel{}
	rejected := errors.New("native state rejected")
	var prepares, streams atomic.Int32
	model.recover = func(request core.Request, cause error) (core.Request, bool, error) {
		if !errors.Is(cause, rejected) {
			return core.Request{}, false, nil
		}
		for i := range request.Messages {
			parts := request.Messages[i].Content[:0]
			for _, part := range request.Messages[i].Content {
				if part.Type != "provider" {
					parts = append(parts, part)
				}
			}
			request.Messages[i].Content = parts
		}
		return request, true, nil
	}
	model.prepare = func(_ context.Context, request core.Request) (core.RequestExecution, error) {
		ordinal := prepares.Add(1)
		raw, _ := json.Marshal(request)
		if (ordinal == 1) != strings.Contains(string(raw), "opaque-original") {
			t.Error("repair did not change the execution payload")
		}
		return core.RequestExecutionFunc(func(context.Context, func(core.ModelEvent)) (core.Reply, error) {
			streams.Add(1)
			if ordinal == 1 {
				return core.Reply{}, &core.ModelError{Code: "replay", Cause: rejected}
			}
			return lifecycleExecutionReply(), nil
		}), nil
	}
	s := lifecycleExecutionSession(t, store, model, func(context.Context, core.Model, core.Request) (int, error) { return 100, nil })
	message := core.User("repair the native wrapper")
	message.Content = append(message.Content, core.Content{Type: "provider", Data: json.RawMessage(`{"state":"opaque-original"}`)})
	prompt, err := s.Prompt(ctx, core.Input{ID: "repair-execution", Message: message})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prompt.Run.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	attempts, err := s.Attempts().Scan(ctx, core.AttemptQuery{Purpose: "generation", Limit: 10})
	if err != nil || len(attempts.Attempts) != 2 || prepares.Load() != 2 || streams.Load() != 2 || attempts.Attempts[0].Input.Digest == attempts.Attempts[1].Input.Digest {
		t.Fatalf("repair reused old execution: prepares=%d streams=%d attempts=%d %v", prepares.Load(), streams.Load(), len(attempts.Attempts), err)
	}
	for _, attempt := range attempts.Attempts {
		if _, err := s.Attempts().Request(ctx, attempt.ID); err != nil {
			t.Fatal(err)
		}
	}
	view, err := s.ReadContext(ctx)
	raw, _ := json.Marshal(view.Request)
	if err != nil || !strings.Contains(string(raw), "opaque-original") {
		t.Fatal("repair changed durable history")
	}
}

func TestExecutionDoesNotBypassRejectingValidatorOrEstimator(t *testing.T) {
	for _, boundary := range []string{"validator", "estimator"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			store := memory.New()
			model := &lifecycleExecutionModel{}
			failure := errors.New("rejected by " + boundary)
			var executions atomic.Int32
			model.prepare = func(context.Context, core.Request) (core.RequestExecution, error) { executions.Add(1); return nil, nil }
			model.validate = func(core.Request) error {
				if boundary == "validator" {
					return failure
				}
				return nil
			}
			s := lifecycleExecutionSession(t, store, model, func(context.Context, core.Model, core.Request) (int, error) {
				if boundary == "estimator" {
					return 0, failure
				}
				return 100, nil
			})
			if _, err := s.FollowUp(ctx, core.Input{ID: "reject-execution", Message: core.User("pending")}); err != nil {
				t.Fatal(err)
			}
			run, err := s.Continue(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = run.Wait(ctx); !errors.Is(err, failure) {
				t.Fatalf("custom hook was bypassed: %v", err)
			}
			receipt, err := s.LookupInput(ctx, "reject-execution")
			if err != nil || receipt.State != core.InputPending || executions.Load() != 0 || model.legacy.Load() != 0 {
				t.Fatalf("rejected hook reached execution or consumed input: %+v %v", receipt, err)
			}
		})
	}
}
