package integration_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/oines/astral-core"
)

func branchSeed(t *testing.T, st core.Store) core.RuntimeState {
	t.Helper()
	if err := appendStore(context.Background(), st, "session", 0, storeHeader()); err != nil {
		t.Fatal(err)
	}
	return branchState(t, st, "session")
}

func branchState(t *testing.T, st core.Store, id string) core.RuntimeState {
	t.Helper()
	state, err := st.LoadState(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func branchAppend(t *testing.T, st core.Store, id string, entries ...core.Entry) core.RuntimeState {
	t.Helper()
	state := branchState(t, st, id)
	if err := appendStore(context.Background(), st, id, state.Version, entries); err != nil {
		t.Fatal(err)
	}
	return branchState(t, st, id)
}

func branchFork(t *testing.T, st core.Store, source, target, head string, mode core.ForkMode) core.RuntimeState {
	t.Helper()
	state := branchState(t, st, source)
	child, err := st.Fork(context.Background(), core.ForkRequest{SourceID: source, SourceRolloutID: state.RolloutID, TargetID: target, HeadID: head, Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	return child
}

func branchAttempt(t *testing.T, st core.Store, id, head string) core.ModelAttempt {
	t.Helper()
	state := branchState(t, st, id)
	base, err := core.ReadContext(context.Background(), st, state.RolloutID, head, core.DefaultContextBytes)
	if err != nil {
		t.Fatal(err)
	}
	input, err := core.FreezeRequest(state.RolloutID, base, base.Request, core.ModelSpec{ID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	attempt := core.ModelAttempt{ID: "attempt", SessionID: id, Purpose: "generation", Number: 1, Version: state.Version + 1, StartedAt: time.Now().UTC(), Input: input}
	commit := makeCommit(state, state.Version, nil)
	commit.Attempts = []core.AttemptMutation{{ID: attempt.ID, Create: &attempt}}
	if _, err = st.Commit(context.Background(), id, commit); err != nil {
		t.Fatal(err)
	}
	return attempt
}

func TestDeleteBranchRemovesSharedDescendantsAndPreservesOtherBranches(t *testing.T) {
	eachStore(t, func(t *testing.T, st core.Store) {
		ctx := context.Background()
		root := branchSeed(t, st)
		branchFork(t, st, "session", "child", "config", core.ForkShared)
		branchAppend(t, st, "child", storeMessage("child-message", "config"))
		branchFork(t, st, "child", "grandchild", "child-message", core.ForkShared)
		branchAppend(t, st, "grandchild", storeMessage("grandchild-message", "child-message"))
		branchFork(t, st, "session", "sibling", "config", core.ForkShared)
		branchAppend(t, st, "sibling", storeMessage("sibling-message", "config"))
		copy := branchFork(t, st, "child", "detached", "child-message", core.ForkDetached)
		parentBefore := branchAppend(t, st, "session", storeMessage("parent-message", "config"))
		siblingBefore := branchState(t, st, "sibling")
		attempt := branchAttempt(t, st, "child", "child-message")
		message := core.User("queued work")
		input := core.InputRecord{ID: "pending", Kind: core.InputFollowUp, Digest: strings.Repeat("a", 64), Message: &message}
		branchAppend(t, st, "child", core.Entry{ID: "queued-entry", Kind: core.EntryInputQueued, Input: &input})
		deleter := st.(core.BranchDeleter)
		result, err := deleter.DeleteBranch(ctx, core.BranchDeleteRequest{SessionID: "child"})
		if err != nil || result.RolloutID != root.RolloutID || !reflect.DeepEqual(result.SessionIDs, []string{"child", "grandchild"}) {
			t.Fatalf("branch deletion: %+v %v", result, err)
		}
		for _, id := range result.SessionIDs {
			if _, err := st.LoadState(ctx, id); !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("deleted Session %s remains: %v", id, err)
			}
		}
		for _, id := range []string{"child-message", "grandchild-message", "queued-entry"} {
			if _, err := st.Get(ctx, root.RolloutID, id); !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("deleted entry %s remains: %v", id, err)
			}
		}
		if _, err := st.GetAttempt(ctx, "child", attempt.ID); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("deleted attempt remains: %v", err)
		}
		if _, err := st.LookupInput(ctx, "child", input.ID); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("deleted input remains: %v", err)
		}
		if _, err := st.LookupCommit(ctx, "child", core.ForkCommitID); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("deleted receipt remains: %v", err)
		}
		if after := branchState(t, st, "session"); !reflect.DeepEqual(parentBefore, after) {
			t.Fatal("parent state changed")
		}
		if after := branchState(t, st, "sibling"); !reflect.DeepEqual(siblingBefore, after) {
			t.Fatal("sibling state changed")
		}
		if _, err := core.ReadContext(ctx, st, copy.RolloutID, copy.Branch.HeadID, core.DefaultContextBytes); err != nil {
			t.Fatalf("independent detached history damaged: %v", err)
		}
		if retry, err := deleter.DeleteBranch(ctx, core.BranchDeleteRequest{SessionID: "child"}); err != nil || len(retry.SessionIDs) != 0 {
			t.Fatalf("retry: %+v %v", retry, err)
		}
		branchAppend(t, st, "session", storeMessage("parent-next", "parent-message"))
		branchAppend(t, st, "sibling", storeMessage("sibling-next", "sibling-message"))
		// A root history node removes the complete shared Rollout, but cannot
		// reach a detached Rollout even though it remembers source provenance.
		result, err = deleter.DeleteBranch(ctx, core.BranchDeleteRequest{RolloutID: root.RolloutID, EntryID: "config"})
		if err != nil || !reflect.DeepEqual(result.SessionIDs, []string{"session", "sibling"}) {
			t.Fatalf("root deletion: %+v %v", result, err)
		}
		if _, err := st.Get(ctx, root.RolloutID, "config"); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("root Rollout remains: %v", err)
		}
		if _, err := core.ReadContext(ctx, st, copy.RolloutID, copy.Branch.HeadID, core.DefaultContextBytes); err != nil {
			t.Fatal("root deletion damaged detached copy:", err)
		}
	})
}

