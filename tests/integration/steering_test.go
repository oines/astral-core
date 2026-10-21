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

type inputEndpoint interface {
	Prompt(context.Context, astral.Input) (astral.PromptResult, error)
	Steer(context.Context, astral.Input) (astral.InputReceipt, error)
	FollowUp(context.Context, astral.Input) (astral.InputReceipt, error)
}

func steeringMessage(i int) astral.Message {
	return astral.Message{Role: astral.RoleUser, Metadata: json.RawMessage(fmt.Sprintf(`{"input":%d,"counter":900719925474099312345}`, i)), Content: []astral.Content{
		{Type: "text", Text: fmt.Sprintf("steer-%d", i)},
		{Type: "provider_extension", Data: json.RawMessage(`{"opaque":true}`), Metadata: json.RawMessage(`{"signature":"input-proof"}`)},
	}}
}

func userMessages(request astral.Request) []astral.Message {
	var out []astral.Message
	for _, m := range request.Messages {
		if m.Role == astral.RoleUser {
			out = append(out, m)
		}
	}
	return out
}

func TestMaxSteeringPerTurnConfiguration(t *testing.T) {
	for _, limit := range []int{0, 1, 2, 16, -1, 17, int(^uint(0) >> 1)} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
				return answer("unused"), nil
			})
			core, coreErr := newMemorySession(astral.Config{Model: m, MaxSteeringPerTurn: limit})
			c := testConfig(m, memory.New())
			c.MaxSteeringPerTurn = limit
			manager, sessionErr := astral.NewSession(context.Background(), c, astral.SessionOptions{})
			if core != nil {
				defer core.Close(context.Background())
			}
			if manager != nil {
				defer manager.Close(context.Background())
			}
			if limit < 0 || limit > 16 {
				if !errors.Is(coreErr, astral.ErrInvalid) || !errors.Is(sessionErr, astral.ErrInvalid) {
					t.Fatalf("invalid configuration accepted: core=%v session=%v", coreErr, sessionErr)
				}
			} else if coreErr != nil || sessionErr != nil {
				t.Fatalf("valid configuration rejected: core=%v session=%v", coreErr, sessionErr)
			}
		})
	}
}

func exerciseSteering(t *testing.T, store astral.Store, limit, count int) ([]astral.Request, []string) {
	t.Helper()
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	m := model(func(ctx context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
		if n == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return astral.Reply{}, ctx.Err()
			}
		}
		return answer("answer"), nil
	})
	var endpoint inputEndpoint
	if store == nil {
		a, err := newMemorySession(astral.Config{Model: m, MaxSteeringPerTurn: limit})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { a.Close(ctx) })
		endpoint = a
	} else {
		c := testConfig(m, store)
		c.MaxSteeringPerTurn = limit
		endpoint = newSession(t, newSessions(t, c), astral.SessionOptions{})
	}
	run, err := prompt(endpoint, ctx, astral.User("initial"))
	if err != nil {
		t.Fatal(err)
	}
	signal(t, entered)
	if _, err := followUp(endpoint, ctx, astral.User("follow-0")); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < count; i++ {
		message := steeringMessage(i)
		id, err := steer(endpoint, ctx, message)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		message.Content[0].Text = "mutated by host"
		message.Metadata[0] = '!'
		if i == 0 {
			if _, err := followUp(endpoint, ctx, astral.User("follow-1")); err != nil {
				t.Fatal(err)
			}
		}
	}
	close(release)
	waitOK(t, run)
	effective := max(1, limit)
	requests := m.observed()
	if want := 1 + (count+effective-1)/effective + 2; len(requests) != want {
		t.Fatalf("wrong number of requests: got %d want %d", len(requests), want)
	}
	want := []astral.Message{astral.User("initial")}
	n := 1
	for start := 0; start < count; start += effective {
		for i := start; i < min(start+effective, count); i++ {
			want = append(want, steeringMessage(i))
		}
		if !reflect.DeepEqual(userMessages(requests[n]), want) {
			t.Fatalf("steering boundary %d split, merged or reordered inputs", n)
		}
		n++
	}
	for i := 0; i < 2; i++ {
		want = append(want, astral.User(fmt.Sprintf("follow-%d", i)))
		if !reflect.DeepEqual(userMessages(requests[n+i]), want) {
			t.Fatal("follow-ups were batched or overtook steering")
		}
	}
	return requests, ids
}

