// Package sqlite supplies the indexed long-session Store. Writes use SQLite
// transactions in WAL mode with synchronous=FULL; no CGO or service is required.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	core "github.com/oines/astral-core"

	"github.com/oines/astral-core/store/internal/historypage"
	"github.com/oines/astral-core/store/internal/pending"
	"github.com/oines/astral-core/store/internal/targetlock"
	_ "modernc.org/sqlite"
)

type Store struct {
	path    string
	db      *sql.DB
	readDB  *sql.DB
	write   sync.Mutex
	rows    atomic.Uint64
	commits atomic.Uint64
}
type Statistics struct {
	RowsRead uint64
	Commits  uint64
}

func (st *Store) Stats() Statistics {
	return Statistics{RowsRead: st.rows.Load(), Commits: st.commits.Load()}
}
func (st *Store) Close() error { return errors.Join(st.readDB.Close(), st.db.Close()) }

var schema = fmt.Sprintf(`
CREATE TABLE format(version INTEGER NOT NULL);
INSERT INTO format VALUES(%d);
CREATE TABLE rollouts(id TEXT PRIMARY KEY,status TEXT NOT NULL,seq INTEGER NOT NULL);
CREATE TABLE sessions(id TEXT PRIMARY KEY,rollout_id TEXT NOT NULL,ready INTEGER NOT NULL,state BLOB NOT NULL,fork_digest TEXT NOT NULL DEFAULT '');
CREATE INDEX session_rollout ON sessions(rollout_id,id);
CREATE TABLE entries(rollout_id TEXT NOT NULL,session_id TEXT NOT NULL,id TEXT NOT NULL,seq INTEGER NOT NULL,version INTEGER NOT NULL,parent_id TEXT NOT NULL,kind TEXT NOT NULL,reference TEXT NOT NULL,payload BLOB NOT NULL,projection BLOB,payload_size INTEGER NOT NULL,projection_size INTEGER NOT NULL,PRIMARY KEY(rollout_id,id),UNIQUE(rollout_id,seq));
CREATE INDEX entry_parent ON entries(rollout_id,parent_id,seq) WHERE projection IS NOT NULL;
CREATE INDEX entry_history ON entries(rollout_id,seq) WHERE projection IS NOT NULL;
CREATE INDEX entry_kind ON entries(rollout_id,kind,seq);
CREATE INDEX entry_reference ON entries(rollout_id,kind,reference,seq);
CREATE INDEX entry_writer ON entries(rollout_id,session_id,seq);
CREATE TABLE receipts(rollout_id TEXT NOT NULL,session_id TEXT NOT NULL,commit_id TEXT NOT NULL,version INTEGER NOT NULL,digest TEXT NOT NULL,first_seq INTEGER NOT NULL,last_seq INTEGER NOT NULL,PRIMARY KEY(session_id,commit_id));
CREATE INDEX receipt_rollout ON receipts(rollout_id);
CREATE TABLE pending(rollout_id TEXT NOT NULL,session_id TEXT NOT NULL,kind TEXT NOT NULL,input_kind TEXT NOT NULL,key TEXT NOT NULL,owner TEXT NOT NULL,seq INTEGER NOT NULL,entry_id TEXT NOT NULL,PRIMARY KEY(session_id,kind,key));
CREATE INDEX pending_order ON pending(session_id,kind,input_kind,seq);
CREATE INDEX pending_owner ON pending(session_id,kind,owner);
CREATE INDEX pending_rollout ON pending(rollout_id);
CREATE TABLE inputs(session_id TEXT NOT NULL,id TEXT NOT NULL,kind TEXT NOT NULL,digest TEXT NOT NULL,accepted_at TEXT NOT NULL,current_queue_entry_id TEXT NOT NULL,state TEXT NOT NULL,entry_id TEXT NOT NULL DEFAULT '',PRIMARY KEY(session_id,id));
CREATE INDEX input_status ON inputs(session_id,kind,state);
CREATE TABLE attempts(session_id TEXT NOT NULL,id TEXT NOT NULL,rollout_id TEXT NOT NULL,version INTEGER NOT NULL,purpose TEXT NOT NULL,payload BLOB NOT NULL,payload_size INTEGER NOT NULL,PRIMARY KEY(session_id,id),UNIQUE(session_id,version));
CREATE INDEX attempt_purpose ON attempts(session_id,purpose,version);
CREATE INDEX attempt_rollout ON attempts(rollout_id);

%s
PRAGMA user_version=%d;
`, core.FormatVersion, payloadSchema12, SchemaVersion)

