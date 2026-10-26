package integration_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	astral "github.com/oines/astral-core"
)

func TestForkBoundaryMatrixAndHistoricalResults(t *testing.T) {
	eachStore(t, func(t *testing.T, st astral.Store) {
		for _, mode := range []astral.ForkMode{astral.ForkShared, astral.ForkDetached} {
			for _, terminate := range []bool{false, true} {
				t.Run(fmt.Sprintf("mode=%d/terminate=%v", mode, terminate), func(t *testing.T) {
					ctx := context.Background()
					var effects, before, after atomic.Int32
					m := model(func(_ context.Context, _ int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
						if len(texts(r.Messages, astral.RoleTool)) == 0 {
							return calls(call("same-provider-id", "echo", `{"value":"x"}`)), nil
						}
						return answer("done"), nil
					})
					c := testConfig(m, st)
					c.Tools = []astral.Tool{echoTool(&effects)}
					c.BeforeToolCall = func(context.Context, astral.ToolContext) (astral.ToolDecision, error) {
						before.Add(1)
						return astral.ToolDecision{}, nil
					}
					c.AfterToolCall = func(context.Context, astral.Invocation, astral.ToolResult) (astral.ToolResultPatch, error) {
						after.Add(1)
						return astral.ToolResultPatch{Terminate: &terminate}, nil
					}
					a := newSessions(t, c)
					parent := newSession(t, a, astral.SessionOptions{})
					waitOK(t, start(t, parent, "run tools"))
					parentState := parent.State()
					var user, partial, result string
					for _, e := range entries(parent) {
						if e.Message != nil && e.Message.Role == astral.RoleUser {
							user = e.ID
						}
						if e.Message != nil && len(e.Message.ToolCalls()) > 0 {
							partial = e.ID
						}
						if e.Kind == astral.EntryToolResult {
							result = e.ID
						}
					}
					if _, err := parent.Fork(ctx, partial, astral.ForkOptions{ID: parent.ID() + "-partial", Mode: mode}); !errors.Is(err, astral.ErrIncompleteBatch) {
						t.Fatalf("partial: %v", err)
					}
					if _, err := st.LoadState(ctx, parent.ID()+"-partial"); !errors.Is(err, astral.ErrNotFound) {
						t.Fatal("invalid child published")
					}
					child, err := parent.Fork(ctx, result, astral.ForkOptions{ID: parent.ID() + "-result", Mode: mode})
					if err != nil {
						t.Fatal(err)
					}
					if child.State().Version != 1 || len(child.State().Tools) != 0 || len(child.State().Approvals) != 0 {
						t.Fatal("execution facts inherited")
					}
					n := len(m.observed())
					run, err := child.Continue(ctx)
					if terminate {
						if !errors.Is(err, astral.ErrNotContinuable) {
							t.Fatalf("terminated batch continued: %v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
					if !terminate {
						waitOK(t, run)
					}
					wantCalls := n + 1
					if terminate {
						wantCalls = n
					}
					if effects.Load() != 1 || before.Load() != 1 || after.Load() != 1 || len(m.observed()) != wantCalls {
						t.Fatalf("history reexecuted effects=%d hooks=%d/%d model=%d want=%d", effects.Load(), before.Load(), after.Load(), len(m.observed()), wantCalls)
					}
					if !reflect.DeepEqual(parent.State(), parentState) {
						t.Fatal("child changed parent")
					}
					if !terminate {
						final, err := parent.Fork(ctx, parent.Head(), astral.ForkOptions{ID: parent.ID() + "-final", Mode: mode})
						if err != nil {
							t.Fatal(err)
						}
						n = len(m.observed())
						if _, err := final.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
							t.Fatalf("completed boundary accepted: %v", err)
						}
						if len(m.observed()) != n {
							t.Fatal("completed assistant boundary called model")
						}
						waitOK(t, start(t, final, "more"))
					}
					fromUser, err := parent.Fork(ctx, user, astral.ForkOptions{ID: parent.ID() + "-user", Mode: mode})
					if err != nil {
						t.Fatal(err)
					}
					r, err := fromUser.Continue(ctx)
					if err != nil {
						t.Fatal(err)
					}
					waitOK(t, r)
					if effects.Load() != 2 {
						t.Fatal("user boundary did not execute new batch")
					}
				})
			}
		}
	})
}

func TestRolloutConcurrentSessionsSequenceAndReplay(t *testing.T) {
	eachStore(t, func(t *testing.T, st astral.Store) {
		ctx := context.Background()
		if err := appendStore(ctx, st, "session", 0, storeHeader()); err != nil {
			t.Fatal(err)
		}
		initial, _ := st.LoadState(ctx, "session")
		const workers, rounds = 6, 8
		states := make([]astral.RuntimeState, workers)
		for i := range states {
			var err error
			states[i], err = st.Fork(ctx, astral.ForkRequest{SourceID: "session", SourceRolloutID: initial.RolloutID, TargetID: fmt.Sprintf("worker-%d", i), HeadID: initial.Branch.HeadID})
			if err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for i := range states {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				state := states[i]
				for n := 0; n < rounds; n++ {
					msg := storeMessage(fmt.Sprintf("writer-%d-message-%d", i, n), state.Branch.HeadID)
					c := makeCommit(state, state.Version, []astral.Entry{msg})
					receipt, err := st.Commit(ctx, state.SessionID, c)
					if err != nil {
						errs <- err
						return
					}
					confirmed, err := c.Confirmed(receipt)
					if err != nil {
						errs <- err
						return
					}
					state, _, err = astral.Reduce(state, confirmed)
					if err != nil {
						errs <- err
						return
					}
				}
				actual, err := st.LoadState(ctx, state.SessionID)
				if err != nil {
					errs <- err
					return
				}
				if !reflect.DeepEqual(actual, state) {
					errs <- fmt.Errorf("live reduce differs for %s", state.SessionID)
					return
				}
				states[i] = state
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		parent, _ := st.LoadState(ctx, "session")
		if !reflect.DeepEqual(parent, initial) {
			t.Fatal("parent CAS advanced")
		}
		page, err := st.Scan(ctx, initial.RolloutID, astral.Query{Limit: 200})
		if err != nil {
			t.Fatal(err)
		}
		for i, r := range page.Records {
			if r.Entry.Sequence != uint64(i+1) {
				t.Fatalf("sequence gap or duplicate at %d", i)
			}
		}
		for _, state := range states {
			page, err := st.Scan(ctx, state.RolloutID, astral.Query{SessionID: state.SessionID})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Records) != rounds+1 {
				t.Fatal("writer filter leaked sibling records")
			}
			var replay astral.RuntimeState
			for _, r := range page.Records {
				replay, _, err = astral.Reduce(replay, astral.Commit{ID: fmt.Sprint(r.Entry.Version), ExpectedVersion: r.Entry.Version - 1, Entries: []astral.Entry{r.Entry}})
				if err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(state, replay) {
				t.Fatal("checkpoint / replay differ")
			}
		}
	})
}

func TestForkIdentityIsolationAndRetryAfterChildAdvances(t *testing.T) {
	eachStore(t, func(t *testing.T, st astral.Store) {
		ctx := context.Background()
		m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
			return answer("done"), nil
		})
		a := newSessions(t, testConfig(m, st))
		parent := newSession(t, a, astral.SessionOptions{})
		waitOK(t, start(t, parent, "first"))
		boundary := parent.Head()
		if _, err := followUp(parent, ctx, astral.User("parent only")); err != nil {
			t.Fatal(err)
		}
		saved := parent.State()
		for _, mode := range []astral.ForkMode{astral.ForkShared, astral.ForkDetached} {
			options := astral.ForkOptions{ID: fmt.Sprintf("child-%d", mode), Mode: mode}
			const parallel = 8
			var wg sync.WaitGroup
			results := make(chan *astral.Session, parallel)
			errs := make(chan error, parallel)
			for i := 0; i < parallel; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					child, err := parent.Fork(ctx, boundary, options)
					results <- child
					errs <- err
				}()
			}
			wg.Wait()
			close(results)
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			var child *astral.Session
			for got := range results {
				if child != nil {
					if child.ID() != got.ID() || child.RolloutID() != got.RolloutID() || !reflect.DeepEqual(child.State(), got.State()) {
						t.Fatal("fork retry changed durable identity")
					}
					got.Close(ctx)
					continue
				}
				child = got
			}
			if (mode == astral.ForkShared) != (child.RolloutID() == parent.RolloutID()) {
				t.Fatal("wrong rollout ownership")
			}
			if child.State().Version != 1 || len(child.State().PendingInputs) != 0 {
				t.Fatal("inherited execution state")
			}
			pending, err := st.Pending(ctx, child.ID(), astral.PendingQuery{})
			if err != nil || len(pending.Records) != 0 {
				t.Fatal("parent queue leaked")
			}
			original, ok := entry(parent, boundary)
			if !ok {
				t.Fatal("missing boundary")
			}
			copied, ok := entry(child, boundary)
			if !ok || !reflect.DeepEqual(original, copied) || original.Version <= child.State().Version {
				t.Fatal("ancestor provenance or version changed")
			}
			waitOK(t, start(t, child, "child input"))
			advanced := child.State()
			again, err := parent.Fork(ctx, boundary, options)
			if err != nil || !reflect.DeepEqual(again.State(), advanced) {
				t.Fatalf("retry reset child: %v", err)
			}
			changed := options
			changed.Mode = astral.ForkMode(1 - int(options.Mode))
			if _, err = parent.Fork(ctx, boundary, changed); !errors.Is(err, astral.ErrConflict) {
				t.Fatalf("different parameters accepted: %v", err)
			}
			if err = child.Close(ctx); err != nil {
				t.Fatal(err)
			}
			reopened, err := parent.Fork(ctx, boundary, options)
			if err != nil || !reflect.DeepEqual(reopened.State(), advanced) {
				t.Fatalf("reopen retry: %v", err)
			}
			grandchild, err := reopened.Fork(ctx, reopened.Head(), astral.ForkOptions{ID: fmt.Sprintf("grandchild-%d", mode)})
			if err != nil || grandchild.RolloutID() != reopened.RolloutID() {
				t.Fatalf("nested fork: %v", err)
			}
		}
		if !reflect.DeepEqual(parent.State(), saved) {
			t.Fatal("forks changed parent")
		}
		rollout := parent.RolloutID()
		if err := parent.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if parent.RolloutID() != rollout || len(entries(parent)) == 0 {
			t.Fatal("Close lost durable identity/history")
		}
		child, err := a.OpenSession(ctx, "child-0")
		if err != nil {
			t.Fatal(err)
		}
		waitOK(t, start(t, child, "after parent closes"))
	})
}

