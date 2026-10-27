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
	for _, backend := range []string{"sqlite"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store, closeStore := durableStore(t, backend, dir)
			var environment atomic.Int32
			environment.Store(1)
			base := model(func(_ context.Context, n int, req astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
				if !strings.Contains(req.SystemPrompt, "environment-") {
					t.Fatalf("preparation was not frozen before estimation: %s", req.SystemPrompt)
				}
				state, err := store.LoadState(ctx, "snapshot")
				if err != nil || state.CurrentAttemptID == "" {
					t.Fatalf("provider invoked without durable attempt: %v", err)
				}
				attempt, err := store.GetAttempt(ctx, "snapshot", state.CurrentAttemptID)
				if err != nil {
					t.Fatal(err)
				}
				frozen, err := astral.RebuildRequest(ctx, store, attempt.Input, astral.DefaultContextBytes)
				if err != nil || !reflect.DeepEqual(frozen, req) {
					t.Fatalf("provider input was not atomically frozen: %v", err)
				}
				if n == 1 {
					return answer("partial"), &astral.ModelError{Code: "temporary", Transient: true, Cause: errors.New("retry")}
				}
				return answer("complete"), nil
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
			manager := newSessions(t, c)
			s := newSession(t, manager, astral.SessionOptions{ID: "snapshot"})
			input := astral.User("image URI: astral-file://computer/source")
			input.Content = append(input.Content, astral.Content{Type: "image", URL: "astral-file://computer/source", MediaType: "image/png", Data: json.RawMessage(`"cGl4ZWxz"`), Metadata: json.RawMessage(`{"source":"original"}`)})
			run, err := prompt(s, ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			waitOK(t, run)
			observed := base.observed()
			if len(observed) != 2 || !reflect.DeepEqual(observed[0], observed[1]) {
				t.Fatal("retry was not exercised")
			}
			if err = manager.Close(ctx); err != nil {
				t.Fatal(err)
			}
			closeStore()
			store, closeStore = durableStore(t, backend, dir)
			defer closeStore()
			c.Store = store
			manager = newSessions(t, c)
			s, err = manager.OpenSession(ctx, "snapshot")
			if err != nil {
				t.Fatal(err)
			}
			var requests []astral.Request
			attempts := allAttempts(t, s)
			for _, attempt := range attempts {
				request, err := s.Attempts().Request(ctx, attempt.ID)
				if err != nil {
					t.Fatal(err)
				}
				requests = append(requests, request)
				raw, _ := json.Marshal(attempt)
				if strings.Contains(string(raw), "cGl4ZWxz") {
					t.Fatal("image body copied into attempt")
				}
			}
			if !reflect.DeepEqual(requests, observed) || len(attempts) != 2 || attempts[0].Outcome == nil || attempts[0].Outcome.Error == "" ||
				attempts[0].Outcome.Reply.Message.Content[0].Text != "partial" || attempts[1].Outcome == nil || attempts[1].Outcome.Error != "" || attempts[1].Outcome.EntryID == "" || attempts[1].Outcome.Reply != nil {
				t.Fatalf("attempt inputs/results changed on reopen: requests=%d attempts=%+v", len(requests), attempts)
			}
			accepted, err := s.History().Get(ctx, attempts[1].Outcome.EntryID)
			if err != nil || accepted.Message.Content[0].Text != "complete" || accepted.Generation.AttemptID != attempts[1].ID {
				t.Fatalf("missing canonical reply: %v", err)
			}
			view, err := s.InspectContext(ctx)
			if err != nil || len(view.Request.Messages) != 2 {
				t.Fatalf("attempts polluted projected messages: %+v %v", view, err)
			}
		})
	}
}

func mustReadHistory(t *testing.T, store astral.Store, id string) []astral.Entry {
	t.Helper()
	history, err := readStore(context.Background(), store, id)
	if err != nil {
		t.Fatal(err)
	}
	return history.Entries
}

func TestModelSnapshotCommitFailurePreventsDependentEffects(t *testing.T) {
	for _, phase := range []string{"create", "finish"} {
		t.Run(phase, func(t *testing.T) {
			store := &faultStore{Store: memory.New(), attemptPhase: phase}
			var effects atomic.Int32
			m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
				return calls(call("effect", "echo", `{"value":"execute"}`)), nil
			})
			c := testConfig(m, store)
			c.Tools = []astral.Tool{echoTool(&effects)}
			c.Retry.MaxAttempts = 3
			s := newSession(t, newSessions(t, c), astral.SessionOptions{})
			if _, err := wait(t, start(t, s, "go")); err == nil {
				t.Fatal("storage failure was hidden")
			}
			want := 1
			if phase == "create" {
				want = 0
			}
			if len(m.observed()) != want || effects.Load() != 0 {
				t.Fatal("a failed snapshot commit allowed dependent external effects or retries")
			}
		})
	}
}

func TestPreviousHistoryFormatsAreRejected(t *testing.T) {
	for _, version := range []int{6, 7, 8, 9, 10} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			eachStore(t, func(t *testing.T, store astral.Store) {
				header := storeHeader()
				header[0].Created.FormatVersion = version
				if err := appendStore(context.Background(), store, "session", 0, header); err == nil {
					t.Fatal("old storage format was accepted")
				}
			})
		})
	}
}