func TestSteeringConfigurationPreservesDefaultsAndCoreSessionOrder(t *testing.T) {
	for _, test := range []struct{ limit, count int }{{0, 1}, {0, 3}, {1, 3}, {2, 5}, {16, 3}, {16, 19}} {
		t.Run(fmt.Sprintf("limit=%d/count=%d", test.limit, test.count), func(t *testing.T) {
			core, _ := exerciseSteering(t, nil, test.limit, test.count)
			eachStore(t, func(t *testing.T, underlying astral.Store) {
				capture := &capturedStore{Store: underlying}
				persistent, ids := exerciseSteering(t, capture, test.limit, test.count)
				if !reflect.DeepEqual(core, persistent) {
					t.Fatal("Core and Session model requests differ")
				}
				capture.mu.Lock()
				defer capture.mu.Unlock()
				var consumed []string
				for _, commit := range capture.commits {
					steers, followups := 0, 0
					for i, entry := range commit.Entries {
						if entry.Kind != astral.EntryInputConsumed {
							continue
						}
						if i == 0 || commit.Entries[i-1].Kind != astral.EntryMessage || commit.Entries[i-1].Message.Role != astral.RoleUser {
							t.Fatal("input consumption is not atomically paired with its message")
						}
						if entry.Input.Kind == astral.InputSteer {
							if commit.Entries[i-1].Message.Content[0].Text == "initial" {
								continue
							}
							steers++
							if !reflect.DeepEqual(*commit.Entries[i-1].Message, steeringMessage(len(consumed))) {
								t.Fatal("consumed input ID was paired with the wrong message")
							}
							consumed = append(consumed, entry.Input.ID)
						} else {
							followups++
						}
					}
					if steers > max(1, test.limit) || followups > 1 || (steers > 0 && followups > 0) {
						t.Fatal("a commit crossed the configured input boundary")
					}
				}
				if !reflect.DeepEqual(consumed, ids) {
					t.Fatal("steering IDs were lost, repeated or reordered")
				}
			})
		})
	}
}

type steeringFaultStore struct {
	astral.Store
	mode             string
	fired            atomic.Bool
	entered, release chan struct{}
}

func (s *steeringFaultStore) Commit(ctx context.Context, id string, c astral.Commit) (astral.Receipt, error) {
	selected := false
	for _, entry := range c.Entries {
		if entry.Kind == astral.EntryInputConsumed && !s.fired.Swap(true) {
			selected = true
		}
	}
	if selected {
		switch s.mode {
		case "failure":
			return astral.Receipt{}, errors.New("injected input commit failure")
		case "conflict":
			return astral.Receipt{}, astral.ErrConflict
		case "blocked", "cancel":
			close(s.entered)
			select {
			case <-s.release:
			case <-ctx.Done():
				return astral.Receipt{}, ctx.Err()
			}
		}
	}
	receipt, err := s.Store.Commit(ctx, id, c)
	if err == nil && selected && (s.mode == "lost-ack" || s.mode == "unconfirmed") {
		return astral.Receipt{}, errors.New("injected lost input commit receipt")
	}
	return receipt, err
}

func (s *steeringFaultStore) LookupCommit(ctx context.Context, id, key string) (astral.Receipt, error) {
	if s.mode == "unconfirmed" && s.fired.Load() {
		return astral.Receipt{}, errors.New("injected unavailable confirmation")
	}
	return s.Store.LookupCommit(ctx, id, key)
}