// CROSS JOIN keeps the bounded chain on the outside of each indexed lookup.
// Without it SQLite can scan every entry in the session before joining a page.
const historyPathSQL = `WITH RECURSIVE chain(id,parent_id,seq,depth) AS (
 SELECT e.id,e.parent_id,e.seq,0 FROM entries e JOIN rollouts r ON r.id=e.rollout_id WHERE e.rollout_id=? AND e.id=? AND r.status='active'
 UNION ALL SELECT e.id,e.parent_id,e.seq,c.depth+1 FROM chain c CROSS JOIN entries e ON e.id=c.parent_id WHERE e.rollout_id=? AND c.depth+1<? AND c.id<>?
 ) SELECT e.payload,e.projection,e.payload_size,e.projection_size,e.id,e.seq FROM chain c CROSS JOIN entries e ON e.rollout_id=? AND e.id=c.id ORDER BY c.depth`

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, core.ErrInvalid
	}
	abs, err := databasePath(path)
	if err != nil {
		return nil, err
	}
	existing := false
	if f, openErr := os.Open(abs); openErr == nil {
		var head [16]byte
		n, readErr := io.ReadFull(f, head[:])
		f.Close()
		if readErr == nil && string(head[:]) == "SQLite format 3\x00" {
			existing = true
		} else if n != 0 {
			return nil, core.ErrUnsupportedFormat
		}
	} else if !errors.Is(openErr, os.ErrNotExist) {
		return nil, openErr
	}
	uri := databaseURI(abs, runtime.GOOS)
	// Inspect an existing database before any schema or journal mutation.
	if existing {
		check, err := sql.Open("sqlite", uri.String()+"?mode=ro")
		if err != nil {
			return nil, err
		}
		status, checkErr := inspectMigration(context.Background(), check, currentMigrationPlan())
		check.Close()
		if checkErr != nil || status.NeedsMigration || status.SchemaCurrent != SchemaVersion {
			return nil, core.ErrUnsupportedFormat
		}
	}
	values := url.Values{}
	values.Set("_txlock", "immediate")
	for _, p := range []string{"journal_mode(WAL)", "synchronous(FULL)", "busy_timeout(5000)", "temp_store(FILE)", "cache_size(-2048)", "foreign_keys(ON)"} {
		values.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", uri.String()+"?"+values.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	st := &Store{db: db, path: abs}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if !existing {
		tx, err := db.BeginTx(ctx, nil)
		if err == nil {
			_, err = tx.ExecContext(ctx, schema)
			if err == nil {
				err = initializeMigrationHistory(ctx, db, tx, currentMigrationPlan())
			}
			if err == nil {
				err = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
		}
		if err != nil {
			db.Close()
			return nil, err
		}
	}
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	readValues := url.Values{"mode": {"ro"}, "_txlock": {"deferred"}, "_pragma": {"busy_timeout(5000)", "cache_size(-2048)"}}
	st.readDB, err = sql.Open("sqlite", uri.String()+"?"+readValues.Encode())
	if err != nil {
		db.Close()
		return nil, err
	}
	st.readDB.SetMaxOpenConns(4)
	st.readDB.SetMaxIdleConns(4)
	return st, nil
}
func decodeState(raw []byte, id string) (core.RuntimeState, error) {
	var state core.RuntimeState
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, core.ErrCorrupt
	}
	if !core.SupportsFormat(state.FormatVersion) {
		return state, core.ErrUnsupportedFormat
	}
	if state.SessionID != id || state.RolloutID == "" || state.Version == 0 {
		return state, core.ErrCorrupt
	}
	return state, nil
}
func decodeRecord(payload, projection []byte) (core.Record, error) {
	var r core.Record
	if json.Unmarshal(payload, &r.Entry) != nil {
		return r, core.ErrCorrupt
	}
	if len(projection) > 0 {
		if json.Unmarshal(projection, &r.Projection) != nil {
			return r, core.ErrCorrupt
		}
	}
	return r, nil
}
func conflict(err error) error {
	var code interface{ Code() int }
	if errors.As(err, &code) && code.Code()&255 == 19 {
		return fmt.Errorf("%w: %v", core.ErrConflict, err)
	}
	return err
}
func (st *Store) activeRollout(ctx context.Context, id string) error {
	var one int
	err := st.db.QueryRowContext(ctx, "SELECT 1 FROM rollouts WHERE id=? AND status='active'", id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ErrNotFound
	}
	return err
}
func (st *Store) Commit(ctx context.Context, id string, c core.Commit) (receipt core.Receipt, err error) {
	if id == "" || c.ID == core.ForkCommitID || len(c.Entries)+len(c.Attempts) == 0 {
		return receipt, core.ErrInvalid
	}
	digest, err := c.Digest()
	if err != nil {
		return receipt, err
	}
	st.write.Lock()
	defer st.write.Unlock()
	defer func() { err = conflict(err) }()
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback()
	var raw []byte
	var rolloutID string
	var ready int
	state := core.RuntimeState{}
	err = tx.QueryRowContext(ctx, "SELECT rollout_id,ready,state FROM sessions WHERE id=?", id).Scan(&rolloutID, &ready, &raw)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return receipt, err
	}
	if exists {
		if ready != 1 {
			return receipt, core.ErrConflict
		}
		state, err = readState(ctx, tx, raw, id)
		if err != nil {
			return receipt, err
		}
		st.rows.Add(1)
	} else {
		if len(c.Entries) == 0 {
			return receipt, core.ErrCorrupt
		}
		created := c.Entries[0].Created
		if created == nil || created.RolloutID == "" || created.Projection != nil {
			return receipt, core.ErrCorrupt
		}
		rolloutID = created.RolloutID
	}
	var sequence uint64
	var status string
	err = tx.QueryRowContext(ctx, "SELECT status,seq FROM rollouts WHERE id=?", rolloutID).Scan(&status, &sequence)
	if exists {
		if err != nil || status != "active" {
			return receipt, core.ErrNotFound
		}
	} else {
		if err == nil {
			return receipt, core.ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return receipt, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO rollouts(id,status,seq) VALUES(?,'active',0)", rolloutID); err != nil {
			return receipt, err
		}
	}
	old := core.Receipt{CommitID: c.ID}
	err = tx.QueryRowContext(ctx, "SELECT version,digest,first_seq,last_seq FROM receipts WHERE session_id=? AND commit_id=?", id, c.ID).Scan(&old.Version, &old.Digest, &old.FirstSequence, &old.LastSequence)
	if err == nil {
		if old.Digest != digest {
			return receipt, core.ErrConflict
		}
		old.CurrentVersion = state.Version
		return old, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return receipt, err
	}
	if state.Version != c.ExpectedVersion {
		return receipt, core.ErrConflict
	}
	confirmed, receipt, err := core.AssignCommit(c, sequence+1)
	if err != nil {
		return receipt, err
	}
	next, records, err := core.Reduce(state, confirmed)
	if err != nil {
		return receipt, err
	}
	if next.SessionID != id || next.RolloutID != rolloutID {
		return receipt, core.ErrCorrupt
	}
	for _, e := range confirmed.Entries {
		var one int
		check := tx.QueryRowContext(ctx, "SELECT 1 FROM entries WHERE rollout_id=? AND id=?", rolloutID, e.ID).Scan(&one)
		if check == nil {
			return receipt, core.ErrCorrupt
		}
		if !errors.Is(check, sql.ErrNoRows) {
			return receipt, check
		}
		if e.Compaction != nil {
			if err = tx.QueryRowContext(ctx, "SELECT 1 FROM entries WHERE rollout_id=? AND id=? AND projection IS NOT NULL", rolloutID, e.Compaction.ThroughID).Scan(&one); err != nil {
				return receipt, core.ErrCorrupt
			}
		}
	}
	if err := pending.Validate(confirmed.Entries, func(key string) (core.InputKind, bool, error) {
		var kind string
		err := tx.QueryRowContext(ctx, "SELECT input_kind FROM pending WHERE session_id=? AND kind=? AND key=?", id, string(core.EntryInputQueued), key).Scan(&kind)
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return core.InputKind(kind), err == nil, err
	}); err != nil {
		return receipt, err
	}

	if err = payloadCandidates(ctx, tx); err != nil {
		return receipt, err
	}
	raw, _, err = encodeValue(ctx, tx, rolloutID, "state", ownerKey(id), next, state12)
	if err != nil {
		return receipt, err
	}
	if !exists {
		_, err = tx.ExecContext(ctx, "INSERT INTO sessions(id,rollout_id,ready,state) VALUES(?,?,1,?)", id, rolloutID, raw)
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE sessions SET state=? WHERE id=?", raw, id)
	}
	if err != nil {
		return receipt, err
	}
	if err = insert(ctx, tx, rolloutID, records, true); err != nil {
		return receipt, err
	}
	if err = saveAttempts(ctx, tx, id, rolloutID, confirmed.Attempts, confirmed.Entries); err != nil {
		return receipt, err
	}
	if err = saveReceipt(ctx, tx, rolloutID, id, receipt); err != nil {
		return receipt, err
	}
	if receipt.LastSequence > 0 {
		if _, err = tx.ExecContext(ctx, "UPDATE rollouts SET seq=? WHERE id=?", receipt.LastSequence, rolloutID); err != nil {
			return receipt, err
		}
	}
	if err = collectPayloads(ctx, tx); err != nil {
		return receipt, err
	}
	err = tx.Commit()
	if err == nil {
		st.commits.Add(1)
	}
	return receipt, err
}
func saveReceipt(ctx context.Context, tx *sql.Tx, rolloutID, id string, r core.Receipt) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO receipts(rollout_id,session_id,commit_id,version,digest,first_seq,last_seq) VALUES(?,?,?,?,?,?,?)", rolloutID, id, r.CommitID, r.Version, r.Digest, r.FirstSequence, r.LastSequence)
	return err
}
func insert(ctx context.Context, tx *sql.Tx, id string, records []core.Record, pending bool) error {
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO entries(rollout_id,session_id,id,seq,version,parent_id,kind,reference,payload,projection,payload_size,projection_size) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range records {
		e := r.Entry
		logical, err := json.Marshal(e)
		if err != nil {
			return err
		}
		payload, blobs, err := encode12(logical, entry12)
		if err != nil {
			return err
		}
		var projection any
		projectionSize := 0
		if r.Projection != nil {
			b, err := json.Marshal(r.Projection)
			if err != nil {
				return err
			}
			projectionSize = len(b)
			encoded, more, err := encode12(b, projection12)
			if err != nil {
				return err
			}
			for hash, value := range more {
				blobs[hash] = value
			}
			projection = encoded
		}
		if err = replacePayloadRefs(ctx, tx, id, "entry", ownerKey(id, e.ID), blobs); err != nil {
			return err
		}
		if _, err = stmt.ExecContext(ctx, id, e.SessionID, e.ID, e.Sequence, e.Version, e.ParentID, string(e.Kind), core.Reference(e), payload, projection, len(logical), projectionSize); err != nil {
			return err
		}
		if !pending {
			continue
		}
		if e.InputID != "" && (e.Kind == core.EntryMessage || e.Kind == core.EntryContext) {
			if _, err = tx.ExecContext(ctx, "UPDATE inputs SET entry_id=? WHERE session_id=? AND id=? AND (entry_id='' OR ?)", e.ID, e.SessionID, e.InputID, e.Kind == core.EntryMessage); err != nil {
				return err
			}
		}
		switch e.Kind {
		case core.EntryInputQueued, core.EntryInputRequeued:
			if e.Kind == core.EntryInputRequeued {
				_, err = tx.ExecContext(ctx, "UPDATE inputs SET kind=?,current_queue_entry_id=?,state=? WHERE session_id=? AND id=?", string(e.Input.Kind), e.ID, string(core.InputPending), e.SessionID, e.Input.ID)
			} else {
				_, err = tx.ExecContext(ctx, "INSERT INTO inputs(session_id,id,kind,digest,accepted_at,current_queue_entry_id,state) VALUES(?,?,?,?,?,?,?)", e.SessionID, e.Input.ID, string(e.Input.Kind), e.Input.Digest, e.Time.Format(time.RFC3339Nano), e.ID, string(core.InputPending))
			}
			if err != nil {
				return err
			}
			fallthrough
		case core.EntryApprovalRequested:
			kind, key, owner := "", core.Reference(e), ""
			if e.Input != nil {
				kind = string(e.Input.Kind)
			}
			if e.Approval != nil {
				owner = e.Approval.ExecutionID
			}
			pendingKind := e.Kind
			if pendingKind == core.EntryInputRequeued {
				pendingKind = core.EntryInputQueued
			}
			_, err = tx.ExecContext(ctx, "INSERT INTO pending(rollout_id,session_id,kind,input_kind,key,owner,seq,entry_id) VALUES(?,?,?,?,?,?,?,?)", id, e.SessionID, string(pendingKind), kind, key, owner, e.Sequence, e.ID)
		case core.EntryInputCleared:
			if e.Input != nil {
				if _, err = tx.ExecContext(ctx, "UPDATE inputs SET state=? WHERE session_id=? AND id=?", string(core.InputCleared), e.SessionID, e.Input.ID); err != nil {
					return err
				}
				_, err = tx.ExecContext(ctx, "DELETE FROM pending WHERE session_id=? AND kind=? AND key=?", e.SessionID, string(core.EntryInputQueued), e.Input.ID)
				break
			}
			for _, kind := range e.Clear.Kinds {
				if _, err = tx.ExecContext(ctx, "UPDATE inputs SET state=? WHERE session_id=? AND kind=? AND state=?", string(core.InputCleared), e.SessionID, string(kind), string(core.InputPending)); err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, "DELETE FROM pending WHERE session_id=? AND kind=? AND input_kind=?", e.SessionID, string(core.EntryInputQueued), string(kind)); err != nil {
					return err
				}
			}
		case core.EntryInputConsumed:
			if _, err = tx.ExecContext(ctx, "UPDATE inputs SET state=? WHERE session_id=? AND id=?", string(core.InputApplied), e.SessionID, e.Input.ID); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, "DELETE FROM pending WHERE session_id=? AND kind=? AND key=?", e.SessionID, string(core.EntryInputQueued), e.Input.ID)
		case core.EntryApprovalDecided:
			_, err = tx.ExecContext(ctx, "DELETE FROM pending WHERE session_id=? AND kind=? AND key=?", e.SessionID, string(core.EntryApprovalRequested), e.Approval.ID)
		case core.EntryToolResult, core.EntryToolResolution, core.EntryToolControl, core.EntryToolUnresolved:
			_, err = tx.ExecContext(ctx, "DELETE FROM pending WHERE session_id=? AND kind=? AND owner=?", e.SessionID, string(core.EntryApprovalRequested), e.Tool.Invocation.ExecutionID)
		case core.EntryMessage:
			_, err = tx.ExecContext(ctx, "DELETE FROM pending WHERE session_id=? AND kind=?", e.SessionID, string(core.EntryApprovalRequested))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

var forkLocks targetlock.Map

func (st *Store) lockFork(ctx context.Context, id string) (func(), error) {
	return forkLocks.Lock(ctx, st.path+"\x00"+id)
}
func (st *Store) existingFork(ctx context.Context, r core.ForkRequest) (core.RuntimeState, bool, error) {
	tx, err := st.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return core.RuntimeState{}, false, err
	}
	defer tx.Rollback()
	var ready int
	var raw []byte
	var digest, rolloutID string
	err = tx.QueryRowContext(ctx, "SELECT ready,state,fork_digest,rollout_id FROM sessions WHERE id=?", r.TargetID).Scan(&ready, &raw, &digest, &rolloutID)
	if errors.Is(err, sql.ErrNoRows) {
		return core.RuntimeState{}, false, nil
	}
	if err != nil {
		return core.RuntimeState{}, false, err
	}
	want, err := r.Digest()
	if err != nil {
		return core.RuntimeState{}, false, err
	}
	if digest != want {
		return core.RuntimeState{}, false, core.ErrConflict
	}
	if ready == 0 {
		return core.RuntimeState{}, false, nil
	}
	if err = st.activeRollout(ctx, rolloutID); err != nil {
		return core.RuntimeState{}, true, err
	}
	state, err := readState(ctx, tx, raw, r.TargetID)
	return state, true, err
}
func (st *Store) writeForkPage(ctx context.Context, rolloutID string, records []core.Record) error {
	st.write.Lock()
	defer st.write.Unlock()
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err = tx.QueryRowContext(ctx, "SELECT status FROM rollouts WHERE id=?", rolloutID).Scan(&status); err != nil || status != "building" {
		return core.ErrNotFound
	}
	if err = payloadCandidates(ctx, tx); err != nil {
		return err
	}
	if err = insert(ctx, tx, rolloutID, records, false); err != nil {
		return err
	}
	return tx.Commit()
}
func (st *Store) Fork(ctx context.Context, r core.ForkRequest) (core.RuntimeState, error) {
	return st.fork(ctx, r, nil)
}
func (st *Store) fork(ctx context.Context, r core.ForkRequest, afterPage func()) (core.RuntimeState, error) {
	return st.forkWithCheckpoint(ctx, r, func(phase string) {
		if phase == "page" && afterPage != nil {
			afterPage()
		}
	})
}

