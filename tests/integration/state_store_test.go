package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	astral "github.com/oines/astral-core"

	"github.com/oines/astral-core/store/memory"
	"github.com/oines/astral-core/store/sqlite"
)

func eachStore(t *testing.T, fn func(*testing.T, astral.Store)) {
	t.Helper()
	for _, name := range []string{"memory", "sqlite"} {
		t.Run(name, func(t *testing.T) {
			if name == "memory" {
				fn(t, memory.New())
				return
			}
			st, closeStore := durableStore(t, name, t.TempDir())
			defer closeStore()
			fn(t, st)
		})
	}
}

func TestCommitReceiptsAreIdempotentAndContentBound(t *testing.T) {
	eachStore(t, func(t *testing.T, st astral.Store) {
		ctx := context.Background()
		c := makeCommit(astral.RuntimeState{}, 0, storeHeader())
		first, err := st.Commit(ctx, "session", c)
		if err != nil {
			t.Fatal(err)
		}
		again, err := st.Commit(ctx, "session", c)
		if err != nil || first != again {
			t.Fatalf("retry: %+v %v", again, err)
		}
		changed := c
		changed.Entries = append([]astral.Entry(nil), c.Entries...)
		changed.Entries[1].ID = "changed"
		if _, err = st.Commit(ctx, "session", changed); !errors.Is(err, astral.ErrConflict) {
			t.Fatalf("changed commit accepted: %v", err)
		}
		if err = appendStore(ctx, st, "session", 1, []astral.Entry{storeMessage("next", "config")}); err != nil {
			t.Fatal(err)
		}
		again, err = st.LookupCommit(ctx, "session", c.ID)
		if err != nil || again.Version != 1 || again.CurrentVersion != 2 || again.Digest != first.Digest {
			t.Fatalf("receipt lost ownership boundary: %+v %v", again, err)
		}
	})
}

func TestMalformedStoreEnvelopeCannotPanicOrPartiallyCommit(t *testing.T) {
	eachStore(t, func(t *testing.T, st astral.Store) {
		ctx := context.Background()
		if err := appendStore(ctx, st, "session", 0, storeHeader()); err != nil {
			t.Fatal(err)
		}
		before, err := st.LoadState(ctx, "session")
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range []astral.EntryKind{astral.EntryInputConsumed, astral.EntryToolStarted, astral.EntryApprovalDecided, astral.EntryCompaction} {
			c := makeCommit(before, before.Version, []astral.Entry{{ID: string(kind), Kind: kind}})
			if _, err = st.Commit(ctx, "session", c); !errors.Is(err, astral.ErrCorrupt) {
				t.Fatalf("malformed %s: %v", kind, err)
			}
		}
		after, err := st.LoadState(ctx, "session")
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("invalid envelopes advanced state")
		}
	})
}