func TestSteeringAtomicCommitAndColdRecovery(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"failure", "conflict", "lost-ack", "unconfirmed", "cancel"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				dir := t.TempDir()
				var store astral.Store = memory.New()
				closeStore := func() {}
				if backend != "memory" {
					store, closeStore = durableStore(t, backend, dir)
				}
				defer func() { closeStore() }()
				fault := &steeringFaultStore{Store: store, mode: mode, entered: make(chan struct{}), release: make(chan struct{})}
				m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
					return answer("done"), nil
				})
				c := testConfig(m, fault)
				c.MaxSteeringPerTurn = 16
				a := newSessions(t, c)
				s := newSession(t, a, astral.SessionOptions{ID: "input-recovery"})
				seedCompletedBoundary(t, s, store)
				var ids []string
				for i := 0; i < 3; i++ {
					id, err := steer(s, ctx, steeringMessage(i))
					if err != nil {
						t.Fatal(err)
					}
					ids = append(ids, id)
				}
				run, err := s.Continue(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "cancel" {
					signal(t, fault.entered)
					run.Cancel()
				}
				_, err = wait(t, run)
				if (mode == "lost-ack") != (err == nil) {
					t.Fatalf("unexpected injected outcome: %v", err)
				}
				if mode == "conflict" && !errors.Is(err, astral.ErrConflict) {
					t.Fatalf("conflict was hidden: %v", err)
				}
				if mode == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel was hidden: %v", err)
				}
				if err := a.Close(ctx); err != nil {
					t.Fatal(err)
				}
				closeStore()
				if backend != "memory" {
					store, closeStore = durableStore(t, backend, dir)
				}
				c.Store = store
				a = newSessions(t, c)
				s, err = a.OpenSession(ctx, "input-recovery")
				if err != nil {
					t.Fatal(err)
				}
				written := mode == "lost-ack" || mode == "unconfirmed"
				wantPending := uint64(3)
				if written {
					wantPending = 0
				}
				if s.State().PendingInputs[astral.InputSteer] != wantPending || len(entries(s)) == 0 {
					t.Fatal("partial input batch recovered")
				}
				for _, e := range entries(s) {
					if !written && e.Message != nil && e.Message.Role == astral.RoleUser {
						t.Fatal("uncommitted message appeared in history")
					}
				}
				run, err = s.Continue(ctx)
				if written && len(m.observed()) == 1 {
					if !errors.Is(err, astral.ErrNotContinuable) {
						t.Fatal(err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					waitOK(t, run)
				}
				if len(m.observed()) != 1 || !reflect.DeepEqual(userMessages(m.observed()[0]), []astral.Message{steeringMessage(0), steeringMessage(1), steeringMessage(2)}) {
					t.Fatal("recovery lost, split or repeated an input batch")
				}
				snapshot, err := readStore(ctx, store, s.ID())
				if err != nil {
					t.Fatal(err)
				}
				var consumed []string
				var version uint64
				for i, e := range snapshot.Entries {
					if e.Kind != astral.EntryInputConsumed {
						continue
					}
					previous := snapshot.Entries[i-1]
					if previous.Kind != astral.EntryMessage || previous.Version != e.Version || previous.Sequence+1 != e.Sequence || (version != 0 && version != e.Version) || !reflect.DeepEqual(*previous.Message, steeringMessage(len(consumed))) {
						t.Fatal("message/consumption facts were not one atomic commit")
					}
					version = e.Version
					consumed = append(consumed, e.Input.ID)
				}
				if !reflect.DeepEqual(consumed, ids) || len(s.State().PendingInputs) != 0 || s.State().FormatVersion != astral.FormatVersion {
					t.Fatal("consumed IDs or V6 state changed during recovery")
				}
			})
		}
	}
}

