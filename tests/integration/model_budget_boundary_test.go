package integration_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/provider"
	"github.com/oines/astral-core/store/memory"
)

type rejectedBudgetTransport struct{ calls *atomic.Int32 }

func (transport rejectedBudgetTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	return nil, errors.New("Loop dispatched a request without known token budgets")
}

func TestModelsDirectConfigurationRequiresBudgetsOnlyAtLoopBoundary(t *testing.T) {
	for _, scenario := range []struct {
		name string
		spec core.ModelSpec
	}{
		{"bare model ID", core.ModelSpec{}},
		{"unknown context window", core.ModelSpec{ID: "window-unknown", MaxOutputTokens: 4096}},
		{"unknown output maximum", core.ModelSpec{ID: "output-unknown", ContextWindow: 128000}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			var networkCalls, resolutions atomic.Int32
			adapter, err := provider.New("openai-completions", models.Config{
				Model: "new-unlisted-model", Spec: scenario.spec, BaseURL: "https://model.invalid",
				HTTPClient: &http.Client{Transport: rejectedBudgetTransport{calls: &networkCalls}},
			})
			if err != nil {
				t.Fatal("direct adapter construction required agent-loop budget metadata:", err)
			}
			if adapter.Spec().ContextWindow != scenario.spec.ContextWindow || adapter.Spec().MaxOutputTokens != scenario.spec.MaxOutputTokens {
				t.Fatalf("Models invented unknown model budgets: %+v", adapter.Spec())
			}
			if scenario.spec.ID == "" && adapter.Spec().ID != "new-unlisted-model" {
				t.Fatal("bare direct adapter did not retain the remote model ID")
			}

			// Fixed-model agent sessions require complete budgets immediately.
			config := testConfig(adapter, memory.New())
			fixed, err := core.NewSession(ctx, config, core.SessionOptions{})
			if !errors.Is(err, core.ErrInvalid) {
				if fixed != nil {
					_ = fixed.Close(ctx)
				}
				t.Fatalf("fixed Loop accepted unknown budgets: %v", err)
			}

			// Dynamic sessions can be created and inspected without resolving a
			// model; the complete budget is required at the execution boundary.
			config.Model = nil
			config.Selection = core.ModelSelection{ID: "configured-reference"}
			config.ResolveModel = func(context.Context, core.ModelSelection) (core.ResolvedModel, error) {
				resolutions.Add(1)
				return core.ResolvedModel{Model: adapter}, nil
			}
			session, err := core.NewSession(ctx, config, core.SessionOptions{})
			if err != nil || resolutions.Load() != 0 {
				t.Fatalf("creation eagerly resolved a target: %v", err)
			}
			id := session.ID()
			if err = session.Close(ctx); err != nil {
				t.Fatal(err)
			}
			cold, err := core.OpenSession(ctx, config, id)
			if err != nil {
				t.Fatal("unknown budget blocked cold history:", err)
			}
			defer cold.Close(ctx)
			if _, err = cold.ReadContext(ctx); err != nil || resolutions.Load() != 0 {
				t.Fatalf("cold history resolved unknown budgets: %v", err)
			}
			run, err := prompt(cold, ctx, core.User("start agent execution"))
			if err == nil {
				_, err = run.Wait(ctx)
			}
			if !errors.Is(err, core.ErrInvalid) || resolutions.Load() != 1 || networkCalls.Load() != 0 {
				t.Fatalf("fresh Loop did not reject unknown budgets before dispatch: err=%v resolutions=%d HTTP=%d", err, resolutions.Load(), networkCalls.Load())
			}
		})
	}
}
