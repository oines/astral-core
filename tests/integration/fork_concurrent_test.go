package integration_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	astral "github.com/oines/astral-core"
	sessions "github.com/oines/astral-core/session"
)

type pausedForkStore struct {
	sessions.Store
	entered chan struct{}
	release chan struct{}
}

func (st pausedForkStore) Fork(ctx context.Context, r sessions.ForkRequest) (sessions.RuntimeState, error) {
	close(st.entered)
	select {
	case <-st.release:
	case <-ctx.Done():
		return sessions.RuntimeState{}, ctx.Err()
	}
	return st.Store.Fork(ctx, r)
}

func TestRunningParentContinuesWhileForkIsPaused(t *testing.T) {
	eachStore(t, func(t *testing.T, base sessions.Store) {
		entered, release := make(chan struct{}), make(chan struct{})
		st := pausedForkStore{base, make(chan struct{}), make(chan struct{})}
		var once sync.Once
		m := model(func(ctx context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			if n == 2 {
				once.Do(func() { close(entered) })
				select {
				case <-release:
				case <-ctx.Done():
					return astral.Reply{}, ctx.Err()
				}
			}
			return answer("done"), nil
		})
		a := newAgent(t, testConfig(m, st))
		parent := newSession(t, a, sessions.SessionOptions{ID: "parent"})
		first := waitOK(t, start(t, parent, "first"))
		run := start(t, parent, "second")
		signal(t, entered)
		type result struct {
			child *sessions.Session
			err   error
		}
		finished := make(chan result, 1)
		go func() {
			child, err := parent.Fork(context.Background(), first.LastMessageID, sessions.ForkOptions{ID: "child"})
			finished <- result{child, err}
		}()
		signal(t, st.entered)
		if _, err := base.LoadState(context.Background(), "child"); !errors.Is(err, sessions.ErrNotFound) {
			t.Fatal("child visible before publication")
		}
		if _, err := parent.FollowUp(context.Background(), astral.User("parent queue")); err != nil {
			t.Fatalf("fork entered parent maintenance: %v", err)
		}
		close(release)
		waitOK(t, run)
		parentState := parent.State()
		close(st.release)
		out := <-finished
		if out.err != nil {
			t.Fatal(out.err)
		}
		if !reflect.DeepEqual(parent.State(), parentState) {
			t.Fatal("fork changed the running parent's state")
		}
		child := out.child
		view, err := child.InspectContext(context.Background())
		if err != nil || !reflect.DeepEqual(texts(view.Request.Messages, astral.RoleUser), []string{"first"}) {
			t.Fatalf("fixed fork boundary: %+v %v", view, err)
		}
		for _, e := range entries(child) {
			if e.SessionID == child.ID() && !e.IsHistory() && e.Kind != sessions.EntryCreated {
				t.Fatalf("operational fact copied: %s", e.Kind)
			}
		}
		waitOK(t, start(t, child, "child only"))
		if !reflect.DeepEqual(parent.State(), parentState) {
			t.Fatal("child modified parent")
		}
	})
}

func TestRunningParentRejectsIncompleteFork(t *testing.T) {
	eachStore(t, func(t *testing.T, st sessions.Store) {
		entered, release := make(chan struct{}), make(chan struct{})
		m := model(func(_ context.Context, n int, _ astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			if n == 1 {
				return calls(call("c", "wait", `{"value":"x"}`)), nil
			}
			return answer("done"), nil
		})
		c := testConfig(m, st)
		c.Tools = []astral.Tool{tool("wait", func(ctx context.Context, _ astral.Invocation, _ func(astral.ToolProgress)) (astral.ToolExecution, error) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return astral.ToolExecution{Result: &astral.ToolResult{}}, ctx.Err()
			}
			return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text("done")}}, nil
		})}
		parent := newSession(t, newAgent(t, c), sessions.SessionOptions{})
		run := start(t, parent, "go")
		signal(t, entered)
		if _, err := parent.Fork(context.Background(), parent.Head(), sessions.ForkOptions{ID: "invalid"}); !errors.Is(err, astral.ErrIncompleteBatch) {
			t.Fatalf("running partial fork: %v", err)
		}
		if _, err := parent.Fork(context.Background(), "not-committed", sessions.ForkOptions{ID: "invalid"}); !errors.Is(err, sessions.ErrNotFound) {
			t.Fatalf("uncommitted fork: %v", err)
		}
		close(release)
		waitOK(t, run)
	})
}

type ownershipLossStore struct {
	sessions.Store
	advanced bool
}

func (st *ownershipLossStore) Commit(ctx context.Context, id string, c sessions.Commit) (sessions.Receipt, error) {
	r, err := st.Store.Commit(ctx, id, c)
	if err != nil {
		return r, err
	}
	for _, e := range c.Entries {
		if e.Kind == sessions.EntryToolStarted && !st.advanced {
			st.advanced = true
			state, err := st.Store.LoadState(ctx, id)
			if err != nil {
				return r, err
			}
			other := makeCommit(state, state.Version, []sessions.Entry{{ID: "other-owner", Kind: sessions.EntryRun, Run: &sessions.RunRecord{ID: "other-owner", Status: "completed"}}})
			if _, err = st.Store.Commit(ctx, id, other); err != nil {
				return r, err
			}
			return sessions.Receipt{}, errors.New("ack lost before another owner wrote")
		}
	}
	return r, nil
}

func TestLostAckDoesNotAuthorizeAfterOwnerVersionChanges(t *testing.T) {
	eachStore(t, func(t *testing.T, base sessions.Store) {
		st := &ownershipLossStore{Store: base}
		m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
			return calls(call("c", "never", `{"value":"x"}`)), nil
		})
		c := testConfig(m, st)
		c.Tools = []astral.Tool{tool("never", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
			t.Error("execution continued after ownership changed")
			return astral.ToolExecution{Result: &astral.ToolResult{Content: astral.Text("unexpected")}}, nil
		})}
		s := newSession(t, newAgent(t, c), sessions.SessionOptions{})
		if _, err := wait(t, start(t, s, "go")); !errors.Is(err, sessions.ErrConflict) {
			t.Fatalf("owner conflict: %v", err)
		}
	})
}