func TestStorePaginationAndCancelledFork(t *testing.T) {
	eachStore(t, func(t *testing.T, st astral.Store) {
		ctx := context.Background()
		if err := appendStore(ctx, st, "session", 0, storeHeader()); err != nil {
			t.Fatal(err)
		}
		parent := "config"
		var batch []astral.Entry
		for i := 0; i < 31; i++ {
			id := fmt.Sprintf("entry-%02d", i)
			batch = append(batch, storeMessage(id, parent))
			parent = id
		}
		if err := appendStore(ctx, st, "session", 1, batch); err != nil {
			t.Fatal(err)
		}
		var ids []string
		cursor := ""
		for {
			page, err := st.Scan(ctx, "session", astral.Query{PathHead: parent, Cursor: cursor, Limit: 7})
			if err != nil || len(page.Records) > 7 {
				t.Fatalf("page: %+v %v", page, err)
			}
			for _, r := range page.Records {
				ids = append(ids, r.Entry.ID)
			}
			if page.Next == "" {
				break
			}
			cursor = page.Next
		}
		if len(ids) != 32 || ids[0] != parent || ids[31] != "config" {
			t.Fatalf("path pagination: %v", ids)
		}
		page, err := st.Scan(ctx, "session", astral.Query{Kind: astral.EntryMessage, Reverse: true, Limit: 2})
		if err != nil || len(page.Records) != 2 || page.Records[0].Entry.ID != parent || page.Next == "" {
			t.Fatalf("indexed kind query: %+v %v", page, err)
		}
		root := ""
		page, err = st.Scan(ctx, "session", astral.Query{ParentID: &root, Kind: astral.EntryMessage, HistoryOnly: true})
		if err != nil || len(page.Records) != 0 {
			t.Fatalf("combined filters ignored: %+v %v", page, err)
		}
		if _, err = st.Scan(ctx, "session", astral.Query{MaxBytes: 1}); !errors.Is(err, astral.ErrBudget) {
			t.Fatalf("page bytes not bounded: %v", err)
		}
		if _, err = st.Scan(ctx, "session", astral.Query{PathHead: "created"}); !errors.Is(err, astral.ErrNotFound) {
			t.Fatalf("operational entry used as history head: %v", err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = st.Fork(cancelled, astral.ForkRequest{SourceID: "session", SourceRolloutID: "session", TargetID: "child", HeadID: parent, Mode: astral.ForkDetached})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled fork: %v", err)
		}
		if _, err = st.LoadState(ctx, "child"); !errors.Is(err, astral.ErrNotFound) {
			t.Fatalf("partial child published: %v", err)
		}
	})
}

type capturedStore struct {
	astral.Store
	mu      sync.Mutex
	commits []astral.Commit
	states  []astral.RuntimeState
}

func (s *capturedStore) Commit(ctx context.Context, id string, c astral.Commit) (astral.Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.Store.Commit(ctx, id, c)
	if err != nil {
		return r, err
	}
	state, err := s.LoadState(ctx, id)
	if err != nil {
		return r, err
	}
	confirmed, err := c.Confirmed(r)
	if err != nil {
		return r, err
	}
	raw, _ := json.Marshal(confirmed)
	var owned astral.Commit
	_ = json.Unmarshal(raw, &owned)
	s.commits = append(s.commits, owned)
	s.states = append(s.states, state)
	return r, nil
}

func TestIncrementalFullReplayAndCheckpointAgree(t *testing.T) {
	eachStore(t, func(t *testing.T, underlying astral.Store) {
		st := &capturedStore{Store: underlying}
		m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			if n == 1 {
				return calls(call("c", "echo", `{"value":"result"}`)), nil
			}
			return answer("answer or summary"), nil
		})
		c := testConfig(m, st)
		c.Tools = []astral.Tool{echoTool(nil)}
		c.Compaction.KeepRecentTokens = 1
		c.BeforeToolCall = func(ctx context.Context, inv astral.ToolContext) (astral.ToolDecision, error) {
			d, err := inv.AwaitApproval(ctx, astral.ApprovalRequest{Prompt: "allow"})
			return astral.ToolDecision{Block: !d.Approved}, err
		}
		s := newSession(t, newSessions(t, c), astral.SessionOptions{ID: "session"})
		sub := s.Subscribe(128)
		defer sub.Close()
		r := start(t, s, "one")
		approval := waitApproval(t, sub)
		if _, err := followUp(s, context.Background(), astral.User("queued")); err != nil {
			t.Fatal(err)
		}
		if err := s.DecideApproval(context.Background(), approval, astral.ApprovalDecision{Approved: true}); err != nil {
			t.Fatal(err)
		}
		waitOK(t, r)
		waitOK(t, start(t, s, "two"))
		if _, err := s.Compact(context.Background(), "retain decisions"); err != nil {
			t.Fatal(err)
		}
		waitOK(t, start(t, s, "branch"))
		replayed := astral.RuntimeState{}
		for i, commit := range st.commits {
			var err error
			replayed, _, err = astral.Reduce(replayed, commit)
			if err != nil || !reflect.DeepEqual(replayed, st.states[i]) {
				t.Fatalf("replay at commit %d: %v", i, err)
			}
		}
		for checkpoint := range st.states {
			raw, _ := json.Marshal(st.states[checkpoint])
			var restored astral.RuntimeState
			_ = json.Unmarshal(raw, &restored)
			for _, commit := range st.commits[checkpoint+1:] {
				var err error
				restored, _, err = astral.Reduce(restored, commit)
				if err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(restored, replayed) {
				t.Fatalf("checkpoint %d diverged", checkpoint)
			}
		}
		if !reflect.DeepEqual(s.State(), replayed) {
			t.Fatal("live state differs from committed state")
		}
	})
}

