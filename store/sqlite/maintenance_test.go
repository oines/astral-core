package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/oines/astral-core"
)

func TestReclaimShrinksOpenStoreAndPreservesSharedHistory(t *testing.T) {
	ctx := context.Background()
	st := payloadStore(t)
	shared := core.User(strings.Repeat("shared-body", 8000))
	payloadCommit(t, st, "s", []core.Entry{{ID: "shared", ParentID: "config", Kind: core.EntryMessage, Message: &shared}}, nil)
	child, err := st.Fork(ctx, core.ForkRequest{SourceID: "s", SourceRolloutID: "r", TargetID: "child", HeadID: "shared", Mode: core.ForkDetached})
	if err != nil {
		t.Fatal(err)
	}
	large := core.User(strings.Repeat("discard", 1<<19))
	payloadCommit(t, st, "s", []core.Entry{{ID: "large", ParentID: "shared", Kind: core.EntryMessage, Message: &large}}, nil)
	base, err := core.ReadContext(ctx, st, child.RolloutID, child.Branch.HeadID, core.DefaultContextBytes)
	if err != nil {
		t.Fatal(err)
	}
	input, err := core.FreezeRequest(child.RolloutID, base, base.Request, core.ModelSpec{ID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	attempt := core.ModelAttempt{ID: "a", SessionID: "child", Version: child.Version + 1, Purpose: "compaction", Number: 1, StartedAt: time.Now().UTC(), Input: input}
	payloadCommit(t, st, "child", nil, []core.AttemptMutation{{ID: "a", Create: &attempt}})
	reply := core.Reply{Message: core.Message{Role: core.RoleAssistant, Content: core.Text(strings.Repeat("attempt", 2000))}}
	payloadCommit(t, st, "child", nil, []core.AttemptMutation{{ID: "a", Finish: &core.AttemptOutcome{Status: "completed", FinishedAt: time.Now().UTC(), Reply: &reply}}})
	queued := core.InputRecord{ID: "queued", Kind: core.InputFollowUp, Digest: strings.Repeat("a", 64), Message: &shared}
	receipt := payloadCommit(t, st, "child", []core.Entry{{ID: "queued", Kind: core.EntryInputQueued, Input: &queued}}, nil)

	stateBefore, err := st.LoadState(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	entryBefore, err := st.Get(ctx, child.RolloutID, "shared")
	if err != nil {
		t.Fatal(err)
	}
	attemptsBefore, err := st.ScanAttempts(ctx, "child", core.AttemptQuery{})
	if err != nil {
		t.Fatal(err)
	}
	pendingBefore, err := st.Pending(ctx, "child", core.PendingQuery{})
	if err != nil {
		t.Fatal(err)
	}
	inputBefore, err := st.LookupInput(ctx, "child", "queued")
	if err != nil {
		t.Fatal(err)
	}
	receiptBefore, err := st.LookupCommit(ctx, "child", receipt.CommitID)
	if err != nil {
		t.Fatal(err)
	}
	// Establish the checkpointed main file before deleting, rather than
	// mistaking an uncheckpointed WAL for a compacted database.
	if err = st.Reclaim(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(st.path)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.DeleteSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	payloads := payloadCount(t, st)
	if err = st.Reclaim(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(st.path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size()-after.Size() < 2<<20 {
		t.Fatalf("main file did not shrink: before=%d after=%d", before.Size(), after.Size())
	}
	t.Logf("main file shrank from %d to %d bytes", before.Size(), after.Size())
	wal, err := os.Stat(st.path + "-wal")
	if err != nil || wal.Size() != 0 {
		t.Fatalf("WAL was not truncated: %v %v", wal, err)
	}
	stateAfter, err := st.LoadState(ctx, "child")
	if err != nil || !reflect.DeepEqual(stateBefore, stateAfter) {
		t.Fatalf("state changed: %v", err)
	}
	entryAfter, err := st.Get(ctx, child.RolloutID, "shared")
	if err != nil || !reflect.DeepEqual(entryBefore, entryAfter) {
		t.Fatalf("shared history changed: %v", err)
	}
	attemptsAfter, err := st.ScanAttempts(ctx, "child", core.AttemptQuery{})
	if err != nil || !reflect.DeepEqual(attemptsBefore, attemptsAfter) {
		t.Fatalf("attempts changed: %v", err)
	}
	pendingAfter, err := st.Pending(ctx, "child", core.PendingQuery{})
	if err != nil || !reflect.DeepEqual(pendingBefore, pendingAfter) {
		t.Fatalf("pending input changed: %v", err)
	}
	inputAfter, err := st.LookupInput(ctx, "child", "queued")
	if err != nil || !reflect.DeepEqual(inputBefore, inputAfter) {
		t.Fatalf("input receipt changed: %v", err)
	}
	receiptAfter, err := st.LookupCommit(ctx, "child", receipt.CommitID)
	if err != nil || !reflect.DeepEqual(receiptBefore, receiptAfter) {
		t.Fatalf("commit receipt changed: %v", err)
	}
	if payloadCount(t, st) != payloads {
		t.Fatal("reclaim changed payload ownership")
	}
	var schema int64
	var autoVacuum int
	var integrity string
	if err = st.db.QueryRow(`PRAGMA user_version`).Scan(&schema); err != nil || schema != SchemaVersion {
		t.Fatalf("schema changed: %d %v", schema, err)
	}
	if err = st.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&autoVacuum); err != nil || autoVacuum != 0 {
		t.Fatalf("auto_vacuum changed: %d %v", autoVacuum, err)
	}
	if err = st.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity check: %q %v", integrity, err)
	}
	assertReclaimStoreUsable(t, st, "child")
}

func TestReclaimCancellationReleasesWaitingResources(t *testing.T) {
	for _, blocked := range []string{"writer", "reader"} {
		t.Run(blocked, func(t *testing.T) {
			st := payloadStore(t)
			var release func()
			if blocked == "writer" {
				st.write.Lock()
				release = st.write.Unlock
			} else {
				conn, err := st.readDB.Conn(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				release = func() { conn.Close() }
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- st.Reclaim(ctx) }()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("cancellation: %v", err)
				}
			case <-time.After(2 * time.Second):
				release()
				t.Fatal("reclaim did not honor cancellation")
			}
			release()
			if n := st.readDB.Stats().InUse; n != 0 {
				t.Fatalf("reclaim leaked %d reader connections", n)
			}
			assertReclaimStoreUsable(t, st, "s")
			if err := st.Reclaim(context.Background()); err != nil {
				t.Fatalf("retry after cancellation: %v", err)
			}
		})
	}
}

func TestReclaimExternalTransactionsAreRetryable(t *testing.T) {
	for _, blocked := range []string{"reader", "writer"} {
		t.Run(blocked, func(t *testing.T) {
			ctx := context.Background()
			st := payloadStore(t)
			if err := st.Reclaim(ctx); err != nil {
				t.Fatal(err)
			}
			other, err := Open(st.path)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			var tx *sql.Tx
			if blocked == "reader" {
				tx, err = other.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
				if err == nil {
					var count int
					err = tx.QueryRow(`SELECT count(*) FROM entries`).Scan(&count)
				}
				if err == nil {
					message := core.User("WAL newer than external reader")
					payloadCommit(t, st, "s", []core.Entry{{ID: "newer", ParentID: "config", Kind: core.EntryMessage, Message: &message}}, nil)
				}
			} else {
				tx, err = other.db.BeginTx(ctx, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			started := time.Now()
			if err = st.Reclaim(ctx); !errors.Is(err, core.ErrBusy) {
				t.Fatalf("external %s should be retryable: %v", blocked, err)
			}
			if time.Since(started) > time.Second {
				t.Fatal("external busy transaction did not fail promptly")
			}
			if _, err = st.Get(ctx, "r", "config"); err != nil {
				t.Fatalf("read after failed reclaim: %v", err)
			}
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			assertReclaimStoreUsable(t, st, "s")
			if err = st.Reclaim(ctx); err != nil {
				t.Fatalf("retry after external %s ended: %v", blocked, err)
			}
		})
	}
}

func TestReclaimConcurrentReadersWritersAndReclaimers(t *testing.T) {
	st := payloadStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for range 3 {
		wg.Go(func() {
			for range 20 {
				if _, err := st.Get(ctx, "r", "config"); err != nil {
					errs <- err
					return
				}
				if _, err := st.Scan(ctx, "r", core.Query{}); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	for range 2 {
		wg.Go(func() {
			for range 5 {
				if err := st.Reclaim(ctx); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Go(func() {
		for range 20 {
			state, err := st.LoadState(ctx, "s")
			if err != nil {
				errs <- err
				return
			}
			if err = reclaimTestCommit(ctx, st, "s", state); err != nil {
				errs <- err
				return
			}
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	state, err := st.LoadState(ctx, "s")
	if err != nil || state.Version != 21 {
		t.Fatalf("lost concurrent writes: version=%d error=%v", state.Version, err)
	}
}

func TestReclaimDuringDetachedFork(t *testing.T) {
	st := payloadStore(t)
	message := core.User(strings.Repeat("fork shared", 1000))
	payloadCommit(t, st, "s", []core.Entry{{ID: "message", ParentID: "config", Kind: core.EntryMessage, Message: &message}}, nil)
	request := core.ForkRequest{SourceID: "s", SourceRolloutID: "r", TargetID: "child", HeadID: "message", Mode: core.ForkDetached}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	var once sync.Once
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		_, err := st.fork(ctx, request, func() { once.Do(func() { close(entered); <-release }) })
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("fork did not reach a page boundary")
	}
	err := st.Reclaim(ctx)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	record, err := st.Get(ctx, request.RolloutID(), "message")
	if err != nil || !reflect.DeepEqual(record.Entry.Message, &message) {
		t.Fatalf("reclaim damaged an unpublished fork: %v", err)
	}
}

func TestReclaimConcurrentCloseDoesNotDeadlock(t *testing.T) {
	st := payloadStore(t)
	conn, err := st.readDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reclaimed, closed := make(chan error, 1), make(chan error, 1)
	go func() { reclaimed <- st.Reclaim(ctx) }()
	for st.readDB.Stats().InUse < maxReadConnections && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	go func() { closed <- st.Close() }()
	conn.Close()
	select {
	case <-reclaimed:
	case <-ctx.Done():
		t.Fatal("reclaim deadlocked with Close")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Close deadlocked with reclaim")
	}
}

func assertReclaimStoreUsable(t *testing.T, st *Store, id string) {
	t.Helper()
	state, err := st.LoadState(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Get(context.Background(), state.RolloutID, state.Branch.HeadID); err != nil {
		t.Fatalf("read after reclaim: %v", err)
	}
	if err = reclaimTestCommit(context.Background(), st, id, state); err != nil {
		t.Fatalf("write after reclaim: %v", err)
	}
}

func reclaimTestCommit(ctx context.Context, st *Store, id string, state core.RuntimeState) error {
	key := fmt.Sprintf("after-reclaim-%d", state.Version)
	message := core.User(key)
	_, err := st.Commit(ctx, id, core.Commit{ID: key, ExpectedVersion: state.Version, Entries: []core.Entry{{ID: key, SessionID: id, ParentID: state.Branch.HeadID, Version: state.Version + 1, Kind: core.EntryMessage, Message: &message}}})
	return err
}