// The private checkpoint observes durable boundaries in real process-crash tests.
func (st *Store) forkWithCheckpoint(ctx context.Context, r core.ForkRequest, checkpoint func(string)) (state core.RuntimeState, err error) {
	if err = core.ValidateFork(r); err != nil {
		return state, err
	}
	unlock, err := st.lockFork(ctx, r.TargetID)
	if err != nil {
		return state, err
	}
	defer unlock()
	var found bool
	if state, found, err = st.existingFork(ctx, r); found || err != nil {
		return state, err
	}
	source, err := st.LoadState(ctx, r.SourceID)
	if err != nil {
		return state, err
	}
	if source.RolloutID != r.SourceRolloutID {
		return state, core.ErrNotFound
	}
	head, err := st.Get(ctx, r.SourceRolloutID, r.HeadID)
	if err != nil {
		return state, err
	}
	state, err = core.ForkState(r, head)
	if err != nil {
		return state, err
	}
	if r.Mode == core.ForkDetached {
		// A caller owns this stable target across processes; the process-local lock
		// also serializes retries through distinct Store handles.
		if err = st.discardUnpublished(ctx, r.TargetID); err != nil {
			return state, err
		}
		st.write.Lock()
		tx, e := st.db.BeginTx(ctx, nil)
		if e == nil {
			e = payloadCandidates(ctx, tx)
			var raw []byte
			if e == nil {
				raw, _, e = encodeValue(ctx, tx, r.RolloutID(), "state", ownerKey(r.TargetID), state, state12)
			}
			digest, _ := r.Digest()
			if e == nil {
				_, e = tx.ExecContext(ctx, "INSERT INTO rollouts(id,status,seq) VALUES(?,'building',?)", r.RolloutID(), head.Entry.Sequence)
			}
			if e == nil {
				_, e = tx.ExecContext(ctx, "INSERT INTO sessions(id,rollout_id,ready,state,fork_digest) VALUES(?,?,0,?,?)", r.TargetID, r.RolloutID(), raw, digest)
			}
			if e == nil {
				e = tx.Commit()
			} else {
				tx.Rollback()
			}
		}
		st.write.Unlock()
		if e != nil {
			return state, conflict(e)
		}
		defer func() {
			if err != nil {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				_ = st.discardUnpublished(cleanup, r.TargetID)
			}
		}()
		cursor := ""
		for {
			page, e := historypage.Read(ctx, st, r.SourceRolloutID, r.HeadID, cursor)
			if e != nil {
				return state, e
			}
			if e = st.writeForkPage(ctx, r.RolloutID(), page.Records); e != nil {
				return state, e
			}
			if checkpoint != nil {
				checkpoint("page")
			}
			if page.Next == "" {
				break
			}
			cursor = page.Next
		}
	}
	st.write.Lock()
	defer st.write.Unlock()
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return state, err
	}
	defer tx.Rollback()
	var sequence uint64
	var status string
	if err = tx.QueryRowContext(ctx, "SELECT status,seq FROM rollouts WHERE id=?", r.SourceRolloutID).Scan(&status, &sequence); err != nil || status != "active" {
		return state, core.ErrNotFound
	}
	if r.Mode == core.ForkDetached {
		if err = tx.QueryRowContext(ctx, "SELECT status FROM rollouts WHERE id=?", r.RolloutID()).Scan(&status); err != nil || status != "building" {
			return state, core.ErrNotFound
		}
		sequence = head.Entry.Sequence
	}
	_, state, records, receipt, err := core.PrepareFork(r, head, sequence+1)
	if err != nil {
		return state, err
	}
	if err = payloadCandidates(ctx, tx); err != nil {
		return state, err
	}
	raw, _, err := encodeValue(ctx, tx, r.RolloutID(), "state", ownerKey(r.TargetID), state, state12)
	if err != nil {
		return state, err
	}
	digest, _ := r.Digest()
	if r.Mode == core.ForkShared {
		_, err = tx.ExecContext(ctx, "INSERT INTO sessions(id,rollout_id,ready,state,fork_digest) VALUES(?,?,1,?,?)", r.TargetID, r.RolloutID(), raw, digest)
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE sessions SET ready=1,state=? WHERE id=? AND ready=0", raw, r.TargetID)
	}
	if err != nil {
		return state, conflict(err)
	}
	if err = insert(ctx, tx, r.RolloutID(), records, true); err != nil {
		return state, err
	}
	if err = saveReceipt(ctx, tx, r.RolloutID(), r.TargetID, receipt); err != nil {
		return state, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE rollouts SET status='active',seq=? WHERE id=?", receipt.LastSequence, r.RolloutID()); err != nil {
		return state, err
	}
	if err = collectPayloads(ctx, tx); err != nil {
		return state, err
	}
	if checkpoint != nil {
		checkpoint("before_publish")
	}
	err = tx.Commit()
	if err == nil && checkpoint != nil {
		checkpoint("after_publish")
	}
	return state, err
}