type lostForkReply struct {
	astral.Store
	lost atomic.Bool
}

func (st *lostForkReply) Fork(ctx context.Context, r astral.ForkRequest) (astral.RuntimeState, error) {
	state, err := st.Store.Fork(ctx, r)
	if err == nil && st.lost.CompareAndSwap(false, true) {
		return astral.RuntimeState{}, errors.New("published; reply lost")
	}
	return state, err
}
func TestForkLostPublicationReply(t *testing.T) {
	eachStore(t, func(t *testing.T, base astral.Store) {
		st := &lostForkReply{Store: base}
		m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
			return answer("done"), nil
		})
		a := newSessions(t, testConfig(m, st))
		s := newSession(t, a, astral.SessionOptions{})
		waitOK(t, start(t, s, "one"))
		child, err := s.Fork(context.Background(), s.Head(), astral.ForkOptions{ID: "lost-reply", Mode: astral.ForkDetached})
		if err != nil {
			t.Fatal(err)
		}
		r, err := base.LookupCommit(context.Background(), child.ID(), astral.ForkCommitID)
		if err != nil || r.Version != 1 {
			t.Fatalf("missing creation receipt: %v", err)
		}
	})
}

func TestUnknownEffectAndForeignRolloutRejectFork(t *testing.T) {
	eachStore(t, func(t *testing.T, st astral.Store) {
		ctx := context.Background()
		m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
			return calls(call("effect", "effect", `{"value":"x"}`)), nil
		})
		c := testConfig(m, st)
		c.Tools = []astral.Tool{tool("effect", func(context.Context, astral.Invocation, func(astral.ToolProgress)) (astral.ToolExecution, error) {
			return astral.ToolExecution{Result: &astral.ToolResult{Outcome: astral.OutcomeUnknown, Content: astral.Text("uncertain")}}, nil
		})}
		a := newSessions(t, c)
		parent := newSession(t, a, astral.SessionOptions{})
		if _, err := wait(t, start(t, parent, "go")); !errors.Is(err, astral.ErrOutcomeUnknown) {
			t.Fatal(err)
		}
		for _, mode := range []astral.ForkMode{astral.ForkShared, astral.ForkDetached} {
			id := fmt.Sprintf("unknown-%d", mode)
			if _, err := parent.Fork(ctx, parent.Head(), astral.ForkOptions{ID: id, Mode: mode}); !errors.Is(err, astral.ErrOutcomeUnknown) {
				t.Fatalf("unknown accepted: %v", err)
			}
			if _, err := st.LoadState(ctx, id); !errors.Is(err, astral.ErrNotFound) {
				t.Fatal("unknown child published")
			}
		}
		other := newSession(t, a, astral.SessionOptions{})
		if _, err := parent.Fork(ctx, other.Head(), astral.ForkOptions{ID: "foreign"}); !errors.Is(err, astral.ErrNotFound) {
			t.Fatalf("foreign rollout accepted: %v", err)
		}
		if _, err := parent.Fork(ctx, other.Head(), astral.ForkOptions{}); !errors.Is(err, astral.ErrInvalid) {
			t.Fatalf("missing target ID accepted: %v", err)
		}
	})
}

