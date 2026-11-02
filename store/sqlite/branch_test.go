package sqlite

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/oines/astral-core"
)

func TestBranchDeletionCollectsOnlyUnreferencedPayloads(t *testing.T) {
	ctx := context.Background()
	st := payloadStore(t)
	shared := core.User(strings.Repeat("shared ancestor", 1200))
	payloadCommit(t, st, "s", []core.Entry{{ID: "ancestor", ParentID: "config", Kind: core.EntryMessage, Message: &shared}}, nil)
	child, err := st.Fork(ctx, core.ForkRequest{SourceID: "s", SourceRolloutID: "r", TargetID: "child", HeadID: "ancestor", Mode: core.ForkShared})
	if err != nil {
		t.Fatal(err)
	}
	discarded := strings.Repeat("discard branch", 1300)
	message := core.User(discarded)
	payloadCommit(t, st, "child", []core.Entry{{ID: "child-message", ParentID: child.Branch.HeadID, Kind: core.EntryMessage, Message: &message}}, nil)
	copy, err := st.Fork(ctx, core.ForkRequest{SourceID: "child", SourceRolloutID: "r", TargetID: "copy", HeadID: "child-message", Mode: core.ForkDetached})
	if err != nil {
		t.Fatal(err)
	}
	queueOnly := strings.Repeat("only child queue", 1400)
	queuedMessage := core.User(queueOnly)
	input := core.InputRecord{ID: "queued", Kind: core.InputFollowUp, Digest: strings.Repeat("a", 64), Message: &queuedMessage}
	payloadCommit(t, st, "child", []core.Entry{{ID: "queue-entry", Kind: core.EntryInputQueued, Input: &input}}, nil)
	base, err := core.ReadContext(ctx, st, "r", "child-message", core.DefaultContextBytes)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := core.FreezeRequest("r", base, base.Request, core.ModelSpec{ID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	state, err := st.LoadState(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	attempt := core.ModelAttempt{ID: "attempt", SessionID: "child", Number: 1, Version: state.Version + 1, Purpose: "compaction", StartedAt: time.Now().UTC(), Input: frozen}
	payloadCommit(t, st, "child", nil, []core.AttemptMutation{{ID: attempt.ID, Create: &attempt}})
	attemptOnly := strings.Repeat("failed model body", 1500)
	reply := core.Reply{Message: core.Message{Role: core.RoleAssistant, Content: core.Text(attemptOnly)}}
	payloadCommit(t, st, "child", nil, []core.AttemptMutation{{ID: attempt.ID, Finish: &core.AttemptOutcome{Status: "failed", FinishedAt: time.Now().UTC(), Reply: &reply}}})
	if n := payloadCount(t, st); n != 4 {
		t.Fatalf("expected four unique bodies before deletion, got %d", n)
	}
	if _, err = st.DeleteBranch(ctx, core.BranchDeleteRequest{SessionID: "child"}); err != nil {
		t.Fatal(err)
	}
	if n := payloadCount(t, st); n != 2 {
		t.Fatalf("queue/attempt bodies leaked or shared bodies were removed: %d", n)
	}
	for _, table := range []string{"sessions", "pending", "inputs", "attempts", "receipts"} {
		column := "session_id"
		if table == "sessions" {
			column = "id"
		}
		var count int
		if err = st.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE `+column+`=?`, "child").Scan(&count); err != nil || count != 0 {
			t.Fatalf("removed child remains in %s: count=%d err=%v", table, count, err)
		}
	}
	var dangling int
	if err = st.db.QueryRow(`SELECT count(*) FROM payload_refs WHERE owner_kind='state' AND owner_key=? OR owner_kind='attempt' AND owner_key=? OR owner_kind='entry' AND owner_key=?`, ownerKey("child"), ownerKey("child", "attempt"), ownerKey("r", "child-message")).Scan(&dangling); err != nil || dangling != 0 {
		t.Fatalf("deleted payload owners remain: %d %v", dangling, err)
	}
	if _, err = core.ReadContext(ctx, st, copy.RolloutID, copy.Branch.HeadID, core.DefaultContextBytes); err != nil {
		t.Fatal("detached copy lost shared payloads:", err)
	}
	if _, err = st.DeleteBranch(ctx, core.BranchDeleteRequest{SessionID: "copy"}); err != nil {
		t.Fatal(err)
	}
	if n := payloadCount(t, st); n != 1 {
		t.Fatalf("deleted detached copy's unique body remains: %d", n)
	}
	if _, err = st.DeleteBranch(ctx, core.BranchDeleteRequest{RolloutID: "r"}); err != nil {
		t.Fatal(err)
	}
	if n := payloadCount(t, st); n != 0 {
		t.Fatalf("root payloads remain: %d", n)
	}
}

func TestBranchDeletionRollsBackPayloadsAndStateOnSQLFailure(t *testing.T) {
	ctx := context.Background()
	st := payloadStore(t)
	child, err := st.Fork(ctx, core.ForkRequest{SourceID: "s", SourceRolloutID: "r", TargetID: "child", HeadID: "config", Mode: core.ForkShared})
	if err != nil {
		t.Fatal(err)
	}
	message := core.User(strings.Repeat("retained after rollback", 1000))
	payloadCommit(t, st, "child", []core.Entry{{ID: "child-message", ParentID: child.Branch.HeadID, Kind: core.EntryMessage, Message: &message}}, nil)
	before, err := st.LoadState(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.Exec(`CREATE TRIGGER fail_branch_delete BEFORE DELETE ON sessions WHEN OLD.id='child' BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DeleteBranch(ctx, core.BranchDeleteRequest{SessionID: "child"}); err == nil {
		t.Fatal("injected SQL failure was ignored")
	}
	after, err := st.LoadState(ctx, "child")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("state changed despite rollback: %v", err)
	}
	if _, err = core.ReadContext(ctx, st, before.RolloutID, before.Branch.HeadID, core.DefaultContextBytes); err != nil {
		t.Fatal("payload refs changed despite rollback:", err)
	}
	if _, err = st.db.Exec(`DROP TRIGGER fail_branch_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DeleteBranch(ctx, core.BranchDeleteRequest{SessionID: "child"}); err != nil {
		t.Fatal("retry failed:", err)
	}
}

func TestBranchDeletionCannotBeUndoneByForkPublication(t *testing.T) {
	for _, test := range []struct {
		name, source string
		mode         core.ForkMode
	}{
		{"shared-deleted-source", "child", core.ForkShared},
		{"shared-deleted-head", "s", core.ForkShared},
		{"detached-deleted-source", "child", core.ForkDetached},
		{"detached-deleted-head", "s", core.ForkDetached},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			st := payloadStore(t)
			child, err := st.Fork(ctx, core.ForkRequest{SourceID: "s", SourceRolloutID: "r", TargetID: "child", HeadID: "config", Mode: core.ForkShared})
			if err != nil {
				t.Fatal(err)
			}
			message := core.User(strings.Repeat("captured fork body", 1000))
			payloadCommit(t, st, "child", []core.Entry{{ID: "child-message", ParentID: child.Branch.HeadID, Kind: core.EntryMessage, Message: &message}}, nil)
			other, err := Open(st.path)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			captured, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				_, err := st.forkWithCheckpoint(ctx, core.ForkRequest{SourceID: test.source, SourceRolloutID: "r", TargetID: "late", HeadID: "child-message", Mode: test.mode}, func(phase string) {
					if phase == "before_source_check" {
						close(captured)
						select {
						case <-release:
						case <-ctx.Done():
						}
					}
				})
				done <- err
			}()
			select {
			case <-captured:
			case <-ctx.Done():
				t.Fatal("Fork did not capture its immutable boundary")
			}
			_, err = other.DeleteBranch(ctx, core.BranchDeleteRequest{SessionID: "child"})
			close(release)
			if err != nil {
				t.Fatal(err)
			}
			if err = <-done; !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("Fork published after source deletion: %v", err)
			}
			if _, err = st.LoadState(ctx, "late"); !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("deleted branch resurrected: %v", err)
			}
			var count int
			if err = st.db.QueryRow(`SELECT count(*) FROM sessions WHERE id IN ('child','late')`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("deleted/unpublished cursors remain: count=%d err=%v", count, err)
			}
			if n := payloadCount(t, st); n != 0 {
				t.Fatalf("unpublished Fork leaked payloads: %d", n)
			}
		})
	}
}