func TestDeleteBranchEmptySharedForkRetainsInheritedBoundary(t *testing.T) {
	eachStore(t, func(t *testing.T, st core.Store) {
		ctx := context.Background()
		before := branchSeed(t, st)
		branchFork(t, st, "session", "empty", "config", core.ForkShared)
		result, err := st.(core.BranchDeleter).DeleteBranch(ctx, core.BranchDeleteRequest{SessionID: "empty"})
		if err != nil || !reflect.DeepEqual(result.SessionIDs, []string{"empty"}) {
			t.Fatalf("empty fork deletion: %+v %v", result, err)
		}
		if _, err := st.Get(ctx, before.RolloutID, "config"); err != nil {
			t.Fatal("deleted shared Fork point:", err)
		}
		if after := branchState(t, st, "session"); !reflect.DeepEqual(before, after) {
			t.Fatal("parent changed")
		}
	})
}

func TestDeleteBranchAtHistoryNodeRetainsEarlierWriterHistory(t *testing.T) {
	eachStore(t, func(t *testing.T, st core.Store) {
		ctx := context.Background()
		root := branchSeed(t, st)
		branchAppend(t, st, "session", storeMessage("prefix", "config"))
		branchFork(t, st, "session", "sibling", "prefix", core.ForkShared)
		branchAppend(t, st, "sibling", storeMessage("sibling-message", "prefix"))
		branchAppend(t, st, "session", storeMessage("cut", "prefix"), storeMessage("tail", "cut"))
		result, err := st.(core.BranchDeleter).DeleteBranch(ctx, core.BranchDeleteRequest{RolloutID: root.RolloutID, EntryID: "cut"})
		if err != nil || !reflect.DeepEqual(result.SessionIDs, []string{"session"}) {
			t.Fatalf("subtree deletion: %+v %v", result, err)
		}
		for _, id := range []string{"config", "prefix", "sibling-message"} {
			if _, err := st.Get(ctx, root.RolloutID, id); err != nil {
				t.Fatalf("ancestor/sibling %s lost: %v", id, err)
			}
		}
		for _, id := range []string{"cut", "tail"} {
			if _, err := st.Get(ctx, root.RolloutID, id); !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("subtree entry %s remains: %v", id, err)
			}
		}
		branchAppend(t, st, "sibling", storeMessage("sibling-next", "sibling-message"))
	})
}

