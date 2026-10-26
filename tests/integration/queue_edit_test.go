package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/memory"
	"github.com/oines/astral-core/store/sqlite"
)

func TestQueueEditsSurviveColdOpenAndConsumeOnce(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "queue.db")
			var store core.Store = memory.New()
			var disk *sqlite.Store
			if backend == "sqlite" {
				var err error
				disk, err = sqlite.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				store = disk
				defer func() { disk.Close() }()
			}
			m := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
				return answer("done"), nil
			})
			config := testConfig(m, store)
			s, err := core.NewSession(ctx, config, core.SessionOptions{})
			if err != nil {
				t.Fatal(err)
			}
			original := map[string]core.InputReceipt{}
			for _, id := range []string{"a", "b", "c", "d"} {
				message := core.User(id)
				message.Content = append(message.Content, core.Content{Type: "image", MediaType: "image/png", URL: "data:image/png;base64,Zm9v"})
				message.Metadata = json.RawMessage(`{"counter":9007199254740993,"source":"web"}`)
				receipt, err := s.FollowUp(ctx, core.Input{ID: id, Message: message, Contexts: []core.Message{snapshotMessage("frozen " + id)}})
				if err != nil {
					t.Fatal(err)
				}
				original[id] = receipt
			}
			if err = s.MoveQueuedInput(ctx, "c", "a"); err != nil {
				t.Fatal(err)
			}
			edited := *original["b"].Message
			edited.Content = append([]core.Content(nil), edited.Content...)
			edited.Content[0].Text = "edited b"
			if err = s.UpdateQueuedInput(ctx, "b", core.InputSteer, &edited); err != nil {
				t.Fatal(err)
			}
			if err = s.ClearQueuedInput(ctx, "d"); err != nil {
				t.Fatal(err)
			}
			queued, err := s.PeekQueuedInputs(ctx)
			if err != nil || len(queued) != 3 || queued[0].ID != "c" || queued[1].ID != "a" || queued[2].ID != "b" {
				t.Fatalf("queue: %+v %v", queued, err)
			}
			if err = s.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if disk != nil {
				if err = disk.Close(); err != nil {
					t.Fatal(err)
				}
				disk, err = sqlite.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				config.Store = disk
			}
			s, err = core.OpenSession(ctx, config, s.ID())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(ctx)
			restored, err := s.PeekQueuedInputs(ctx)
			if err != nil || !reflect.DeepEqual(queued, restored) {
				t.Fatalf("cold queue changed: %+v %v", restored, err)
			}
			for _, item := range restored {
				receipt, err := s.LookupInput(ctx, item.ID)
				old := original[item.ID]
				if err != nil || receipt.Digest != old.Digest || !receipt.AcceptedAt.Equal(old.AcceptedAt) || !reflect.DeepEqual(receipt.Contexts, old.Contexts) || !reflect.DeepEqual(receipt.Message.Content[1:], old.Message.Content[1:]) {
					t.Fatalf("lost frozen input: %+v %v", receipt, err)
				}
			}
			run, err := s.Continue(ctx)
			if err != nil {
				t.Fatal(err)
			}
			waitOK(t, run)
			requests := m.observed()
			if len(requests) != 3 {
				t.Fatalf("consumed %d times, want 3", len(requests))
			}
			for i, id := range []string{"b", "c", "a"} {
				messages := userMessages(requests[i])
				wanted := id
				if id == "b" {
					wanted = "edited b"
				}
				if messages[len(messages)-1].Content[0].Text != wanted {
					t.Fatalf("wrong application order: %+v", messages)
				}
				receipt, err := s.LookupInput(ctx, id)
				if err != nil || receipt.State != core.InputApplied || receipt.EntryID == "" {
					t.Fatalf("missing application receipt: %+v %v", receipt, err)
				}
			}
			for _, change := range []func() error{
				func() error { return s.MoveQueuedInput(ctx, "b", "") },
				func() error { return s.UpdateQueuedInput(ctx, "b", core.InputFollowUp, nil) },
				func() error { return s.ClearQueuedInput(ctx, "b") },
			} {
				if err = change(); !errors.Is(err, core.ErrNotFound) {
					t.Fatalf("late UI action revived consumed input: %v", err)
				}
			}
		})
	}
}

func TestQueueReorderCommitFailureLeavesWholeQueue(t *testing.T) {
	for _, lostAck := range []bool{false, true} {
		eachStore(t, func(t *testing.T, underlying core.Store) {
			ctx := context.Background()
			store := &faultStore{Store: underlying, kind: core.EntryInputRequeued, after: lostAck}
			m := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
				return answer("done"), nil
			})
			s := newSession(t, newSessions(t, testConfig(m, store)), core.SessionOptions{})
			for _, id := range []string{"a", "b"} {
				if _, err := s.FollowUp(ctx, core.Input{ID: id, Message: core.User(id)}); err != nil {
					t.Fatal(err)
				}
			}
			err := s.MoveQueuedInput(ctx, "b", "a")
			if (err == nil) != lostAck {
				t.Fatalf("unexpected commit result: %v", err)
			}
			queue, err := s.PeekQueuedInputs(ctx)
			first := "a"
			if lostAck {
				first = "b"
			}
			if err != nil || len(queue) != 2 || queue[0].ID != first || s.State().PendingInputs[core.InputFollowUp] != 2 {
				t.Fatalf("partial queue commit: %+v %v", queue, err)
			}
		})
	}
}

func TestQueueRevisionDuringPreparationRebuildsBeforeConsumption(t *testing.T) {
	eachStore(t, func(t *testing.T, store core.Store) {
		ctx := context.Background()
		entered, release := make(chan struct{}), make(chan struct{})
		var blocked atomic.Bool
		m := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
			return answer("done"), nil
		})
		config := testConfig(m, store)
		config.CollectContext = func(ctx context.Context, boundary core.ContextBoundary) ([]core.Message, error) {
			if boundary.Reason == "boundary" && !blocked.Swap(true) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return nil, nil
		}
		s := newSession(t, newSessions(t, config), core.SessionOptions{})
		for _, id := range []string{"a", "b"} {
			if _, err := s.FollowUp(ctx, core.Input{ID: id, Message: core.User(id)}); err != nil {
				t.Fatal(err)
			}
		}
		run, err := s.Continue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		signal(t, entered)
		edited := core.User("edited a")
		if err = s.UpdateQueuedInput(ctx, "a", "", &edited); err != nil {
			t.Fatal(err)
		}
		if err = s.MoveQueuedInput(ctx, "b", "a"); err != nil {
			t.Fatal(err)
		}
		close(release)
		waitOK(t, run)
		requests := m.observed()
		if len(requests) != 2 {
			t.Fatalf("obsolete preparation reached model: %d", len(requests))
		}
		for i, wanted := range []string{"b", "edited a"} {
			messages := userMessages(requests[i])
			if messages[len(messages)-1].Content[0].Text != wanted {
				t.Fatalf("old queue consumed: %+v", messages)
			}
		}
		if s.Snapshot().CanContinue {
			t.Fatal("completed empty queue is continuable")
		}
	})
}
