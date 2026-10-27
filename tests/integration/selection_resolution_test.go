package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/memory"
)

func TestColdHistoryAndHistoricalCloneNeedNoModelResolution(t *testing.T) {
	eachStore(t, func(t *testing.T, store core.Store) {
		ctx := context.Background()
		m := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
			return answer("saved history"), nil
		})
		config := testConfig(m, store)
		config.Selection = core.ModelSelection{ID: m.spec.ID, Reasoning: &core.ReasoningOptions{Effort: "historical"}}
		manager := newSessions(t, config)
		session := newSession(t, manager, core.SessionOptions{ID: "parent"})
		waitOK(t, start(t, session, "original"))
		historicalHead := session.Head()
		if err := session.SelectModel(ctx, core.ModelSelection{ID: "newer-choice", Reasoning: &core.ReasoningOptions{Effort: "future"}}); err != nil {
			t.Fatal(err)
		}
		if err := session.Close(ctx); err != nil {
			t.Fatal(err)
		}

		config.Model = nil
		config.ResolveModel = func(context.Context, core.ModelSelection) (core.ResolvedModel, error) {
			t.Fatal("history operation attempted to resolve a removed model")
			return core.ResolvedModel{}, errors.New("model removed")
		}
		cold, err := core.OpenSession(ctx, config, "parent")
		if err != nil {
			t.Fatal(err)
		}
		defer cold.Close(ctx)
		view, err := cold.ReadContext(ctx)
		if err != nil || len(view.Request.Messages) != 2 || view.Selection.ID != "newer-choice" {
			t.Fatalf("cold active history: %+v %v", view, err)
		}
		if err = cold.SelectModel(ctx, core.ModelSelection{ID: "offline-choice"}); err != nil {
			t.Fatal(err)
		}
		fork, err := cold.Fork(ctx, historicalHead, core.ForkOptions{ID: "historical-clone", Mode: core.ForkDetached})
		if err != nil {
			t.Fatal(err)
		}
		defer fork.Close(ctx)
		selection := fork.Configuration().Selection
		if selection.ID != m.spec.ID || selection.Reasoning == nil || selection.Reasoning.Effort != "historical" {
			t.Fatalf("clone inherited a future selection: %+v", selection)
		}
		cloneView, err := fork.ReadContext(ctx)
		if err != nil || len(cloneView.Request.Messages) != 2 {
			t.Fatalf("clone history: %+v %v", cloneView, err)
		}
		if cold.Configuration().Selection.ID != "offline-choice" {
			t.Fatal("clone modified the parent choice")
		}
	})
}

func TestFrozenTargetRestoresWithoutFreshDefaults(t *testing.T) {
	eachStore(t, func(t *testing.T, store core.Store) {
		ctx := context.Background()
		batch := append(storeHeader(), storeMessage("input", "config"))
		if err := appendStore(ctx, store, "session", 0, batch); err != nil {
			t.Fatal(err)
		}
		base, err := core.ReadContext(ctx, store, "session", "input", core.DefaultContextBytes)
		if err != nil {
			t.Fatal(err)
		}
		old := model(func(_ context.Context, n int, request core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
			if request.ModelID != "resolved-v1" || request.Options.Reasoning.Effort != "old-default" {
				t.Fatalf("frozen request changed: %+v", request)
			}
			if n == 1 {
				return core.Reply{}, &core.ModelError{Code: "temporary", Transient: true}
			}
			return answer("restored"), nil
		})
		old.spec.ID, old.spec.Revision = "resolved-v1", "revision-1"
		request := base.Request
		request.ModelID, request.ModelRevision = old.spec.ID, old.spec.Revision
		request.MaxOutputTokens = 100
		request.Options.Reasoning.Effort = "old-default"
		input, err := core.FreezeRequest("session", base, request, old.spec)
		if err != nil {
			t.Fatal(err)
		}
		input.Snapshot = json.RawMessage(`{"resolved_configuration":"version-1"}`)
		attempt := core.ModelAttempt{ID: "pending", SessionID: "session", Purpose: "generation", Number: 1, Version: 2, StartedAt: time.Now().UTC(), Input: input}
		if _, err = store.Commit(ctx, "session", core.Commit{ID: "frozen-boundary", ExpectedVersion: 1, Attempts: []core.AttemptMutation{{ID: attempt.ID, Create: &attempt}}}); err != nil {
			t.Fatal(err)
		}
		var freshCalls, restoreCalls atomic.Int32
		next := model(func(_ context.Context, _ int, request core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
			if request.Options.Reasoning.Effort != "new-default" {
				t.Fatalf("fresh defaults not applied: %+v", request.Options)
			}
			return answer("new target"), nil
		})
		next.spec.ID = "resolved-v2"
		config := testConfig(nil, store)
		config.ResolveModel = func(_ context.Context, selection core.ModelSelection) (core.ResolvedModel, error) {
			freshCalls.Add(1)
			if selection.ID != "future-choice" {
				t.Fatalf("unexpected selection: %+v", selection)
			}
			return core.ResolvedModel{Model: next, Options: core.GenerationOptions{Reasoning: core.ReasoningOptions{Effort: "new-default"}}, Snapshot: json.RawMessage(`{"resolved_configuration":"version-2"}`)}, nil
		}
		config.RestoreModel = func(_ context.Context, snapshot core.ModelSnapshot) (core.Model, error) {
			restoreCalls.Add(1)
			if snapshot.Spec != old.spec || string(snapshot.Data) != string(input.Snapshot) {
				t.Fatalf("target snapshot changed: %+v", snapshot)
			}
			return old, nil
		}
		config.Retry = core.RetryPolicy{MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}
		session, err := core.OpenSession(ctx, config, "session")
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close(ctx)
		if err = session.SelectModel(ctx, core.ModelSelection{ID: "future-choice"}); err != nil {
			t.Fatal(err)
		}
		run, err := session.Continue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		waitOK(t, run)
		if freshCalls.Load() != 0 || restoreCalls.Load() != 1 || len(old.observed()) != 2 {
			t.Fatalf("restoration re-resolved defaults or retry target: fresh=%d restore=%d calls=%d", freshCalls.Load(), restoreCalls.Load(), len(old.observed()))
		}
		if !reflect.DeepEqual(old.observed()[0], old.observed()[1]) {
			t.Fatal("ordinary retry changed frozen input")
		}
		for _, recorded := range allAttempts(t, session) {
			if string(recorded.Input.Snapshot) != string(input.Snapshot) {
				t.Fatal("ordinary retry lost frozen target configuration")
			}
		}
		waitOK(t, start(t, session, "next"))
		if freshCalls.Load() != 1 || len(next.observed()) != 1 {
			t.Fatal("new request did not resolve the latest selection")
		}
	})
}