func TestSteeringArrivingDuringCommitWaitsForNextBoundary(t *testing.T) {
	eachStore(t, func(t *testing.T, store astral.Store) {
		ctx := context.Background()
		fault := &steeringFaultStore{Store: store, mode: "blocked", entered: make(chan struct{}), release: make(chan struct{})}
		entered, release := make(chan struct{}), make(chan struct{})
		m := model(func(ctx context.Context, n int, request astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			if n == 1 {
				if !reflect.DeepEqual(userMessages(request), []astral.Message{steeringMessage(0), steeringMessage(1), steeringMessage(2)}) {
					t.Fatal("a later arrival changed the selected batch")
				}
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return astral.Reply{}, ctx.Err()
				}
			}
			return answer("answer"), nil
		})
		c := testConfig(m, fault)
		c.MaxSteeringPerTurn = 16
		s := newSession(t, newSessions(t, c), astral.SessionOptions{})
		seedCompletedBoundary(t, s, store)
		for i := 0; i < 3; i++ {
			if _, err := steer(s, ctx, steeringMessage(i)); err != nil {
				t.Fatal(err)
			}
		}
		run, err := s.Continue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		signal(t, fault.entered)
		queued := make(chan error, 1)
		go func() { _, err := steer(s, ctx, steeringMessage(3)); queued <- err }()
		close(fault.release)
		signal(t, entered)
		select {
		case err := <-queued:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("new input was blocked after the boundary commit")
		}
		close(release)
		waitOK(t, run)
		if len(m.observed()) != 2 || !reflect.DeepEqual(userMessages(m.observed()[1]), []astral.Message{steeringMessage(0), steeringMessage(1), steeringMessage(2), steeringMessage(3)}) {
			t.Fatal("new arrival was lost or reordered")
		}
	})
}

func TestSteeringByteBudgetCommitsNoPartialSelectedBatch(t *testing.T) {
	eachStore(t, func(t *testing.T, store astral.Store) {
		ctx := context.Background()
		entered, release := make(chan struct{}), make(chan struct{})
		m := model(func(ctx context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			if n == 1 {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return astral.Reply{}, ctx.Err()
				}
			}
			return answer("answer"), nil
		})
		c := testConfig(m, store)
		c.MaxContextBytes, c.MaxSteeringPerTurn = 4096, 16
		a := newSessions(t, c)
		s := newSession(t, a, astral.SessionOptions{ID: "byte-prefix"})
		initial := astral.User("")
		probe := astral.Request{ModelID: m.Spec().ID, MaxOutputTokens: 16384, Messages: []astral.Message{initial, answer("answer").Message, steeringMessage(0), steeringMessage(1)}}
		raw, _ := json.Marshal(probe)
		initial.Content[0].Text = strings.Repeat("h", 4096-len(raw)-32)
		run, err := prompt(s, ctx, initial)
		if err != nil {
			t.Fatal(err)
		}
		signal(t, entered)
		var ids []string
		for i := 0; i < 3; i++ {
			id, err := steer(s, ctx, steeringMessage(i))
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		close(release)
		if _, err := wait(t, run); !errors.Is(err, astral.ErrBudget) {
			t.Fatalf("oversized head did not stop the next boundary: %v", err)
		}
		if len(m.observed()) != 1 {
			t.Fatal("failed preparation partially consumed its selected batch")
		}
		pending, err := store.Pending(ctx, s.ID(), astral.PendingQuery{Kind: astral.EntryInputQueued, InputKind: astral.InputSteer, Limit: 16, MaxBytes: 4096})
		if err != nil || len(pending.Records) != 3 || pending.Records[0].Entry.Input.ID != ids[0] || pending.Records[2].Entry.Input.ID != ids[2] {
			t.Fatalf("oversized head was discarded: %+v %v", pending, err)
		}
		if err := a.Close(ctx); err != nil {
			t.Fatal(err)
		}
		c.MaxContextBytes = 8192
		s, err = newSessions(t, c).OpenSession(ctx, s.ID())
		if err != nil {
			t.Fatal(err)
		}
		run, err = s.Continue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		waitOK(t, run)
		if len(m.observed()) != 2 || !reflect.DeepEqual(userMessages(m.observed()[1]), []astral.Message{initial, steeringMessage(0), steeringMessage(1), steeringMessage(2)}) {
			t.Fatal("retained input did not resume in FIFO order")
		}
	})
}

