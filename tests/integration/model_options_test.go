package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	astral "github.com/oines/astral-core"

	"github.com/oines/astral-core/store/memory"
)

func frozenOptions() astral.GenerationOptions {
	enabled, budget, temperature := false, 0, 0.25
	return astral.GenerationOptions{
		Temperature: &temperature,
		Stop:        []string{"END"},
		Reasoning:   astral.ReasoningOptions{Enabled: &enabled, Effort: "vendor-max", BudgetTokens: &budget},
		ProviderOptions: map[string]json.RawMessage{
			"example": json.RawMessage(`{"native_mode":"opaque"}`),
		},
	}
}

func TestModelSelectionFreezesNextRequestOptionsDuringRun(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	m := model(func(_ context.Context, n int, req astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			close(entered)
			<-release
			if req.Options.Reasoning.Effort != "initial" {
				t.Error("in-flight request changed")
			}
		} else if req.Options.Reasoning.Effort != "selected" {
			t.Errorf("next request did not use selection: %+v", req.Options)
		}
		return answer("done"), nil
	})
	config := testConfig(m, memory.New())
	config.Options.Reasoning.Effort = "initial"
	s := newSession(t, newSessions(t, config), astral.SessionOptions{})
	run := start(t, s, "first")
	<-entered
	options := astral.GenerationOptions{Reasoning: astral.ReasoningOptions{Effort: "selected"}}
	if err := s.SelectModel(context.Background(), m.spec.ID, options); err != nil {
		close(release)
		t.Fatal(err)
	}
	options.Reasoning.Effort = "mutated"
	close(release)
	waitOK(t, run)
	waitOK(t, start(t, s, "second"))
}

func TestCompactionUsesIndependentFrozenOptionsAndReasoningReply(t *testing.T) {
	normal := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("normal answer"), nil
	})
	summary := model(func(_ context.Context, _ int, req astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if req.ModelRevision != "summary-v2" || req.Options.Reasoning.Effort != "high" {
			t.Fatalf("wrong summary request: %+v", req)
		}
		reply := answer("compressed history")
		reply.Message.Content = append([]astral.Content{{Type: "reasoning", Data: json.RawMessage(`{"encrypted":"opaque"}`)}}, reply.Message.Content...)
		return reply, nil
	})
	summary.spec.ID, summary.spec.Revision = "summary", "summary-v2"
	config := testConfig(normal, memory.New())
	config.Options.Reasoning.Effort = "normal-effort"
	config.ResolveModel = func(context.Context, string) (astral.Model, error) { return summary, nil }
	config.Compaction = astral.CompactionPolicy{ModelID: "summary", KeepRecentTokens: 1,
		Options: astral.GenerationOptions{Reasoning: astral.ReasoningOptions{Effort: "high"}}}
	s := newSession(t, newSessions(t, config), astral.SessionOptions{})
	waitOK(t, start(t, s, "first"))
	waitOK(t, start(t, s, "second"))
	record, err := s.Compact(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if record.Summary != "compressed history" || record.ModelRevision != "summary-v2" || record.Options.Reasoning.Effort != "high" {
		t.Fatalf("missing summary provenance: %+v", record)
	}
	if s.Configuration().Options.Reasoning.Effort != "normal-effort" {
		t.Fatal("summary options replaced the normal generation selection")
	}
}

func mutateOptions(o astral.GenerationOptions) {
	*o.Temperature = 1.5
	*o.Reasoning.Enabled = true
	*o.Reasoning.BudgetTokens = 1000
	o.Stop[0] = "CHANGED"
	o.ProviderOptions["example"][2] = 'X'
}

func TestStandaloneFreezesGenerationOptions(t *testing.T) {
	options := frozenOptions()
	m := model(func(_ context.Context, _ int, req astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if !reflect.DeepEqual(req.Options, frozenOptions()) || req.ModelRevision != "revision-1" {
			t.Fatalf("model request did not retain the selected parameters: %+v", req)
		}
		// A provider cannot mutate the configuration used by the next call.
		mutateOptions(req.Options)
		return answer("done"), nil
	})
	m.spec.Revision = "revision-1"
	a, err := newMemorySession(astral.Config{Model: m, Options: options})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	mutateOptions(options)
	for range 2 {
		run, err := prompt(a, context.Background(), astral.User("go"))
		if err != nil {
			t.Fatal(err)
		}
		waitOK(t, run)
	}
}

func TestPersistentGenerationOptionsSurviveConfigurationAndReopen(t *testing.T) {
	eachStore(t, func(t *testing.T, store astral.Store) {
		options := frozenOptions()
		m := model(func(_ context.Context, _ int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			return answer("done"), nil
		})
		m.spec.Revision = "revision-1"
		config := testConfig(m, store)
		config.Options = options
		manager := newSessions(t, config)
		s := newSession(t, manager, astral.SessionOptions{})
		mutateOptions(options)
		waitOK(t, start(t, s, "first"))
		if got := m.observed()[0]; !reflect.DeepEqual(got.Options, frozenOptions()) || got.ModelRevision != "revision-1" {
			t.Fatalf("defaults were not frozen: %+v", got)
		}
		selected := s.Configuration()
		selected.Options.Reasoning.Effort = "vendor-ultra"
		if err := s.Configure(context.Background(), selected); err != nil {
			t.Fatal(err)
		}
		mutateOptions(selected.Options)
		id := s.ID()
		if err := manager.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		// Different manager defaults must not overwrite the committed selection.
		config.Options = astral.GenerationOptions{}
		reopenedManager := newSessions(t, config)
		reopened, err := reopenedManager.OpenSession(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		waitOK(t, start(t, reopened, "second"))
		want := frozenOptions()
		want.Reasoning.Effort = "vendor-ultra"
		if got := m.observed()[1].Options; !reflect.DeepEqual(got, want) {
			t.Fatalf("reopened options = %+v; want %+v", got, want)
		}
		view, err := reopened.InspectContext(context.Background())
		if err != nil || !reflect.DeepEqual(view.Request.Options, want) {
			t.Fatalf("context inspection lost selected options: %+v, %v", view, err)
		}
	})
}

func TestModelInputLimitIsCheckedIndependentlyOfContextWindow(t *testing.T) {
	eachStore(t, func(t *testing.T, store astral.Store) {
		m := model(func(_ context.Context, _ int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			t.Fatal("provider called after input limit was exceeded")
			return astral.Reply{}, nil
		})
		m.spec.ContextWindow = 10000
		m.spec.MaxInputTokens = 1000
		m.estimator = func(astral.Request) (int, error) { return 900, nil }
		config := testConfig(m, store)
		config.Budget.SafetyMargin = 200
		s := newSession(t, newSessions(t, config), astral.SessionOptions{})
		run, err := prompt(s, context.Background(), astral.User("go"))
		if err == nil {
			_, err = run.Wait(context.Background())
		}
		if !errors.Is(err, astral.ErrBudget) || len(m.observed()) != 0 {
			t.Fatalf("input budget was not enforced: %v", err)
		}
	})
}
