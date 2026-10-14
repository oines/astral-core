package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	astral "github.com/oines/astral-core"
	environments "github.com/oines/astral-core/environment"
	sessions "github.com/oines/astral-core/session"
)

const privateBindingMarker = "HOST_BINDING_SECRET"

func environmentTexts(request astral.Request) []string {
	var out []string
	for _, message := range request.Messages {
		if message.Role == astral.RoleContext && message.ContextKind == "environment" {
			for _, part := range message.Content {
				out = append(out, part.Text)
			}
		}
	}
	return out
}

func assertNoBindingLeak(t *testing.T, request astral.Request) {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil || bytes.Contains(raw, []byte(privateBindingMarker)) {
		t.Fatalf("private execution metadata reached the model: %s %v", raw, err)
	}
}

func TestEnvironmentAcceptsOpaqueJSONAndIsolatesBindings(t *testing.T) {
	eachStore(t, func(t *testing.T, store sessions.Store) {
		for _, raw := range []string{`{"custom":{"credential":"HOST_BINDING_SECRET"}}`, `["HOST_BINDING_SECRET",{"custom":7}]`, `"HOST_BINDING_SECRET"`, `42`, `true`, `null`} {
			t.Run(raw, func(t *testing.T) {
				ctx := context.Background()
				var received json.RawMessage
				m := model(func(_ context.Context, n int, request astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
					assertNoBindingLeak(t, request)
					if n == 1 {
						return calls(call("inspect", "inspect", `{"value":"x"}`)), nil
					}
					return answer("done"), nil
				})
				c := testConfig(m, store)
				c.SystemPrompt = "Stable harness rules."
				c.Tools = []astral.Tool{tool("inspect", func(_ context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolResult, error) {
					received = append(json.RawMessage(nil), inv.Binding...)
					inv.Binding[0] = 'X'
					return astral.ToolResult{Content: astral.Text("inspected")}, nil
				})}
				a := newAgent(t, c)
				content := "# Host environment\nAGENTS.md and SKILL.md are source material."
				snapshot := environments.Snapshot{Binding: json.RawMessage(raw), Context: content}
				s := newSession(t, a, sessions.SessionOptions{Environment: &snapshot})
				snapshot.Binding[0] = 'X'
				snapshot.Context = "changed by host"
				view, err := s.InspectContext(ctx)
				if err != nil || string(view.Binding) != raw || view.SnapshotID != s.State().Branch.EnvironmentID || !reflect.DeepEqual(environmentTexts(view.Request), []string{content}) {
					t.Fatalf("initial environment: %+v %v", view, err)
				}
				view.Binding[0] = 'X'
				view.Environment.Binding[0] = 'X'
				waitOK(t, start(t, s, "inspect"))
				if string(received) != raw {
					t.Fatalf("invocation received %s, want %s", received, raw)
				}
				for _, request := range m.observed() {
					if request.SystemPrompt != c.SystemPrompt || !reflect.DeepEqual(environmentTexts(request), []string{content}) {
						t.Fatal("host context changed system instructions or history position")
					}
				}
				for _, entry := range entries(s) {
					if entry.Environment != nil && (string(entry.Environment.Binding) != raw || entry.Environment.Context != content) {
						t.Fatal("host or tool mutation contaminated environment history")
					}
					if entry.Tool != nil && string(entry.Tool.Invocation.Binding) != raw {
						t.Fatal("tool mutation contaminated its persisted binding")
					}
				}
			})
		}
	})
}