type siblingDuringLostAck struct {
	astral.Store
	sibling   string
	triggered atomic.Bool
}

func (st *siblingDuringLostAck) Commit(ctx context.Context, id string, c astral.Commit) (astral.Receipt, error) {
	receipt, err := st.Store.Commit(ctx, id, c)
	if err != nil {
		return receipt, err
	}
	if st.sibling != "" && id != st.sibling && st.triggered.CompareAndSwap(false, true) {
		state, err := st.LoadState(ctx, st.sibling)
		if err != nil {
			return receipt, err
		}
		next := makeCommit(state, state.Version, []astral.Entry{{ID: "sibling-write", Kind: astral.EntryRun, Run: &astral.RunRecord{ID: "sibling-run", Status: "completed"}}})
		if _, err = st.Store.Commit(ctx, st.sibling, next); err != nil {
			return receipt, err
		}
		return astral.Receipt{}, errors.New("reply lost while sibling advances rollout")
	}
	return receipt, nil
}
func TestLostAckRestoresConfirmedSequenceAfterSiblingCommit(t *testing.T) {
	eachStore(t, func(t *testing.T, base astral.Store) {
		ctx := context.Background()
		st := &siblingDuringLostAck{Store: base}
		m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
			return answer("done"), nil
		})
		a := newSessions(t, testConfig(m, st))
		parent := newSession(t, a, astral.SessionOptions{ID: "parent"})
		_, err := parent.Fork(ctx, parent.Head(), astral.ForkOptions{ID: "sibling"})
		if err != nil {
			t.Fatal(err)
		}
		st.sibling = "sibling"
		waitOK(t, start(t, parent, "ack recovery"))
		stored, err := base.LoadState(ctx, parent.ID())
		if err != nil || !reflect.DeepEqual(stored, parent.State()) {
			t.Fatalf("confirmed order diverged: %v", err)
		}
		all := entries(parent)
		for i, e := range all {
			if e.Sequence != uint64(i+1) {
				t.Fatal("retry allocated a second sequence range")
			}
		}
		if !st.triggered.Load() {
			t.Fatal("ack injection did not run")
		}
	})
}

