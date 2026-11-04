package astral_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/memory"
)

func TestParallelDigestCancellationKeepsInputAndAttemptUncommitted(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	model := &lifecycleExecutionModel{}
	entered, release := make(chan struct{}), make(chan struct{})
	var validations, executions atomic.Int32
	model.validate = func(request core.Request) error {
		if len(request.Messages) > 0 && validations.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	model.prepare = func(context.Context, core.Request) (core.RequestExecution, error) {
		executions.Add(1)
		return nil, errors.New("cancelled preparation reached execution")
	}
	s := lifecycleExecutionSession(t, store, model, func(context.Context, core.Model, core.Request) (int, error) { return 100, nil })
	before := s.Head()
	if _, err := s.FollowUp(ctx, core.Input{ID: "parallel-cancel", Message: core.User(strings.Repeat("line\n中文 ", 8192))}); err != nil {
		t.Fatal(err)
	}
	run, err := s.Continue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request validation did not run")
	}
	run.Cancel()
	close(release) // Pure measurement must finish and join before returning.
	if _, err := run.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error changed: %v", err)
	}
	receipt, err := s.LookupInput(ctx, "parallel-cancel")
	attempts, attemptsErr := store.ScanAttempts(ctx, s.ID(), core.AttemptQuery{Limit: 10})
	if err != nil || attemptsErr != nil || receipt.State != core.InputPending || s.Head() != before || len(attempts.Attempts) != 0 || executions.Load() != 0 || model.legacy.Load() != 0 {
		t.Fatalf("cancelled measurement committed work: receipt=%+v attempts=%+v executions=%d err=%v/%v", receipt, attempts, executions.Load(), err, attemptsErr)
	}
}

func TestAnchoredExecutionDigestCancellationKeepsSelectedInputPending(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	model := &lifecycleExecutionModel{}
	entered := make(chan struct{})
	var prepares atomic.Int32
	model.prepare = func(ctx context.Context, _ core.Request) (core.RequestExecution, error) {
		if prepares.Add(1) == 1 {
			return core.RequestExecutionFunc(func(context.Context, func(core.ModelEvent)) (core.Reply, error) {
				reply := lifecycleExecutionReply()
				reply.Usage = core.Usage{Available: true, InputTokens: 100, OutputTokens: 10}
				return reply, nil
			}), nil
		}
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	var mu sync.Mutex
	var estimates []int
	s := lifecycleExecutionSession(t, store, model, func(_ context.Context, _ core.Model, request core.Request) (int, error) {
		mu.Lock()
		estimates = append(estimates, len(request.Messages))
		mu.Unlock()
		return 100, nil
	})
	first, err := s.Prompt(ctx, core.Input{ID: "anchored-seed", Message: core.User("first question")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Run.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	estimates = nil
	mu.Unlock()
	head := s.Head()
	if _, err := s.FollowUp(ctx, core.Input{ID: "anchored-cancel", Message: core.User("next question")}); err != nil {
		t.Fatal(err)
	}
	run, err := s.Continue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("anchored request did not reach wire preparation")
	}
	run.Cancel()
	if _, err := run.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("anchored cancellation changed: %v", err)
	}
	receipt, err := s.LookupInput(ctx, "anchored-cancel")
	attempts, attemptsErr := store.ScanAttempts(ctx, s.ID(), core.AttemptQuery{Limit: 10})
	if err != nil || attemptsErr != nil || receipt.State != core.InputPending || s.Head() != head || len(attempts.Attempts) != 1 || prepares.Load() != 2 || model.legacy.Load() != 0 {
		t.Fatalf("cancelled anchored preparation committed work: receipt=%+v attempts=%+v err=%v/%v", receipt, attempts, err, attemptsErr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(estimates) != 2 || estimates[0] != 0 || estimates[1] != 1 {
		t.Fatalf("fixture did not exercise the usage-anchor path: estimates=%v", estimates)
	}
}