func TestOversizedInputBatchRejectedBeforeAdmission(t *testing.T) {
	eachStore(t, func(t *testing.T, store astral.Store) {
		m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
			return answer("unused"), nil
		})
		c := testConfig(m, store)
		c.MaxContextBytes = 4096
		s := newSession(t, newSessions(t, c), astral.SessionOptions{})
		before := s.State()
		large := astral.User(strings.Repeat("x", 5000))
		if _, err := steer(s, context.Background(), large); !errors.Is(err, astral.ErrBudget) {
			t.Fatal(err)
		}
		if _, err := inject(s, context.Background(), []astral.Message{large}); !errors.Is(err, astral.ErrBudget) {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, s.State()) || len(m.observed()) != 0 {
			t.Fatal("oversized admission changed state")
		}
	})
}

func TestSteeringLaterArrivalsUseTheFollowingRequestInCoreAndSession(t *testing.T) {
	exercise := func(t *testing.T, store astral.Store) []astral.Request {
		ctx := context.Background()
		entered := []chan struct{}{make(chan struct{}), make(chan struct{})}
		release := []chan struct{}{make(chan struct{}), make(chan struct{})}
		m := model(func(ctx context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			if n <= 2 {
				close(entered[n-1])
				select {
				case <-release[n-1]:
				case <-ctx.Done():
					return astral.Reply{}, ctx.Err()
				}
			}
			return answer("answer"), nil
		})
		var endpoint inputEndpoint
		if store == nil {
			a, err := newMemorySession(astral.Config{Model: m, MaxSteeringPerTurn: 16})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { a.Close(ctx) })
			endpoint = a
		} else {
			c := testConfig(m, store)
			c.MaxSteeringPerTurn = 16
			endpoint = newSession(t, newSessions(t, c), astral.SessionOptions{})
		}
		run, err := prompt(endpoint, ctx, astral.User("initial"))
		if err != nil {
			t.Fatal(err)
		}
		signal(t, entered[0])
		for i := 0; i < 3; i++ {
			if _, err := steer(endpoint, ctx, steeringMessage(i)); err != nil {
				t.Fatal(err)
			}
		}
		close(release[0])
		signal(t, entered[1])
		if _, err := steer(endpoint, ctx, steeringMessage(3)); err != nil {
			t.Fatal(err)
		}
		close(release[1])
		waitOK(t, run)
		requests := m.observed()
		if len(requests) != 3 || len(userMessages(requests[1])) != 4 || !reflect.DeepEqual(userMessages(requests[2]), []astral.Message{astral.User("initial"), steeringMessage(0), steeringMessage(1), steeringMessage(2), steeringMessage(3)}) {
			t.Fatal("new arrival was folded into a prepared request or was lost")
		}
		return requests
	}
	core := exercise(t, nil)
	eachStore(t, func(t *testing.T, store astral.Store) {
		if !reflect.DeepEqual(core, exercise(t, store)) {
			t.Fatal("later-arrival request order differs between Core and Session")
		}
	})
}

func TestSteeringInputIDRemainsDistinctFromHistoryEntryIdentity(t *testing.T) {
	eachStore(t, func(t *testing.T, store astral.Store) {
		ctx := context.Background()
		m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
			return answer("done"), nil
		})
		s := newSession(t, newSessions(t, testConfig(m, store)), astral.SessionOptions{})
		seedCompletedBoundary(t, s, store)
		message := steeringMessage(0)
		// The input ID may equal its queued record's entry ID. A new
		// message must keep a fresh history identity when this input is consumed.
		queued := astral.Entry{ID: "existing-input", Kind: astral.EntryInputQueued, Input: &astral.InputRecord{ID: "existing-input", Kind: astral.InputSteer, Message: &message}}
		if err := appendStore(ctx, store, s.ID(), s.State().Version, []astral.Entry{queued}); err != nil {
			t.Fatal(err)
		}
		if err := s.Reload(ctx); err != nil {
			t.Fatal(err)
		}
		run, err := s.Continue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		waitOK(t, run)
		if len(m.observed()) != 1 || !reflect.DeepEqual(userMessages(m.observed()[0]), []astral.Message{message}) {
			t.Fatal("existing queued identity could not be consumed")
		}
		pending, err := store.Pending(ctx, s.ID(), astral.PendingQuery{Kind: astral.EntryInputQueued})
		if err != nil || len(pending.Records) != 0 {
			t.Fatal("existing input was not marked consumed")
		}
	})
}