func TestFailedEnvironmentChangesKeepEffectiveSnapshot(t *testing.T) {
	eachStore(t, func(t *testing.T, store sessions.Store) {
		ctx := context.Background()
		initial := hostSnapshot("original", "location")
		var received hostBinding
		m := model(func(_ context.Context, n int, request astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			if n == 1 {
				return calls(call("deny", "switch_environment", `{}`), call("inspect", "inspect", `{"value":"x"}`)), nil
			}
			if !reflect.DeepEqual(environmentTexts(request), []string{initial.Context}) {
				t.Fatal("failed switch changed model-visible environment")
			}
			return answer("done"), nil
		})
		c := testConfig(m, store)
		c.Tools = []astral.Tool{
			switchEnvironment(func(context.Context, astral.Invocation) (environments.Snapshot, error) {
				return hostSnapshot("denied", "location"), errors.New("host denied target")
			}),
			tool("inspect", func(_ context.Context, inv astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolResult, error) {
				received = bindingOf(inv.Binding)
				return astral.ToolResult{Content: astral.Text("inspected")}, nil
			}),
		}
		a := newAgent(t, c)
		s := newSession(t, a, sessions.SessionOptions{Environment: &initial})
		before := s.State()
		invalid := environments.Snapshot{Binding: json.RawMessage(`{"broken":`), Context: "invalid"}
		if err := s.RefreshEnvironment(ctx, invalid); err == nil || !reflect.DeepEqual(before, s.State()) {
			t.Fatal("invalid binding changed runtime state")
		}
		if _, err := a.NewSession(ctx, sessions.SessionOptions{ID: "invalid-environment", Environment: &invalid}); err == nil {
			t.Fatal("new session accepted invalid binding")
		}
		if _, err := store.LoadState(ctx, "invalid-environment"); !errors.Is(err, sessions.ErrNotFound) {
			t.Fatal("invalid environment was partially published")
		}
		waitOK(t, start(t, s, "switch"))
		if received != bindingOf(initial.Binding) || s.State().Branch.EnvironmentID != before.Branch.EnvironmentID {
			t.Fatal("failed switch contaminated later invocation binding")
		}
		results := toolResults(s)
		if len(results) != 2 || !results[0].IsError || len(results[0].Context) != 0 {
			t.Fatalf("failed switch result: %+v", results)
		}
	})
}

func TestEnvironmentCompactionForkAndReopenAcrossStores(t *testing.T) {
	eachStore(t, func(t *testing.T, store sessions.Store) {
		ctx := context.Background()
		m := model(func(_ context.Context, _ int, request astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			assertNoBindingLeak(t, request)
			if request.SystemPrompt == "summary" {
				return answer("Completed conversation."), nil
			}
			return answer("done"), nil
		})
		c := testConfig(m, store)
		c.SystemPrompt = "Stable harness rules."
		c.Compaction = sessions.CompactionPolicy{Disabled: true, SystemPrompt: "summary", KeepRecentTokens: 1, OutputTokens: 128}
		a := newAgent(t, c)
		firstSnapshot := environments.Snapshot{Binding: json.RawMessage(`{"private":"HOST_BINDING_SECRET","chosen":1}`), Context: "First host context."}
		secondSnapshot := environments.Snapshot{Binding: json.RawMessage(`["HOST_BINDING_SECRET",2]`), Context: "Second host context."}
		s := newSession(t, a, sessions.SessionOptions{Environment: &firstSnapshot})
		first := waitOK(t, start(t, s, "first"))
		if err := s.RefreshEnvironment(ctx, secondSnapshot); err != nil {
			t.Fatal(err)
		}
		environmentBoundary := s.Head()
		second := waitOK(t, start(t, s, "second"))
		if _, err := s.Compact(ctx, ""); err != nil {
			t.Fatal(err)
		}
		view, err := s.InspectContext(ctx)
		if err != nil || string(view.Binding) != string(secondSnapshot.Binding) || view.Request.Messages[0].ContextKind != "summary" || !reflect.DeepEqual(environmentTexts(view.Request), []string{secondSnapshot.Context}) {
			t.Fatalf("compaction lost environment: %+v %v", view, err)
		}
		for _, mode := range []sessions.ForkMode{sessions.ForkShared, sessions.ForkDetached} {
			for i, boundary := range []struct {
				id       string
				snapshot environments.Snapshot
			}{{first.LastMessageID, firstSnapshot}, {environmentBoundary, secondSnapshot}, {second.LastMessageID, secondSnapshot}, {s.Head(), secondSnapshot}} {
				child, err := s.Fork(ctx, boundary.id, sessions.ForkOptions{ID: fmt.Sprintf("environment-fork-%d-%d", mode, i), Mode: mode})
				if err != nil {
					t.Fatal(err)
				}
				forkView, err := child.InspectContext(ctx)
				contexts := environmentTexts(forkView.Request)
				if err != nil || string(forkView.Binding) != string(boundary.snapshot.Binding) || len(contexts) == 0 || contexts[len(contexts)-1] != boundary.snapshot.Context {
					t.Fatalf("fork inherited wrong environment: %+v %v", forkView, err)
				}
				assertNoBindingLeak(t, forkView.Request)
				if _, err := child.Continue(ctx); !errors.Is(err, sessions.ErrEnvironmentRefresh) {
					t.Fatalf("fork trusted historical binding: %v", err)
				}
				confirmed := hostSnapshot("confirmed", "")
				if err := child.RefreshEnvironment(ctx, confirmed); err != nil {
					t.Fatal(err)
				}
				waitOK(t, start(t, child, "new input"))
			}
		}
		id := s.ID()
		if err := s.Close(ctx); err != nil {
			t.Fatal(err)
		}
		s, err = a.OpenSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		reopened, err := s.InspectContext(ctx)
		if err != nil || !reflect.DeepEqual(view, reopened) {
			t.Fatalf("persisted environment changed on reopen: %v", err)
		}
		if _, err := s.Start(ctx, astral.User("unconfirmed")); !errors.Is(err, sessions.ErrEnvironmentRefresh) {
			t.Fatalf("reopen accepted historical binding: %v", err)
		}
		if err := a.Close(ctx); err != nil {
			t.Fatal(err)
		}
		refreshes := 0
		c.Environment = func(_ context.Context, request environments.Request) (environments.Snapshot, error) {
			refreshes++
			if request.Previous == nil || string(request.Previous.Binding) != string(secondSnapshot.Binding) {
				t.Fatal("provider lost previous opaque binding")
			}
			request.Previous.Binding[0] = 'X'
			return hostSnapshot("provider-confirmed", ""), nil
		}
		s, err = newAgent(t, c).OpenSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		waitOK(t, start(t, s, "confirmed input"))
		if refreshes != 1 {
			t.Fatalf("provider confirmations: %d", refreshes)
		}
		for _, entry := range entries(s) {
			if entry.Environment != nil && strings.Contains(entry.Environment.Context, "Second host") && string(entry.Environment.Binding) != string(secondSnapshot.Binding) {
				t.Fatal("provider mutated historical binding")
			}
		}
	})
}