func TestSelectionBlockedDuringPreparationAndClearsOldOverrides(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	base := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
		return answer("done"), nil
	})
	m := preparedModel{testModel: base, prepare: func(ctx context.Context, request core.Request) (core.Request, error) {
		close(entered)
		select {
		case <-release:
			return request, nil
		case <-ctx.Done():
			return core.Request{}, ctx.Err()
		}
	}}
	config := testConfig(m, memory.New())
	config.Selection = core.ModelSelection{ID: base.spec.ID, Reasoning: &core.ReasoningOptions{Effort: "old"}}
	session := newSession(t, newSessions(t, config), core.SessionOptions{})
	run := start(t, session, "go")
	signal(t, entered)
	if err := session.SelectModel(context.Background(), core.ModelSelection{ID: "next"}); !errors.Is(err, core.ErrBusy) {
		close(release)
		t.Fatalf("selection during preparation: %v", err)
	}
	close(release)
	waitOK(t, run)
	if err := session.SelectModel(context.Background(), core.ModelSelection{ID: "not-currently-configured"}); err != nil {
		t.Fatal(err)
	}
	if selection := session.Configuration().Selection; selection.Reasoning != nil || !reflect.DeepEqual(selection.Options, core.GenerationOptions{}) {
		t.Fatalf("new model inherited old overrides: %+v", selection)
	}
}

func TestUnavailableNextModelPreservesCompletedToolOutput(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var executions atomic.Int32
	m := model(func(_ context.Context, n int, _ core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
		if n == 1 {
			return calls(call("effect", "effect", `{"value":"x"}`)), nil
		}
		return answer("continued"), nil
	})
	missing := errors.New("selected connection is unavailable")
	config := testConfig(m, memory.New())
	config.ResolveModel = func(context.Context, core.ModelSelection) (core.ResolvedModel, error) {
		return core.ResolvedModel{}, missing
	}
	config.Tools = []core.Tool{tool("effect", func(_ context.Context, _ core.Invocation, _ func(core.ToolProgress)) (core.ToolExecution, error) {
		executions.Add(1)
		close(entered)
		<-release
		return core.ToolExecution{Result: &core.ToolResult{Content: core.Text("valuable completed output"), Artifact: "artifact://completed"}}, nil
	})}
	session := newSession(t, newSessions(t, config), core.SessionOptions{})
	run := start(t, session, "execute")
	signal(t, entered)
	if err := session.SelectModel(context.Background(), core.ModelSelection{ID: "removed-connection"}); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if _, err := wait(t, run); !errors.Is(err, missing) {
		t.Fatalf("next request did not report unavailable target: %v", err)
	}
	results := toolResults(session)
	if len(results) != 1 || results[0].Content[0].Text != "valuable completed output" || results[0].Outcome != core.OutcomeSucceeded || results[0].IsError || results[0].Code != "" || results[0].Artifact != "artifact://completed" {
		t.Fatalf("model selection destroyed completed output: %+v", results)
	}
	if err := session.SelectModel(context.Background(), core.ModelSelection{ID: m.spec.ID}); err != nil {
		t.Fatal(err)
	}
	run, err := session.Continue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitOK(t, run)
	if executions.Load() != 1 || len(m.observed()) != 2 {
		t.Fatal("model recovery repeated an already completed tool")
	}
}

