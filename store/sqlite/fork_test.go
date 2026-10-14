package sqlite

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	astral "github.com/oines/astral-core"
	s "github.com/oines/astral-core/session"
)

func TestHistoryPathPlanUsesEntryPointLookups(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, request := fixture(t, st)
	rows, err := st.db.Query("EXPLAIN QUERY PLAN "+historyPathSQL, "parent", request.HeadID, "parent", 257, "", "parent")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	lookups := 0
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		t.Log(detail)
		if strings.Contains(detail, "SCAN e") || strings.Contains(detail, "SEARCH e ") && !strings.Contains(detail, "rollout_id=? AND id=?") {
			t.Fatalf("unbounded entry scan in page query: %s", detail)
		}
		if strings.Contains(detail, "SEARCH e ") {
			lookups++
		}
	}
	if err = rows.Err(); err != nil || lookups != 3 {
		t.Fatalf("entry lookups=%d, err=%v", lookups, err)
	}
}

func fixture(t *testing.T, st *Store) (s.RuntimeState, s.ForkRequest) {
	t.Helper()
	entries := []s.Entry{{ID: "created", SessionID: "parent", Version: 1, Kind: s.EntryCreated, Created: &s.CreationRecord{FormatVersion: s.FormatVersion, SessionID: "parent", RolloutID: "parent"}}, {ID: "config", SessionID: "parent", Version: 1, Kind: s.EntryConfiguration, Configuration: &s.Configuration{ModelID: "test"}}}
	parent := "config"
	for i := 0; i < 1024; i++ {
		id := fmt.Sprintf("message-%d", i)
		msg := astral.User("history")
		entries = append(entries, s.Entry{ID: id, ParentID: parent, Version: 1, SessionID: "parent", Kind: s.EntryMessage, Message: &msg})
		parent = id
	}
	if _, err := st.Commit(context.Background(), "parent", s.Commit{ID: "seed", Entries: entries}); err != nil {
		t.Fatal(err)
	}
	state, err := st.LoadState(context.Background(), "parent")
	if err != nil {
		t.Fatal(err)
	}
	r := s.ForkRequest{SourceID: "parent", SourceRolloutID: "parent", TargetID: "child", HeadID: parent, Mode: s.ForkDetached}
	return state, r
}

func TestForkPagesYieldWriterAndCancelAtomically(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	state, request := fixture(t, st)
	entered, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	var once sync.Once
	go func() {
		_, err := st.fork(ctx, request, func() { once.Do(func() { close(entered); <-release }) })
		finished <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("fork did not reach its first page")
	}
	if _, err = st.LoadState(context.Background(), "child"); !errors.Is(err, s.ErrNotFound) {
		t.Fatalf("unpublished child visible: %v", err)
	}
	if _, err = st.Get(context.Background(), request.RolloutID(), request.HeadID); !errors.Is(err, s.ErrNotFound) {
		t.Fatalf("unpublished entry visible: %v", err)
	}
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer writeCancel()
	msg := astral.User("parent continued")
	_, err = st.Commit(writeCtx, "parent", s.Commit{ID: "parent-continues", ExpectedVersion: state.Version, Entries: []s.Entry{{ID: "continued", ParentID: state.Branch.HeadID, SessionID: "parent", Version: state.Version + 1, Kind: s.EntryMessage, Message: &msg}}})
	if err != nil {
		t.Fatalf("fork held writer lock: %v", err)
	}
	cancel()
	close(release)
	if err = <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if _, err = st.LoadState(context.Background(), "child"); !errors.Is(err, s.ErrNotFound) {
		t.Fatalf("cancelled child visible: %v", err)
	}
	var n int
	if err = st.db.QueryRow("SELECT COUNT(*) FROM entries WHERE rollout_id=?", request.RolloutID()).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cancelled target not cleaned: %d %v", n, err)
	}
	if _, err = st.Fork(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Get(context.Background(), request.RolloutID(), "continued"); !errors.Is(err, s.ErrNotFound) {
		t.Fatal("parent advance leaked into fixed fork boundary")
	}
}

func TestForkCrashWorker(t *testing.T) {
	path := os.Getenv("ASTRAL_FORK_CRASH_DB")
	if path == "" {
		t.Skip("subprocess helper")
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, request := fixture(t, st)
	_, err = st.fork(context.Background(), request, func() { fmt.Println("FORK-BARRIER"); select {} })
	t.Fatalf("worker did not stop at barrier: %v", err)
}

func TestCrashDuringForkNeverPublishesPartialSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestForkCrashWorker$", "-test.v")
	cmd.Env = append(os.Environ(), "ASTRAL_FORK_CRASH_DB="+path)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reached := false
	scanner := bufio.NewScanner(pipe)
	for scanner.Scan() {
		if scanner.Text() == "FORK-BARRIER" {
			reached = true
			break
		}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if !reached {
		t.Fatal("fork crash barrier was not reached")
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.LoadState(ctx, "parent"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.LoadState(ctx, "child"); !errors.Is(err, s.ErrNotFound) {
		t.Fatalf("partial fork exposed after process death: %v", err)
	}
	if err = st.DiscardUnpublished(ctx, "child"); err != nil {
		t.Fatal(err)
	}
	if err = st.DiscardUnpublished(ctx, "parent"); !errors.Is(err, s.ErrConflict) {
		t.Fatal("cleanup accepted published session")
	}
}

func TestRolloutHistoryFiltersUseOrderedIndexes(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "indexed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fixture(t, st)
	for _, check := range []struct {
		where, index string
		args         []any
	}{
		{"e.kind=?", "entry_kind", []any{"parent", string(s.EntryMessage), 2}},
		{"e.projection IS NOT NULL", "entry_history", []any{"parent", 2}},
		{"e.parent_id=? AND e.projection IS NOT NULL", "entry_parent", []any{"parent", "config", 2}},
		{"e.session_id=?", "entry_writer", []any{"parent", "parent", 2}},
	} {
		rows, err := st.db.Query("EXPLAIN QUERY PLAN SELECT e.payload,e.projection FROM entries e JOIN rollouts r ON r.id=e.rollout_id WHERE e.rollout_id=? AND r.status='active' AND "+check.where+" ORDER BY e.seq LIMIT ?", check.args...)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			t.Log(detail)
			if strings.Contains(detail, "USE TEMP") || strings.Contains(detail, "SCAN e") {
				t.Fatalf("unbounded sort/scan: %s", detail)
			}
			found = found || strings.Contains(detail, check.index)
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !found {
			t.Fatalf("missing %s index: %v", check.index, err)
		}
	}
}

func TestConcurrentHandlesDoNotDiscardActiveDetachedCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	alias := filepath.Join(t.TempDir(), "alias.db")
	if err = os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	second, err := Open(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_, request := fixture(t, first)
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	var once sync.Once
	go func() {
		_, err := first.fork(context.Background(), request, func() { once.Do(func() { close(entered); <-release }) })
		finished <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err = second.Fork(ctx, request); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("concurrent retry did not wait: %v", err)
	}
	close(release)
	if err = <-finished; err != nil {
		t.Fatal(err)
	}
	state, err := second.Fork(context.Background(), request)
	if err != nil || state.Version != 1 {
		t.Fatalf("same target not preserved: %v", err)
	}
}