func TestStoreForkReturnsIsolatedSnapshot(t *testing.T) {
	eachStore(t, func(t *testing.T, st astral.Store) {
		ctx := context.Background()
		if err := appendStore(ctx, st, "session", 0, storeHeader()); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []astral.ForkMode{astral.ForkShared, astral.ForkDetached} {
			r := astral.ForkRequest{SourceID: "session", SourceRolloutID: "session", TargetID: fmt.Sprintf("isolation-%d", mode), HeadID: "config", Mode: mode}
			state, err := st.Fork(ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			state.Creation.Projection.ConfigurationID = "mutated by caller"
			actual, err := st.LoadState(ctx, r.TargetID)
			if err != nil || actual.Creation.Projection.ConfigurationID != "config" {
				t.Fatal("Fork returned aliased creation seed")
			}
		}
	})
}

func TestParentAndSharedChildRunConcurrentlyAndCloseIndependently(t *testing.T) {
	eachStore(t, func(t *testing.T, st astral.Store) {
		parentEntered, childEntered, childRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
		m := model(func(ctx context.Context, _ int, r astral.Request, _ func(astral.ModelEvent)) (astral.Reply, error) {
			users := texts(r.Messages, astral.RoleUser)
			switch users[len(users)-1] {
			case "parent running":
				close(parentEntered)
				<-ctx.Done()
				return astral.Reply{}, ctx.Err()
			case "child running":
				close(childEntered)
				select {
				case <-childRelease:
				case <-ctx.Done():
					return astral.Reply{}, ctx.Err()
				}
			}
			return answer("done"), nil
		})
		a := newSessions(t, testConfig(m, st))
		parent := newSession(t, a, astral.SessionOptions{})
		first := waitOK(t, start(t, parent, "first"))
		parentRun := start(t, parent, "parent running")
		signal(t, parentEntered)
		child, err := parent.Fork(context.Background(), first.LastMessageID, astral.ForkOptions{ID: "concurrent-child"})
		if err != nil {
			t.Fatal(err)
		}
		childRun := start(t, child, "child running")
		signal(t, childEntered)
		if err = parent.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err = wait(t, parentRun); !errors.Is(err, context.Canceled) {
			t.Fatalf("parent cancellation: %v", err)
		}
		close(childRelease)
		waitOK(t, childRun)
	})
}

func TestConfigurationAndCompactionForkBoundaries(t *testing.T) {
	eachStore(t, func(t *testing.T, st astral.Store) {
		ctx := context.Background()
		m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
			return answer("answer or summary"), nil
		})
		c := testConfig(m, st)
		c.Compaction.KeepRecentTokens = 1
		a := newSessions(t, c)
		parent := newSession(t, a, astral.SessionOptions{})
		waitOK(t, start(t, parent, "one"))
		waitOK(t, start(t, parent, "two"))
		configuration := parent.Configuration()
		configuration.SystemPrompt = "selected configuration"
		if err := parent.Configure(ctx, configuration); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"configuration", "compaction"} {
			if kind == "compaction" {
				if _, err := parent.Compact(ctx, ""); err != nil {
					t.Fatal(err)
				}
			}
			want, err := parent.InspectContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, mode := range []astral.ForkMode{astral.ForkShared, astral.ForkDetached} {
				child, err := parent.Fork(ctx, parent.Head(), astral.ForkOptions{ID: fmt.Sprintf("%s-%d", kind, mode), Mode: mode})
				if err != nil {
					t.Fatal(err)
				}
				got, err := child.InspectContext(ctx)
				if err != nil || !reflect.DeepEqual(got.Request, want.Request) {
					t.Fatalf("projection changed at %s: %v", kind, err)
				}
				before := len(m.observed())
				if _, err := child.Continue(ctx); !errors.Is(err, astral.ErrNotContinuable) {
					t.Fatalf("completed context boundary accepted: %v", err)
				}
				if len(m.observed()) != before {
					t.Fatalf("last final reply lost at %s", kind)
				}
			}
		}
	})
}

