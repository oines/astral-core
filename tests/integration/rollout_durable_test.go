package integration_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	astral "github.com/oines/astral-core"
	sessions "github.com/oines/astral-core/session"
)

func TestDetachedReopensWithoutSourceRollout(t *testing.T) {
	for _, backend := range []string{"jsonl", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			st, closeStore := durableStore(t, backend, dir)
			m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
				return answer("done"), nil
			})
			c := testConfig(m, st)
			a := newAgent(t, c)
			parent := newSession(t, a, sessions.SessionOptions{ID: "parent"})
			for i := 0; i < 3; i++ {
				waitOK(t, start(t, parent, "ancestor"))
			}
			child, err := parent.Fork(ctx, parent.Head(), sessions.ForkOptions{ID: "independent", Mode: sessions.ForkDetached})
			if err != nil {
				t.Fatal(err)
			}
			if child.State().Version != 1 {
				t.Fatal("child version inherited")
			}
			before, err := child.InspectContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			saved := child.State()
			root := parent.RolloutID()
			if err = a.Close(ctx); err != nil {
				t.Fatal(err)
			}
			closeStore()
			if backend == "jsonl" {
				sum := sha256.Sum256([]byte(root))
				path := filepath.Join(dir, "journal", hex.EncodeToString(sum[:])+".jsonl")
				if err = os.Rename(path, filepath.Join(dir, "unavailable-source")); err != nil {
					t.Fatal(err)
				}
			} else {
				db, err := sql.Open("sqlite", filepath.Join(dir, "history.db"))
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec("UPDATE rollouts SET status='unavailable' WHERE id=?", root)
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			st, closeStore = durableStore(t, backend, dir)
			defer closeStore()
			c.Store = st
			a = newAgent(t, c)
			if _, err = a.OpenSession(ctx, "parent"); !errors.Is(err, sessions.ErrNotFound) {
				t.Fatalf("source still accessible: %v", err)
			}
			child, err = a.OpenSession(ctx, "independent")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(child.State(), saved) {
				t.Fatal("cold replay reduced imported ancestors as child commits")
			}
			after, err := child.InspectContext(ctx)
			if err != nil || !reflect.DeepEqual(before.Request, after.Request) {
				t.Fatalf("detached context lost: %v", err)
			}
			waitOK(t, start(t, child, "source independent"))
			request := sessions.ForkRequest{SourceID: "parent", SourceRolloutID: root, TargetID: "independent", HeadID: saved.Creation.ForkEntryID, Mode: sessions.ForkDetached}
			retried, err := st.Fork(ctx, request)
			if err != nil || retried.Version != child.State().Version {
				t.Fatalf("existing child depends on source: %v", err)
			}
		})
	}
}

func TestHistoricalToolResultForkSurvivesColdReplay(t *testing.T) {
	for _, backend := range []string{"sqlite", "jsonl"} {
		for _, mode := range []sessions.ForkMode{sessions.ForkShared, sessions.ForkDetached} {
			t.Run(fmt.Sprintf("%s/%d", backend, mode), func(t *testing.T) {
				ctx := context.Background()
				dir := t.TempDir()
				st, closeStore := durableStore(t, backend, dir)
				var effects atomic.Int32
				m := model(func(_ context.Context, _ int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
					if len(texts(r.Messages, astral.RoleTool)) == 0 {
						return calls(call("historical", "echo", `{"value":"preserved"}`)), nil
					}
					return answer("done"), nil
				})
				c := testConfig(m, st)
				c.Tools = []astral.Tool{echoTool(&effects)}
				a := newAgent(t, c)
				parent := newSession(t, a, sessions.SessionOptions{})
				waitOK(t, start(t, parent, "run"))
				var boundary string
				for _, e := range entries(parent) {
					if e.Kind == sessions.EntryToolResult {
						boundary = e.ID
					}
				}
				child, err := parent.Fork(ctx, boundary, sessions.ForkOptions{ID: "cold-result", Mode: mode})
				if err != nil {
					t.Fatal(err)
				}
				initial := child.State()
				if err = a.Close(ctx); err != nil {
					t.Fatal(err)
				}
				closeStore()
				st, closeStore = durableStore(t, backend, dir)
				defer closeStore()
				c.Store = st
				a = newAgent(t, c)
				child, err = a.OpenSession(ctx, "cold-result")
				if err != nil || !reflect.DeepEqual(child.State(), initial) {
					t.Fatalf("replay lost result projection: %v", err)
				}
				r, err := child.Continue(ctx)
				if err != nil {
					t.Fatal(err)
				}
				waitOK(t, r)
				if effects.Load() != 1 {
					t.Fatal("cold fork repeated tool")
				}
				request := m.observed()[len(m.observed())-1]
				if !reflect.DeepEqual(texts(request.Messages, astral.RoleTool), []string{"preserved"}) {
					t.Fatal("historical result lost")
				}
			})
		}
	}
}
