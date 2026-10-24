package integration_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	core "github.com/oines/astral-core"
)

func TestPermanentDeletionRemovesPrivateHistoryAndPreservesDetachedFork(t *testing.T) {
	eachStore(t, func(t *testing.T, st core.Store) {
		ctx := context.Background()
		m := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
			return answer("private reply"), nil
		})
		parent := newSession(t, newSessions(t, testConfig(m, st)), core.SessionOptions{ID: "parent"})
		waitOK(t, start(t, parent, "private input"))
		attempt := allAttempts(t, parent)[0]
		rollout, head := parent.RolloutID(), parent.Head()
		child, err := parent.Fork(ctx, head, core.ForkOptions{ID: "copy", Mode: core.ForkDetached})
		if err != nil {
			t.Fatal(err)
		}
		defer child.Close(ctx)
		before, err := child.InspectContext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = parent.Close(ctx); err != nil {
			t.Fatal(err)
		}
		deleter := st.(core.SessionDeleter)
		if err = deleter.DeleteSession(ctx, parent.ID()); err != nil {
			t.Fatal(err)
		}
		if err = deleter.DeleteSession(ctx, parent.ID()); err != nil {
			t.Fatal("delete retry:", err)
		}
		if _, err = st.LoadState(ctx, parent.ID()); !errors.Is(err, core.ErrNotFound) {
			t.Fatal("deleted state is accessible:", err)
		}
		if _, err = st.Get(ctx, rollout, head); !errors.Is(err, core.ErrNotFound) {
			t.Fatal("deleted history is accessible:", err)
		}
		if _, err = st.GetAttempt(ctx, parent.ID(), attempt.ID); !errors.Is(err, core.ErrNotFound) {
			t.Fatal("deleted attempt is accessible:", err)
		}
		after, err := child.InspectContext(ctx)
		if err != nil || !reflect.DeepEqual(before.Request, after.Request) {
			t.Fatal("detached copy changed:", err)
		}
	})
}

func TestPermanentDeletionRejectsSharedRolloutWithoutMutation(t *testing.T) {
	eachStore(t, func(t *testing.T, st core.Store) {
		ctx := context.Background()
		if err := appendStore(ctx, st, "session", 0, storeHeader()); err != nil {
			t.Fatal(err)
		}
		state, err := st.LoadState(ctx, "session")
		if err != nil {
			t.Fatal(err)
		}
		_, err = st.Fork(ctx, core.ForkRequest{SourceID: "session", SourceRolloutID: state.RolloutID, TargetID: "shared", HeadID: state.Branch.HeadID, Mode: core.ForkShared})
		if err != nil {
			t.Fatal(err)
		}
		before, err := st.LoadState(ctx, "session")
		if err != nil {
			t.Fatal(err)
		}
		if err = st.(core.SessionDeleter).DeleteSession(ctx, "session"); !errors.Is(err, core.ErrConflict) {
			t.Fatal("shared history deletion accepted:", err)
		}
		after, err := st.LoadState(ctx, "session")
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("rejected deletion changed state:", err)
		}
		if _, err = st.LoadState(ctx, "shared"); err != nil {
			t.Fatal("shared child lost:", err)
		}
	})
}