func TestDetachedCopiesLargeArchivedRecordWithSmallActiveContext(t *testing.T) {
	eachStore(t, func(t *testing.T, st astral.Store) {
		ctx := context.Background()
		header := storeHeader()
		header[1].Configuration.ModelID = "test"
		if err := appendStore(ctx, st, "session", 0, header); err != nil {
			t.Fatal(err)
		}
		large := astral.User(strings.Repeat("x", astral.DefaultPageBytes+1024))
		if err := appendStore(ctx, st, "session", 1, []astral.Entry{{ID: "large", ParentID: "config", Kind: astral.EntryMessage, Message: &large}, storeMessage("through", "large"), storeMessage("recent", "through")}); err != nil {
			t.Fatal(err)
		}
		if err := appendStore(ctx, st, "session", 2, []astral.Entry{{ID: "summary", ParentID: "recent", Kind: astral.EntryCompaction, Compaction: &astral.CompactionRecord{ThroughID: "through", Summary: "archived", ModelID: "test"}}}); err != nil {
			t.Fatal(err)
		}
		m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
			return answer("done"), nil
		})
		c := testConfig(m, st)
		c.MaxContextBytes = 4096
		a := newSessions(t, c)
		parent, err := a.OpenSession(ctx, "session")
		if err != nil {
			t.Fatal(err)
		}
		child, err := parent.Fork(ctx, parent.Head(), astral.ForkOptions{ID: "large-child", Mode: astral.ForkDetached})
		if err != nil {
			t.Fatal(err)
		}
		copied, ok := entry(child, "large")
		if !ok || !reflect.DeepEqual(copied.Message, &large) || copied.SessionID != "session" || copied.Version != 2 {
			t.Fatal("large ancestor was cropped or rewritten")
		}
		if child.Stats().ContextBytes > 8192 {
			t.Fatal("archived payload entered active context")
		}
	})
}