func TestCurrentFormatMarkersAgreeWithRuntimeState(t *testing.T) {
	if astral.FormatVersion != 11 {
		t.Fatalf("current format must be version 11, got %d", astral.FormatVersion)
	}
	for _, backend := range []string{"sqlite"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			store, closeStore := durableStore(t, backend, dir)
			m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
				return answer("done"), nil
			})
			a := newSessions(t, testConfig(m, store))
			s := newSession(t, a, astral.SessionOptions{})
			if s.State().FormatVersion != astral.FormatVersion || s.State().Creation.FormatVersion != astral.FormatVersion {
				t.Fatal("runtime state and creation did not adopt the current format")
			}
			if err := a.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			closeStore()
			db, err := sql.Open("sqlite", filepath.Join(dir, "history.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var format, userVersion int
			if err := db.QueryRow("SELECT version FROM format").Scan(&format); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil || format != astral.FormatVersion || int64(userVersion) != sqlite.SchemaVersion {
				t.Fatalf("SQLite markers: format=%d user_version=%d %v", format, userVersion, err)
			}
		})
	}
}

func TestProtocolPayloadsSurviveColdReopenAndFork(t *testing.T) {
	for _, backend := range []string{"sqlite"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			st, closeStore := durableStore(t, backend, dir)
			opaque := json.RawMessage(`{"signature":"opaque","counter":900719925474099312345,"encrypted":"AA=="}`)
			input := astral.Message{Role: astral.RoleUser, Metadata: opaque, Content: []astral.Content{{Type: "text", Text: "first"}, {Type: "image", MediaType: "image/png", Data: json.RawMessage(`{"base64":"AA=="}`), Metadata: opaque}, {Type: "audio", URL: "https://example.invalid/audio", Metadata: opaque}, {Type: "text", Text: "last"}}}
			assistant := calls(call("provider-call-id", "echo", `{"value":"output"}`))
			assistant.Message.Content[0].ToolCall.Metadata = opaque
			assistant.Message.Content = append([]astral.Content{{Type: "reasoning", Data: json.RawMessage(`{"encrypted_content":"AAA"}`), Metadata: opaque}, {Type: "text", Text: "work"}}, assistant.Message.Content...)
			assistant.Message.Metadata = opaque
			m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
				if n == 1 {
					return assistant, nil
				}
				return answer("done"), nil
			})
			c := testConfig(m, st)
			c.Tools = []astral.Tool{echoTool(nil)}
			a := newSessions(t, c)
			s := newSession(t, a, astral.SessionOptions{ID: "protocol"})
			r, err := prompt(s, context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			waitOK(t, r)
			want, err := s.InspectContext(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(want.Request.Messages[0], input) || !reflect.DeepEqual(want.Request.Messages[1], assistant.Message) || want.Request.Messages[2].ToolCallID != "provider-call-id" {
				t.Fatal("protocol payload changed before persistence")
			}
			if err = a.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			closeStore()
			st, closeStore = durableStore(t, backend, dir)
			defer closeStore()
			c.Store = st
			a = newSessions(t, c)
			s, err = a.OpenSession(context.Background(), "protocol")
			if err != nil {
				t.Fatal(err)
			}
			candidates := []*astral.Session{s}
			for _, mode := range []astral.ForkMode{astral.ForkShared, astral.ForkDetached} {
				child, err := s.Fork(context.Background(), s.Head(), astral.ForkOptions{ID: fmt.Sprintf("child-%d", mode), Mode: mode})
				if err != nil {
					t.Fatal(err)
				}
				candidates = append(candidates, child)
			}
			for _, candidate := range candidates {
				got, err := candidate.InspectContext(context.Background())
				if err != nil || !reflect.DeepEqual(got.Request, want.Request) {
					t.Fatalf("payload lost after reopen/fork: %v", err)
				}
			}
			if err = a.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			closeStore()
			st, closeStore = durableStore(t, backend, dir)
			defer closeStore()
			c.Store = st
			a = newSessions(t, c)
			for _, id := range []string{"child-0", "child-1"} {
				child, err := a.OpenSession(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				got, err := child.InspectContext(context.Background())
				if err != nil || !reflect.DeepEqual(got.Request, want.Request) {
					t.Fatalf("cold child payload changed: %v", err)
				}
			}

		})
	}
}