// DiscardUnpublished only removes a private detached Rollout. Published history
// is never eligible. The host must own this target before cross-process cleanup.
func (st *Store) DiscardUnpublished(ctx context.Context, id string) error {
	unlock, err := st.lockFork(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	return st.discardUnpublished(ctx, id)
}
func (st *Store) discardUnpublished(ctx context.Context, id string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		st.write.Lock()
		tx, err := st.db.BeginTx(ctx, nil)
		if err != nil {
			st.write.Unlock()
			return err
		}
		var rolloutID, status string
		var ready int
		err = tx.QueryRowContext(ctx, "SELECT s.rollout_id,s.ready,r.status FROM sessions s JOIN rollouts r ON r.id=s.rollout_id WHERE s.id=?", id).Scan(&rolloutID, &ready, &status)
		if errors.Is(err, sql.ErrNoRows) {
			tx.Rollback()
			st.write.Unlock()
			return nil
		}
		if err != nil || ready != 0 || status != "building" {
			tx.Rollback()
			st.write.Unlock()
			if err != nil {
				return err
			}
			return core.ErrConflict
		}
		n, e := discardEntryPage(ctx, tx, rolloutID)
		if e == nil && n == 0 {
			if e = removeRolloutPayloads(ctx, tx, rolloutID); e != nil {
				tx.Rollback()
				st.write.Unlock()
				return e
			}
			for _, query := range []string{"DELETE FROM pending WHERE rollout_id=?", "DELETE FROM receipts WHERE rollout_id=?", "DELETE FROM attempts WHERE rollout_id=?", "DELETE FROM sessions WHERE rollout_id=?", "DELETE FROM rollouts WHERE id=?"} {
				if _, e = tx.ExecContext(ctx, query, rolloutID); e != nil {
					break
				}
			}
		}
		if e == nil {
			e = tx.Commit()
		} else {
			tx.Rollback()
		}
		st.write.Unlock()
		if e != nil {
			return e
		}
		if n == 0 {
			return nil
		}
	}
}

var _ core.Store = (*Store)(nil)
