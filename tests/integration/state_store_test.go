package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	astral "github.com/oines/astral-core"
	sessions "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/store/jsonl"
	"github.com/oines/astral-core/store/memory"
	"github.com/oines/astral-core/store/sqlite"
)

func eachStore(t *testing.T, fn func(*testing.T, sessions.Store)) {
	t.Helper()
	for _, name := range []string{"memory", "jsonl", "sqlite"} {
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
	eachStore(t, func(t *testing.T, st sessions.Store) {
		ctx := context.Background()
		c := makeCommit(sessions.RuntimeState{}, 0, storeHeader())
		first, err := st.Commit(ctx, "session", c)
		if err != nil {
			t.Fatal(err)
		}
		again, err := st.Commit(ctx, "session", c)
		if err != nil || first != again {
			t.Fatalf("retry: %+v %v", again, err)
		}
		changed := c
		changed.Entries = append([]sessions.Entry(nil), c.Entries...)
		changed.Entries[1].ID = "changed"
		if _, err = st.Commit(ctx, "session", changed); !errors.Is(err, sessions.ErrConflict) {
			t.Fatalf("changed commit accepted: %v", err)
		}
		if err = appendStore(ctx, st, "session", 1, []sessions.Entry{storeMessage("next", "config")}); err != nil {
			t.Fatal(err)
		}
		again, err = st.LookupCommit(ctx, "session", c.ID)
		if err != nil || again.Version != 1 || again.CurrentVersion != 2 || again.Digest != first.Digest {
			t.Fatalf("receipt lost ownership boundary: %+v %v", again, err)
		}
	})
}

func TestMalformedStoreEnvelopeCannotPanicOrPartiallyCommit(t *testing.T) {
	eachStore(t, func(t *testing.T, st sessions.Store) {
		ctx := context.Background()
		if err := appendStore(ctx, st, "session", 0, storeHeader()); err != nil {
			t.Fatal(err)
		}
		before, err := st.LoadState(ctx, "session")
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range []sessions.EntryKind{sessions.EntryInputConsumed, sessions.EntryToolStarted, sessions.EntryApprovalDecided, sessions.EntryCompaction} {
			c := makeCommit(before, before.Version, []sessions.Entry{{ID: string(kind), Kind: kind}})
			if _, err = st.Commit(ctx, "session", c); !errors.Is(err, sessions.ErrCorrupt) {
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
	eachStore(t, func(t *testing.T, st sessions.Store) {
		ctx := context.Background()
		if err := appendStore(ctx, st, "session", 0, storeHeader()); err != nil {
			t.Fatal(err)
		}
		parent := "config"
		var batch []sessions.Entry
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
			page, err := st.Scan(ctx, "session", sessions.Query{PathHead: parent, Cursor: cursor, Limit: 7})
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
		page, err := st.Scan(ctx, "session", sessions.Query{Kind: sessions.EntryMessage, Reverse: true, Limit: 2})
		if err != nil || len(page.Records) != 2 || page.Records[0].Entry.ID != parent || page.Next == "" {
			t.Fatalf("indexed kind query: %+v %v", page, err)
		}
		root := ""
		page, err = st.Scan(ctx, "session", sessions.Query{ParentID: &root, Kind: sessions.EntryMessage, HistoryOnly: true})
		if err != nil || len(page.Records) != 0 {
			t.Fatalf("combined filters ignored: %+v %v", page, err)
		}
		if _, err = st.Scan(ctx, "session", sessions.Query{MaxBytes: 1}); !errors.Is(err, sessions.ErrBudget) {
			t.Fatalf("page bytes not bounded: %v", err)
		}
		if _, err = st.Scan(ctx, "session", sessions.Query{PathHead: "created"}); !errors.Is(err, sessions.ErrNotFound) {
			t.Fatalf("operational entry used as history head: %v", err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = st.Fork(cancelled, sessions.ForkRequest{SourceID: "session", SourceRolloutID: "session", TargetID: "child", HeadID: parent, Mode: sessions.ForkDetached})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled fork: %v", err)
		}
		if _, err = st.LoadState(ctx, "child"); !errors.Is(err, sessions.ErrNotFound) {
			t.Fatalf("partial child published: %v", err)
		}
	})
}

type capturedStore struct {
	sessions.Store
	mu      sync.Mutex
	commits []sessions.Commit
	states  []sessions.RuntimeState
}

func (s *capturedStore) Commit(ctx context.Context, id string, c sessions.Commit) (sessions.Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.Store.Commit(ctx, id, c)
	if err != nil {
		return r, err
	}
	state, err := s.Store.LoadState(ctx, id)
	if err != nil {
		return r, err
	}
	confirmed, err := c.Confirmed(r)
	if err != nil {
		return r, err
	}
	raw, _ := json.Marshal(confirmed)
	var owned sessions.Commit
	_ = json.Unmarshal(raw, &owned)
	s.commits = append(s.commits, owned)
	s.states = append(s.states, state)
	return r, nil
}

func TestIncrementalFullReplayAndCheckpointAgree(t *testing.T) {
	eachStore(t, func(t *testing.T, underlying sessions.Store) {
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
		c.BeforeToolCall = func(ctx context.Context, inv sessions.ToolContext) (astral.ToolDecision, error) {
			d, err := inv.AwaitApproval(ctx, sessions.ApprovalRequest{Prompt: "allow"})
			return astral.ToolDecision{Block: !d.Approved}, err
		}
		s := newSession(t, newAgent(t, c), sessions.SessionOptions{ID: "session"})
		sub := s.Subscribe(128)
		defer sub.Close()
		r := start(t, s, "one")
		approval := waitApproval(t, sub)
		if _, err := s.FollowUp(context.Background(), astral.User("queued")); err != nil {
			t.Fatal(err)
		}
		if err := s.DecideApproval(context.Background(), approval, sessions.ApprovalDecision{Approved: true}); err != nil {
			t.Fatal(err)
		}
		waitOK(t, r)
		waitOK(t, start(t, s, "two"))
		if _, err := s.Compact(context.Background(), "retain decisions"); err != nil {
			t.Fatal(err)
		}
		waitOK(t, start(t, s, "branch"))
		replayed := sessions.RuntimeState{}
		for i, commit := range st.commits {
			var err error
			replayed, _, err = sessions.Reduce(replayed, commit)
			if err != nil || !reflect.DeepEqual(replayed, st.states[i]) {
				t.Fatalf("replay at commit %d: %v", i, err)
			}
		}
		for checkpoint := range st.states {
			raw, _ := json.Marshal(st.states[checkpoint])
			var restored sessions.RuntimeState
			_ = json.Unmarshal(raw, &restored)
			for _, commit := range st.commits[checkpoint+1:] {
				var err error
				restored, _, err = sessions.Reduce(restored, commit)
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

func TestOldFormatsAreRejectedWithoutMutation(t *testing.T) {
	t.Run("jsonl", func(t *testing.T) {
		dir := t.TempDir()
		sum := sha256.Sum256([]byte("session"))
		path := filepath.Join(dir, hex.EncodeToString(sum[:])+".jsonl")
		old := []byte("{\"version\":1,\"entries\":[{\"kind\":\"session_created\"}]}\n")
		if err := os.WriteFile(path, old, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := jsonl.Open(dir); !errors.Is(err, sessions.ErrUnsupportedFormat) {
			t.Fatalf("old format: %v", err)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(old, after) {
			t.Fatal("old file changed")
		}
	})
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "old.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec("CREATE TABLE format(version INTEGER); INSERT INTO format VALUES(1)"); err != nil {
			t.Fatal(err)
		}
		db.Close()
		before, _ := os.ReadFile(path)
		if _, err = sqlite.Open(path); !errors.Is(err, sessions.ErrUnsupportedFormat) {
			t.Fatalf("old database: %v", err)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("old database changed")
		}
	})
}

func TestVersionThreeOrderedProtocolUpgradeDoesNotMigrateData(t *testing.T) {
	for _, backend := range []string{"jsonl", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			legacy := []byte(`{"role":"assistant","content":[{"type":"text","text":"old"}],"tool_calls":[{"id":"old-call","name":"work","arguments":{}}]}`)
			path := filepath.Join(dir, "old.db")
			if backend == "jsonl" {
				for name, data := range map[string][]byte{
					"format.json":         []byte(`{"format_version":3,"rollout_id":""}`),
					"old.jsonl":           legacy,
					"unpublished.pending": []byte("unpublished V3 data"),
				} {
					if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec("CREATE TABLE format(version INTEGER); INSERT INTO format VALUES(3); CREATE TABLE entries(payload BLOB); PRAGMA user_version=3;")
				if err == nil {
					_, err = db.Exec("INSERT INTO entries VALUES(?)", legacy)
				}
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			snapshot := func() map[string]string {
				out := map[string]string{}
				files, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				for _, file := range files {
					data, err := os.ReadFile(filepath.Join(dir, file.Name()))
					if err != nil {
						t.Fatal(err)
					}
					out[file.Name()] = string(data)
				}
				return out
			}
			before := snapshot()
			var err error
			if backend == "jsonl" {
				_, err = jsonl.Open(dir)
			} else {
				_, err = sqlite.Open(path)
			}
			if !errors.Is(err, sessions.ErrUnsupportedFormat) || !reflect.DeepEqual(before, snapshot()) {
				t.Fatalf("V3 data changed or was silently migrated: %v", err)
			}
		})
	}
}

func TestVersionFourMarkersAgreeWithRuntimeState(t *testing.T) {
	if sessions.FormatVersion != 4 {
		t.Fatalf("ordered message format must be version 4, got %d", sessions.FormatVersion)
	}
	for _, backend := range []string{"jsonl", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			store, closeStore := durableStore(t, backend, dir)
			m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
				return answer("done"), nil
			})
			a := newAgent(t, testConfig(m, store))
			s := newSession(t, a, sessions.SessionOptions{})
			if s.State().FormatVersion != 4 || s.State().Creation.FormatVersion != 4 {
				t.Fatal("runtime state and creation did not adopt V4")
			}
			if err := a.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			closeStore()
			if backend == "jsonl" {
				data, err := os.ReadFile(filepath.Join(dir, "journal", "format.json"))
				var marker struct {
					Format int `json:"format_version"`
				}
				if err != nil || json.Unmarshal(data, &marker) != nil || marker.Format != 4 {
					t.Fatalf("JSONL marker: %s %v", data, err)
				}
				sum := sha256.Sum256([]byte(s.RolloutID()))
				data, err = os.ReadFile(filepath.Join(dir, "journal", hex.EncodeToString(sum[:])+".jsonl"))
				first := bytes.SplitN(data, []byte("\n"), 2)[0]
				if err != nil || json.Unmarshal(first, &marker) != nil || marker.Format != 4 {
					t.Fatalf("JSONL rollout header: %s %v", first, err)
				}
			} else {
				db, err := sql.Open("sqlite", filepath.Join(dir, "history.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				var format, userVersion int
				if err := db.QueryRow("SELECT version FROM format").Scan(&format); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil || format != 4 || userVersion != 4 {
					t.Fatalf("SQLite markers: format=%d user_version=%d %v", format, userVersion, err)
				}
			}
		})
	}
}

func TestProtocolPayloadsSurviveColdReopenAndFork(t *testing.T) {
	for _, backend := range []string{"sqlite", "jsonl"} {
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
			a := newAgent(t, c)
			s := newSession(t, a, sessions.SessionOptions{ID: "protocol"})
			r, err := s.Start(context.Background(), input)
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
			a = newAgent(t, c)
			s, err = a.OpenSession(context.Background(), "protocol")
			if err != nil {
				t.Fatal(err)
			}
			candidates := []*sessions.Session{s}
			for _, mode := range []sessions.ForkMode{sessions.ForkShared, sessions.ForkDetached} {
				child, err := s.Fork(context.Background(), s.Head(), sessions.ForkOptions{ID: fmt.Sprintf("child-%d", mode), Mode: mode})
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
			a = newAgent(t, c)
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
