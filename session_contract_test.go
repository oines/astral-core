package astral_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/memory"
)

type contractModel struct {
	calls atomic.Int32
	enter chan struct{}
	gate  chan struct{}
}

func (m *contractModel) Spec() core.ModelSpec {
	return core.ModelSpec{ID: "contract", ContextWindow: 32000, MaxOutputTokens: 1024}
}
func (m *contractModel) Stream(ctx context.Context, _ core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
	m.calls.Add(1)
	if m.enter != nil {
		select {
		case m.enter <- struct{}{}:
		default:
		}
	}
	if m.gate != nil {
		select {
		case <-m.gate:
		case <-ctx.Done():
			return core.Reply{}, ctx.Err()
		}
	}
	return core.Reply{Message: core.Message{Role: core.RoleAssistant, Content: core.Text("done"), FinishReason: core.FinishStop}}, nil
}
func contractSession(t *testing.T, model *contractModel) *core.Session {
	t.Helper()
	store := memory.New()
	s, err := core.NewSession(context.Background(), core.Config{Model: model, Store: store, EstimateTokens: func(context.Context, core.Model, core.Request) (int, error) { return 100, nil }}, core.SessionOptions{})
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
func TestQueuedInputNeedsExplicitContinue(t *testing.T) {
	m := &contractModel{}
	s := contractSession(t, m)
	receipt, err := s.FollowUp(context.Background(), core.Input{ID: "one", Message: core.User("hello")})
	if err != nil || receipt.State != core.InputPending {
		t.Fatalf("receipt=%+v error=%v", receipt, err)
	}
	if m.calls.Load() != 0 {
		t.Fatal("queue activated model")
	}
	run, err := s.Continue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = run.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.calls.Load() != 1 {
		t.Fatalf("calls=%d", m.calls.Load())
	}
	if _, err = s.Continue(context.Background()); !errors.Is(err, core.ErrNotContinuable) {
		t.Fatalf("empty continue=%v", err)
	}
}
func TestPromptBusyAndDeduplication(t *testing.T) {
	m := &contractModel{enter: make(chan struct{}, 1), gate: make(chan struct{})}
	s := contractSession(t, m)
	in := core.Input{ID: "one", Message: core.User("hello")}
	result, err := s.Prompt(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.enter:
	case <-time.After(time.Second):
		t.Fatal("model did not start")
	}
	if _, err = s.Prompt(context.Background(), in); err != nil {
		t.Fatalf("duplicate while busy=%v", err)
	}
	if _, err = s.Prompt(context.Background(), core.Input{ID: "two", Message: core.User("new")}); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("prompt busy=%v", err)
	}
	if _, err = s.LookupInput(context.Background(), "two"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("busy prompt was accepted: %v", err)
	}
	in.Message = core.User("changed")
	if _, err = s.Prompt(context.Background(), in); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("digest conflict=%v", err)
	}
	close(m.gate)
	if _, err = result.Run.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestAcceptedRunOutlivesRequestContext(t *testing.T) {
	m := &contractModel{enter: make(chan struct{}, 1), gate: make(chan struct{})}
	s := contractSession(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	result, err := s.Prompt(ctx, core.Input{ID: "one", Message: core.User("hello")})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	close(m.gate)
	if _, err = result.Run.Wait(context.Background()); err != nil {
		t.Fatalf("HTTP cancellation stopped accepted work: %v", err)
	}
}
