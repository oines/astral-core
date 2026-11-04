package astral_test

import (
	"context"
	"errors"
	"strings"
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