func TestDeleteBranchProtectsSurvivingFrozenAttemptOutsideCurrentHead(t *testing.T) {
	eachStore(t, func(t *testing.T, st core.Store) {
		ctx := context.Background()
		branchSeed(t, st)
		branchFork(t, st, "session", "child", "config", core.ForkShared)
		branchAppend(t, st, "child", storeMessage("child-message", "config"))
		branchAppend(t, st, "session", storeMessage("parent-message", "config"))
		// Attempt input is an immutable independent boundary. The parent's
		// current head remains outside the child branch selected for deletion.
		attempt := branchAttempt(t, st, "session", "child-message")
		branchAppend(t, st, "session", core.Entry{ID: "attempt-run-ended", Kind: core.EntryRun, Run: &core.RunRecord{ID: "attempt-run", Status: "cancelled"}})
		before := branchState(t, st, "session")
		if before.CurrentAttemptID != "" {
			t.Fatal("fixture did not clear the current-attempt pointer")
		}
		frozen, err := core.RebuildRequest(ctx, st, attempt.Input, core.DefaultContextBytes)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.(core.BranchDeleter).DeleteBranch(ctx, core.BranchDeleteRequest{SessionID: "child"}); !errors.Is(err, core.ErrConflict) {
			t.Fatalf("surviving attempt dependency lost: %v", err)
		}
		if after := branchState(t, st, "session"); !reflect.DeepEqual(before, after) {
			t.Fatal("rejected deletion changed parent state")
		}
		branchState(t, st, "child")
		after, err := core.RebuildRequest(ctx, st, attempt.Input, core.DefaultContextBytes)
		if err != nil || !reflect.DeepEqual(frozen, after) {
			t.Fatalf("frozen request changed after rejection: %v", err)
		}
	})
}

func TestDeleteBranchRejectsInvalidOrCanceledRequestWithoutMutation(t *testing.T) {
	eachStore(t, func(t *testing.T, st core.Store) {
		before := branchSeed(t, st)
		deleter := st.(core.BranchDeleter)
		for _, request := range []core.BranchDeleteRequest{{}, {SessionID: "session", RolloutID: before.RolloutID}, {SessionID: "session", EntryID: "config"}, {RolloutID: before.RolloutID, EntryID: "created"}} {
			if _, err := deleter.DeleteBranch(context.Background(), request); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("invalid selector accepted: %+v %v", request, err)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := deleter.DeleteBranch(ctx, core.BranchDeleteRequest{SessionID: "session"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
		if after := branchState(t, st, "session"); !reflect.DeepEqual(before, after) {
			t.Fatal("invalid/canceled deletion changed state")
		}
	})
}

func TestDeleteBranchSerializesConcurrentCommitAndSharedFork(t *testing.T) {
	eachStore(t, func(t *testing.T, st core.Store) {
		ctx := context.Background()
		before := branchSeed(t, st)
		for i := range 8 {
			childID, lateID := fmt.Sprintf("child-%d", i), fmt.Sprintf("late-%d", i)
			headID, nextID := fmt.Sprintf("head-%d", i), fmt.Sprintf("next-%d", i)
			branchFork(t, st, "session", childID, "config", core.ForkShared)
			child := branchAppend(t, st, childID, storeMessage(headID, "config"))
			commit := makeCommit(child, child.Version, []core.Entry{storeMessage(nextID, headID)})
			start := make(chan struct{})
			forked, committed, deleted := make(chan error, 1), make(chan error, 1), make(chan error, 1)
			go func() {
				<-start
				_, err := st.Fork(ctx, core.ForkRequest{SourceID: childID, SourceRolloutID: before.RolloutID, TargetID: lateID, HeadID: headID, Mode: core.ForkShared})
				forked <- err
			}()
			go func() {
				<-start
				_, err := st.Commit(ctx, childID, commit)
				committed <- err
			}()
			go func() {
				<-start
				_, err := st.(core.BranchDeleter).DeleteBranch(ctx, core.BranchDeleteRequest{SessionID: childID})
				deleted <- err
			}()
			close(start)
			if err := <-deleted; err != nil {
				t.Fatal("concurrent deletion failed:", err)
			}
			if err := <-forked; err != nil && !errors.Is(err, core.ErrNotFound) {
				t.Fatal("unexpected concurrent Fork result:", err)
			}
			<-committed // A stale append may fail after the deletion linearizes.
			for _, id := range []string{childID, lateID} {
				if _, err := st.LoadState(ctx, id); !errors.Is(err, core.ErrNotFound) {
					t.Fatalf("concurrent operation resurrected Session %s: %v", id, err)
				}
			}
			for _, id := range []string{headID, nextID} {
				if _, err := st.Get(ctx, before.RolloutID, id); !errors.Is(err, core.ErrNotFound) {
					t.Fatalf("concurrent operation resurrected entry %s: %v", id, err)
				}
			}
		}
		if after := branchState(t, st, "session"); !reflect.DeepEqual(before, after) {
			t.Fatal("concurrent maintenance changed surviving parent")
		}
	})
}
