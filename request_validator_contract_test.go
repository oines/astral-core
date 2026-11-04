package astral_test

import (
	"context"
	"sync/atomic"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/memory"
)

func TestReadOnlyForwarderCallsConfiguredLegacyValidatorOnce(t *testing.T) {
	ctx := context.Background()
	model := &lifecycleExecutionModel{}
	var validates, prepares atomic.Int32
	model.validate = func(request core.Request) error {
		validates.Add(1)
		request.Messages[0].Content[0].Text = "legacy private mutation"
		return nil
	}
	model.prepare = func(_ context.Context, request core.Request) (core.RequestExecution, error) {
		prepares.Add(1)
		if validates.Load() != 1 || request.Messages[0].Content[0].Text != "unchanged frozen input" {
			t.Error("configured validator was skipped/repeated or mutated frozen input")
		}
		return core.RequestExecutionFunc(func(context.Context, func(core.ModelEvent)) (core.Reply, error) {
			return lifecycleExecutionReply(), nil
		}), nil
	}
	s := lifecycleExecutionSession(t, memory.New(), model, func(context.Context, core.Model, core.Request) (int, error) { return 100, nil })
	prompt, err := s.Prompt(ctx, core.Input{ID: "readonly-validation-contract", Message: core.User("unchanged frozen input")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prompt.Run.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if validates.Load() != 1 || prepares.Load() != 1 {
		t.Fatalf("final validation/execution counts: validate=%d prepare=%d", validates.Load(), prepares.Load())
	}
}
