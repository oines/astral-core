package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
	sessions "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/store/memory"
)

type preparedModel struct {
	*testModel
	prepare func(context.Context, astral.Request) (astral.Request, error)
}

func (m preparedModel) PrepareRequest(ctx context.Context, request astral.Request) (astral.Request, error) {
	return m.prepare(ctx, request)
}

func TestActualModelAttemptsSurviveReopenWithFrozenMultimodalContext(t *testing.T) {
	for _, backend := range []string{"sqlite", "jsonl"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store, closeStore := durableStore(t, backend, dir)
			var environment atomic.Int32
			environment.Store(1)
			base := model(func(_ context.Context, n int, req astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
				if !strings.Contains(req.SystemPrompt, fmt.Sprintf("environment-%d", n)) {
					t.Fatalf("preparation was not frozen before estimation: %s", req.SystemPrompt)
				}
				for _, e := range mustReadHistory(t, store, "snapshot") {
					if e.ModelRequest != nil && reflect.DeepEqual(e.ModelRequest.Request, req) {
						if n == 1 {
							return answer("partial"), &astral.ModelError{Code: "temporary", Transient: true, Cause: errors.New("retry")}
						}
						return answer("complete"), nil
					}
				}
				t.Fatal("provider invoked before exact request was committed")
				return astral.Reply{}, errors.New("missing request")
			})
			base.estimator = func(astral.Request) (int, error) {
				environment.Add(1) // live host changes after preparation
				return 100, nil
			}
			m := preparedModel{testModel: base, prepare: func(_ context.Context, r astral.Request) (astral.Request, error) {
				r.SystemPrompt += fmt.Sprintf("\nenvironment-%d", environment.Load())
				return r, nil
			}}
			c := testConfig(m, store)
			c.Retry = astral.RetryPolicy{MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}
			manager := newAgent(t, c)
			s := newSession(t, manager, sessions.SessionOptions{ID: "snapshot"})
			input := astral.User("image URI: astral-file://computer/source")
			input.Content = append(input.Content, astral.Content{Type: "image", URL: "astral-file://computer/source", MediaType: "image/png", Data: json.RawMessage(`"cGl4ZWxz"`), Metadata: json.RawMessage(`{"source":"original"}`)})
			run, err := s.Start(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			waitOK(t, run)
			observed := base.observed()
			if len(observed) != 2 {
				t.Fatal("retry was not exercised")
			}
			if err = manager.Close(ctx); err != nil {
				t.Fatal(err)
			}
			closeStore()
			store, closeStore = durableStore(t, backend, dir)
			defer closeStore()
			c.Store = store
			manager = newAgent(t, c)
			s, err = manager.OpenSession(ctx, "snapshot")
			if err != nil {
				t.Fatal(err)
			}
			var requests []astral.Request
			var responses []astral.ModelResponseRecord
			ids := map[string]bool{}
			for _, e := range entries(s) {
				if e.ModelRequest != nil {
					requests = append(requests, e.ModelRequest.Request)
					ids[e.ModelRequest.AttemptID] = true
					if e.IsHistory() {
						t.Fatal("attempt snapshot entered conversation tree")
					}
				}
				if e.ModelResponse != nil {
					responses = append(responses, *e.ModelResponse)
					if !ids[e.ModelResponse.AttemptID] {
						t.Fatal("response lacks an earlier request")
					}
				}
			}
			if !reflect.DeepEqual(requests, observed) || len(responses) != 2 || responses[0].Error == "" ||
				responses[0].Reply.Message.Content[0].Text != "partial" || responses[1].Error != "" {
				t.Fatalf("attempt snapshots changed on reopen: requests=%d responses=%+v", len(requests), responses)
			}
			view, err := s.InspectContext(ctx)
			if err != nil || len(view.Request.Messages) != 2 {
				t.Fatalf("attempts polluted projected messages: %+v %v", view, err)
			}
		})
	}
}

func mustReadHistory(t *testing.T, store sessions.Store, id string) []sessions.Entry {
	t.Helper()
	history, err := readStore(context.Background(), store, id)
	if err != nil {
		t.Fatal(err)
	}
	return history.Entries
}

func TestModelSnapshotCommitFailurePreventsDependentEffects(t *testing.T) {
	for _, kind := range []sessions.EntryKind{sessions.EntryModelRequest, sessions.EntryModelResponse} {
		t.Run(string(kind), func(t *testing.T) {
			store := &faultStore{Store: memory.New(), kind: kind}
			var effects atomic.Int32
			m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
				return calls(call("effect", "echo", `{"value":"execute"}`)), nil
			})
			c := testConfig(m, store)
			c.Tools = []astral.Tool{echoTool(&effects)}
			c.Retry.MaxAttempts = 3
			s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
			if _, err := wait(t, start(t, s, "go")); err == nil {
				t.Fatal("storage failure was hidden")
			}
			want := 1
			if kind == sessions.EntryModelRequest {
				want = 0
			}
			if len(m.observed()) != want || effects.Load() != 0 {
				t.Fatal("a failed snapshot commit allowed dependent external effects or retries")
			}
		})
	}
}

func TestVersionSixHistoryRemainsReadableWhenAddingSnapshots(t *testing.T) {
	eachStore(t, func(t *testing.T, store sessions.Store) {
		header := storeHeader()
		header[0].Created.FormatVersion = 6
		header[1].Configuration.ModelID = "test"
		if err := appendStore(context.Background(), store, "session", 0, header); err != nil {
			t.Fatal(err)
		}
		m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
			return answer("v7"), nil
		})
		manager := newAgent(t, testConfig(m, store))
		s, err := manager.OpenSession(context.Background(), "session")
		if err != nil {
			t.Fatal(err)
		}
		waitOK(t, start(t, s, "continue v6"))
		if s.State().FormatVersion != sessions.FormatVersion || s.State().Creation.FormatVersion != 6 {
			t.Fatal("upgrading rewrote the original creation")
		}
	})
}
