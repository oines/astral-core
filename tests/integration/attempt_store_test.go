package integration_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	core "github.com/oines/astral-core"
)

func TestAttemptOnlyCommitReceiptAndFailedAdoptionAreAtomic(t *testing.T) {
	eachStore(t, func(t *testing.T, st core.Store) {
		ctx := context.Background()
		if err := appendStore(ctx, st, "session", 0, storeHeader()); err != nil {
			t.Fatal(err)
		}
		before, err := st.LoadState(ctx, "session")
		if err != nil {
			t.Fatal(err)
		}
		base, err := core.ReadContext(ctx, st, before.RolloutID, before.Branch.HeadID, core.DefaultContextBytes)
		if err != nil {
			t.Fatal(err)
		}
		input, err := core.FreezeRequest(before.RolloutID, base, base.Request, core.ModelSpec{ID: "model"})
		if err != nil {
			t.Fatal(err)
		}
		attempt := core.ModelAttempt{ID: "attempt", SessionID: "session", Purpose: "generation", Number: 1, Version: before.Version + 1, StartedAt: time.Now().UTC(), Input: input}
		commit := makeCommit(before, before.Version, nil)
		commit.Attempts = []core.AttemptMutation{{ID: attempt.ID, Create: &attempt}}
		receipt, err := st.Commit(ctx, "session", commit)
		if err != nil {
			t.Fatal(err)
		}
		if receipt.FirstSequence != 0 || receipt.LastSequence != 0 {
			t.Fatal("attempt allocated conversation sequence numbers")
		}
		again, err := st.Commit(ctx, "session", commit)
		if err != nil || again != receipt {
			t.Fatalf("attempt retry was not idempotent: %v", err)
		}
		confirmed, err := commit.Confirmed(receipt)
		if err != nil {
			t.Fatal(err)
		}
		reduced, records, err := core.Reduce(before, confirmed)
		if err != nil || len(records) != 0 {
			t.Fatalf("attempt-only reducer: %v", err)
		}
		after, err := st.LoadState(ctx, "session")
		if err != nil || !reflect.DeepEqual(after, reduced) || !reflect.DeepEqual(after.Branch, before.Branch) {
			t.Fatal("attempt changed the history projection")
		}
		attempt.Input.Digest = "changed"
		if _, err = st.Commit(ctx, "session", commit); !errors.Is(err, core.ErrConflict) {
			t.Fatalf("same receipt identity accepted changed input: %v", err)
		}
		invalid := makeCommit(after, after.Version, []core.Entry{storeMessage("must-not-commit", "config")})
		invalid.Attempts = []core.AttemptMutation{{ID: "attempt", Finish: &core.AttemptOutcome{Status: "completed", FinishedAt: time.Now().UTC(), EntryID: "missing-assistant"}}}
		if _, err = st.Commit(ctx, "session", invalid); !errors.Is(err, core.ErrCorrupt) {
			t.Fatalf("invalid adoption accepted: %v", err)
		}
		unchanged, err := st.LoadState(ctx, "session")
		if err != nil || !reflect.DeepEqual(unchanged, after) {
			t.Fatal("rejected attempt completion changed runtime")
		}
		if _, err = st.Get(ctx, before.RolloutID, "must-not-commit"); !errors.Is(err, core.ErrNotFound) {
			t.Fatal("history partially committed")
		}
		stored, err := st.GetAttempt(ctx, "session", "attempt")
		if err != nil || stored.Outcome != nil {
			t.Fatal("attempt partially completed")
		}
	})
}

func TestForkHistoryDoesNotDependOnSourceAttempts(t *testing.T) {
	for _, mode := range []core.ForkMode{core.ForkShared, core.ForkDetached} {
		t.Run(map[core.ForkMode]string{core.ForkShared: "shared", core.ForkDetached: "detached"}[mode], func(t *testing.T) {
			eachStore(t, func(t *testing.T, st core.Store) {
				ctx := context.Background()
				m := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
					return answer("self-contained reply"), nil
				})
				parent := newSession(t, newSessions(t, testConfig(m, st)), core.SessionOptions{ID: "parent"})
				waitOK(t, start(t, parent, "original input"))
				original := allAttempts(t, parent)[0]
				frozen, err := parent.Attempts().Request(ctx, original.ID)
				if err != nil {
					t.Fatal(err)
				}
				child, err := parent.Fork(ctx, parent.Head(), core.ForkOptions{ID: "child", Mode: mode})
				if err != nil {
					t.Fatal(err)
				}
				defer child.Close(ctx)
				if len(allAttempts(t, child)) != 0 {
					t.Fatal("fork cloned operational attempts")
				}
				if _, err = child.Attempts().Get(ctx, original.ID); !errors.Is(err, core.ErrNotFound) {
					t.Fatal("child resolved parent attempt as its own")
				}
				view, err := child.InspectContext(ctx)
				if err != nil || len(view.Request.Messages) != 2 || view.Request.Messages[1].Content[0].Text != "self-contained reply" {
					t.Fatalf("clone needs source output to rebuild context: %v", err)
				}
				waitOK(t, start(t, child, "child-only input"))
				waitOK(t, start(t, parent, "parent-only input"))
				rebuilt, err := parent.Attempts().Request(ctx, original.ID)
				if err != nil || !reflect.DeepEqual(rebuilt, frozen) {
					t.Fatalf("branch growth changed a pinned request: %v", err)
				}
				if len(allAttempts(t, child)) != 1 || len(allAttempts(t, parent)) != 2 {
					t.Fatal("attempts crossed session boundaries")
				}
			})
		})
	}
}