func TestPreparedContextRemapsSourcesAfterOmittingPrivateMessages(t *testing.T) {
	old := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
		return core.Reply{Message: core.Message{Role: core.RoleAssistant, FinishReason: core.FinishStop, Content: []core.Content{{Type: "reasoning", Data: json.RawMessage(`{"encrypted":"original-private-content"}`)}}}}, nil
	})
	nextBase := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
		return answer("next answer"), nil
	})
	nextBase.spec.ID = "other-target"
	next := preparedModel{testModel: nextBase, prepare: func(_ context.Context, request core.Request) (core.Request, error) {
		messages := request.Messages[:0]
		for _, message := range request.Messages {
			if message.Role == core.RoleAssistant && len(message.Content) == 1 && message.Content[0].Type == "reasoning" {
				continue
			}
			messages = append(messages, message)
		}
		request.Messages = messages
		return request, nil
	}}
	config := testConfig(old, memory.New())
	config.ResolveModel = func(context.Context, core.ModelSelection) (core.ResolvedModel, error) {
		return core.ResolvedModel{Model: next}, nil
	}
	session := newSession(t, newSessions(t, config), core.SessionOptions{})
	waitOK(t, start(t, session, "original input"))
	original, err := session.ReadContext(context.Background())
	if err != nil || len(original.Request.Messages) != 2 {
		t.Fatalf("original context: %+v %v", original, err)
	}
	if err = session.SelectModel(context.Background(), core.ModelSelection{ID: "other-reference"}); err != nil {
		t.Fatal(err)
	}
	prepared, err := session.InspectContext(context.Background())
	if err != nil || len(prepared.Request.Messages) != 1 || len(prepared.Sources) != 1 || prepared.Sources[0] != original.Sources[0] {
		t.Fatalf("prepared sources no longer identify their messages: %+v %v", prepared, err)
	}
	unchanged, err := session.ReadContext(context.Background())
	if err != nil || len(unchanged.Request.Messages) != 2 || !reflect.DeepEqual(unchanged.Request.Messages, original.Request.Messages) {
		t.Fatalf("target preparation changed canonical history: %+v %v", unchanged, err)
	}
	waitOK(t, start(t, session, "next input"))
	attempts := allAttempts(t, session)
	actual, err := session.Attempts().Request(context.Background(), attempts[len(attempts)-1].ID)
	if err != nil || len(actual.Messages) != 2 || actual.Messages[0].Role != core.RoleUser || actual.Messages[1].Role != core.RoleUser {
		t.Fatalf("frozen target projection did not reconstruct: %+v %v", actual, err)
	}
	canonical, err := session.ReadContext(context.Background())
	if err != nil || string(canonical.Request.Messages[1].Content[0].Data) != `{"encrypted":"original-private-content"}` {
		t.Fatal("frozen target projection removed private content from history")
	}
}

type configurationLookupStore struct {
	core.Store
	configurationID string
	stateReads      int
	nodeReads       int
}

func (s *configurationLookupStore) LoadState(ctx context.Context, id string) (core.RuntimeState, error) {
	s.stateReads++
	return s.Store.LoadState(ctx, id)
}
func (s *configurationLookupStore) Get(ctx context.Context, rolloutID, id string) (core.Record, error) {
	s.nodeReads++
	if id != s.configurationID {
		return core.Record{}, errors.New("configuration lookup read a conversation body")
	}
	return s.Store.Get(ctx, rolloutID, id)
}
func (s *configurationLookupStore) Scan(context.Context, string, core.Query) (core.Page, error) {
	return core.Page{}, errors.New("configuration lookup scanned the active history window")
}

func TestReadConfigurationNeedsOnlyStateAndOneConfigurationNode(t *testing.T) {
	eachStore(t, func(t *testing.T, store core.Store) {
		m := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
			return answer("conversation body must not be read"), nil
		})
		config := testConfig(m, store)
		config.Selection = core.ModelSelection{ID: m.spec.ID, Reasoning: &core.ReasoningOptions{Effort: "historical"}}
		session := newSession(t, newSessions(t, config), core.SessionOptions{})
		waitOK(t, start(t, session, "stored conversation"))
		state := session.State()
		readOnly := &configurationLookupStore{Store: store, configurationID: state.Branch.ConfigurationID}
		selected, err := core.ReadConfiguration(context.Background(), readOnly, session.ID())
		if err != nil || readOnly.stateReads != 1 || readOnly.nodeReads != 1 || selected.Selection.Reasoning == nil || selected.Selection.Reasoning.Effort != "historical" {
			t.Fatalf("configuration lookup loaded more than its references: %+v state=%d nodes=%d err=%v", selected, readOnly.stateReads, readOnly.nodeReads, err)
		}
		selected.Selection.Reasoning.Effort = "mutated"
		again, err := core.ReadConfiguration(context.Background(), readOnly, session.ID())
		if err != nil || again.Selection.Reasoning.Effort != "historical" {
			t.Fatal("configuration lookup exposed mutable persistent state")
		}
	})
}
