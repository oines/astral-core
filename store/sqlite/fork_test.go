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
		if strings.Contains(detail, "SCAN e") || strings.Contains(detail, "SEARCH e ") && !strings.Contains(detail, "session_id=? AND id=?") {
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
	entries := []s.Entry{{ID: "created", Sequence: 1, Version: 1, Kind: s.EntryCreated, Created: &s.CreationRecord{FormatVersion: s.FormatVersion, SessionID: "parent"}}, {ID: "config", Sequence: 2, Version: 1, Kind: s.EntryConfiguration, Configuration: &s.Configuration{ModelID: "test"}}}
	parent := "config"
	for i := 0; i < 1024; i++ {
		id := fmt.Sprintf("message-%d", i)
		msg := astral.User("history")
		entries = append(entries, s.Entry{ID: id, ParentID: parent, Version: 1, Sequence: uint64(i + 3), Kind: s.EntryMessage, Message: &msg})
		parent = id
	}
	if _, err := st.Commit(context.Background(), "parent", s.Commit{ID: "seed", Entries: entries}); err != nil {
		t.Fatal(err)
	}
	state, err := st.LoadState(context.Background(), "parent")
	if err != nil {
		t.Fatal(err)
	}
	r := s.ForkRequest{SourceID: "parent", TargetID: "child", SourceVersion: 1, HeadID: parent, Creation: s.Entry{ID: "child-created", Sequence: 1, Version: 1, Kind: s.EntryCreated, Created: &s.CreationRecord{FormatVersion: s.FormatVersion, SessionID: "child", ForkSessionID: "parent", ForkEntryID: parent}}}
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
	if _, err = st.Get(context.Background(), "child", request.HeadID); !errors.Is(err, s.ErrNotFound) {
		t.Fatalf("unpublished entry visible: %v", err)
	}
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer writeCancel()
	msg := astral.User("parent continued")
	_, err = st.Commit(writeCtx, "parent", s.Commit{ID: "parent-continues", ExpectedVersion: state.Version, Entries: []s.Entry{{ID: "continued", ParentID: state.Branch.HeadID, Sequence: state.Sequence + 1, Version: state.Version + 1, Kind: s.EntryMessage, Message: &msg}}})
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
	if err = st.db.QueryRow("SELECT COUNT(*) FROM entries WHERE session_id='child'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("cancelled target not cleaned: %d %v", n, err)
	}
	if _, err = st.Fork(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Get(context.Background(), "child", "continued"); !errors.Is(err, s.ErrNotFound) {
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