func TestEnvironmentSnapshotsSurviveColdStoreReplay(t *testing.T) {
	for _, backend := range []string{"jsonl", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store, closeStore := durableStore(t, backend, dir)
			initial := environments.Snapshot{Binding: json.RawMessage(`["HOST_BINDING_SECRET",1]`), Context: "Initial host context."}
			changed := environments.Snapshot{Binding: json.RawMessage(`{"custom":"HOST_BINDING_SECRET","selected":2}`), Context: "Changed host context."}
			effects := 0
			m := model(func(_ context.Context, _ int, request astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
				assertNoBindingLeak(t, request)
				if request.SystemPrompt == "summary" {
					return answer("Summary of completed work."), nil
				}
				if len(texts(request.Messages, astral.RoleTool)) == 0 {
					return calls(call("change", "switch_environment", `{}`)), nil
				}
				return answer("done"), nil
			})
			c := testConfig(m, store)
			c.Compaction = sessions.CompactionPolicy{Disabled: true, SystemPrompt: "summary", KeepRecentTokens: 1, OutputTokens: 128}
			c.Tools = []astral.Tool{switchEnvironment(func(context.Context, astral.Invocation) (environments.Snapshot, error) {
				effects++
				return changed, nil
			})}
			a := newAgent(t, c)
			parent := newSession(t, a, sessions.SessionOptions{ID: "environment-parent", Environment: &initial})
			waitOK(t, start(t, parent, "change"))
			var resultID string
			for _, entry := range entries(parent) {
				if entry.Kind == sessions.EntryToolResult {
					resultID = entry.ID
				}
			}
			for _, mode := range []sessions.ForkMode{sessions.ForkShared, sessions.ForkDetached} {
				if _, err := parent.Fork(ctx, resultID, sessions.ForkOptions{ID: fmt.Sprintf("environment-child-%d", mode), Mode: mode}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := parent.Compact(ctx, ""); err != nil {
				t.Fatal(err)
			}
			if err := a.Close(ctx); err != nil {
				t.Fatal(err)
			}
			closeStore()
			store, closeStore = durableStore(t, backend, dir)
			defer closeStore()
			c.Store = store
			a = newAgent(t, c)
			for _, id := range []string{"environment-parent", "environment-child-0", "environment-child-1"} {
				s, err := a.OpenSession(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				view, err := s.InspectContext(ctx)
				contexts := environmentTexts(view.Request)
				if err != nil || string(view.Binding) != string(changed.Binding) || len(contexts) == 0 || contexts[len(contexts)-1] != changed.Context {
					t.Fatalf("cold environment for %s: %+v %v", id, view, err)
				}
				assertNoBindingLeak(t, view.Request)
				if strings.HasPrefix(id, "environment-child") && view.SnapshotID != resultID {
					t.Fatal("cold result-boundary snapshot identity changed")
				}
				if _, err := s.Continue(ctx); !errors.Is(err, sessions.ErrEnvironmentRefresh) {
					t.Fatalf("cold environment was automatically trusted: %v", err)
				}
			}
			if effects != 1 {
				t.Fatal("cold replay repeated environment tool")
			}
		})
	}
}
