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
	"strconv"
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
func (st *Store) Close() error { return st.db.Close() }

var schema = fmt.Sprintf(`
CREATE TABLE format(version INTEGER NOT NULL);
INSERT INTO format VALUES(%d);
CREATE TABLE rollouts(id TEXT PRIMARY KEY,status TEXT NOT NULL,seq INTEGER NOT NULL);
CREATE TABLE sessions(id TEXT PRIMARY KEY,rollout_id TEXT NOT NULL,ready INTEGER NOT NULL,state BLOB NOT NULL,fork_digest TEXT NOT NULL DEFAULT '');
CREATE INDEX session_rollout ON sessions(rollout_id,id);
CREATE TABLE entries(rollout_id TEXT NOT NULL,session_id TEXT NOT NULL,id TEXT NOT NULL,seq INTEGER NOT NULL,version INTEGER NOT NULL,parent_id TEXT NOT NULL,kind TEXT NOT NULL,reference TEXT NOT NULL,payload BLOB NOT NULL,projection BLOB,PRIMARY KEY(rollout_id,id),UNIQUE(rollout_id,seq));
CREATE INDEX entry_parent ON entries(rollout_id,parent_id,seq) WHERE projection IS NOT NULL;
CREATE INDEX entry_history ON entries(rollout_id,seq) WHERE projection IS NOT NULL;
CREATE INDEX entry_kind ON entries(rollout_id,kind,seq);
CREATE INDEX entry_reference ON entries(rollout_id,kind,reference,seq);
CREATE INDEX entry_writer ON entries(rollout_id,session_id,seq);
CREATE TABLE receipts(rollout_id TEXT NOT NULL,session_id TEXT NOT NULL,commit_id TEXT NOT NULL,version INTEGER NOT NULL,digest TEXT NOT NULL,first_seq INTEGER NOT NULL,last_seq INTEGER NOT NULL,PRIMARY KEY(session_id,commit_id));
CREATE INDEX receipt_rollout ON receipts(rollout_id);
CREATE TABLE pending(rollout_id TEXT NOT NULL,session_id TEXT NOT NULL,kind TEXT NOT NULL,input_kind TEXT NOT NULL,key TEXT NOT NULL,owner TEXT NOT NULL,seq INTEGER NOT NULL,payload BLOB NOT NULL,PRIMARY KEY(session_id,kind,key));
CREATE INDEX pending_order ON pending(session_id,kind,input_kind,seq);
CREATE INDEX pending_owner ON pending(session_id,kind,owner);
CREATE INDEX pending_rollout ON pending(rollout_id);
CREATE TABLE inputs(session_id TEXT NOT NULL,id TEXT NOT NULL,kind TEXT NOT NULL,digest TEXT NOT NULL,accepted_at TEXT NOT NULL,payload BLOB NOT NULL,state TEXT NOT NULL,entry_id TEXT NOT NULL DEFAULT '',PRIMARY KEY(session_id,id));
CREATE INDEX input_status ON inputs(session_id,kind,state);

PRAGMA user_version=%d;
`, core.FormatVersion, core.FormatVersion)

// CROSS JOIN keeps the bounded chain on the outside of each indexed lookup.
// Without it SQLite can scan every entry in the session before joining a page.
const historyPathSQL = `WITH RECURSIVE chain(id,parent_id,seq,depth) AS (
 SELECT e.id,e.parent_id,e.seq,0 FROM entries e JOIN rollouts r ON r.id=e.rollout_id WHERE e.rollout_id=? AND e.id=? AND r.status='active'
 UNION ALL SELECT e.id,e.parent_id,e.seq,c.depth+1 FROM chain c CROSS JOIN entries e ON e.id=c.parent_id WHERE e.rollout_id=? AND c.depth+1<? AND c.id<>?
 ) SELECT e.payload,e.projection FROM chain c CROSS JOIN entries e ON e.rollout_id=? AND e.id=c.id ORDER BY c.depth`

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
		var version int
		err = check.QueryRow("SELECT version FROM format").Scan(&version)
		check.Close()
		if err != nil || !core.SupportsFormat(version) {
			return nil, core.ErrUnsupportedFormat
		}
	}
	values := url.Values{}
	values.Set("_txlock", "immediate")
	for _, p := range []string{"journal_mode(WAL)", "synchronous(FULL)", "busy_timeout(5000)", "temp_store(FILE)", "cache_size(-2048)"} {
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
func (st *Store) LoadState(ctx context.Context, id string) (core.RuntimeState, error) {
	var raw []byte
	err := st.db.QueryRowContext(ctx, "SELECT s.state FROM sessions s JOIN rollouts r ON r.id=s.rollout_id WHERE s.id=? AND s.ready=1 AND r.status='active'", id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return core.RuntimeState{}, core.ErrNotFound
	}
	if err != nil {
		return core.RuntimeState{}, err
	}
	st.rows.Add(1)
	return decodeState(raw, id)
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
func (st *Store) Get(ctx context.Context, id, key string) (core.Record, error) {
	var payload, projection []byte
	err := st.db.QueryRowContext(ctx, "SELECT e.payload,e.projection FROM entries e JOIN rollouts r ON r.id=e.rollout_id WHERE e.rollout_id=? AND e.id=? AND r.status='active'", id, key).Scan(&payload, &projection)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Record{}, core.ErrNotFound
	}
	if err != nil {
		return core.Record{}, err
	}
	st.rows.Add(1)
	return decodeRecord(payload, projection)
}
func (st *Store) LookupCommit(ctx context.Context, id, key string) (core.Receipt, error) {
	var r core.Receipt
	var raw []byte
	r.CommitID = key
	err := st.db.QueryRowContext(ctx, "SELECT r.version,r.digest,r.first_seq,r.last_seq,s.state FROM receipts r JOIN sessions s ON s.id=r.session_id JOIN rollouts o ON o.id=s.rollout_id WHERE r.session_id=? AND r.commit_id=? AND s.ready=1 AND o.status='active'", id, key).Scan(&r.Version, &r.Digest, &r.FirstSequence, &r.LastSequence, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return r, core.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	state, err := decodeState(raw, id)
	r.CurrentVersion = state.Version
	st.rows.Add(1)
	return r, err
}
func (st *Store) Scan(ctx context.Context, id string, q core.Query) (core.Page, error) {
	limit, maxBytes := core.PageLimits(q.Limit, q.MaxBytes)
	var query string
	var args []any
	if q.PathHead != "" {
		head := q.PathHead
		if q.Cursor != "" {
			head = q.Cursor
		}
		query = historyPathSQL
		args = []any{id, head, id, limit + 1, q.StopID, id}
	} else {
		query = "SELECT e.payload,e.projection FROM entries e JOIN rollouts r ON r.id=e.rollout_id WHERE e.rollout_id=? AND r.status='active'"
		args = []any{id}
		if q.SessionID != "" {
			query += " AND e.session_id=?"
			args = append(args, q.SessionID)
		}
		if q.ParentID != nil {
			query += " AND e.parent_id=? AND e.projection IS NOT NULL"
			args = append(args, *q.ParentID)
		}
		if q.Kind != "" {
			query += " AND e.kind=?"
			args = append(args, string(q.Kind))
		}
		if q.Reference != "" {
			query += " AND e.reference=?"
			args = append(args, q.Reference)
		}
		if q.HistoryOnly {
			query += " AND e.projection IS NOT NULL"
		}
		if q.Cursor != "" {
			seq, err := strconv.ParseUint(q.Cursor, 10, 63)
			if err != nil {
				return core.Page{}, core.ErrInvalid
			}
			if q.Reverse {
				query += " AND e.seq<?"
			} else {
				query += " AND e.seq>?"
			}
			args = append(args, seq)
		}
		if q.Reverse {
			query += " ORDER BY e.seq DESC"
		} else {
			query += " ORDER BY e.seq"
		}
		query += " LIMIT ?"
		args = append(args, limit+1)
	}
	rows, err := st.db.QueryContext(ctx, query, args...)
	if err != nil {
		return core.Page{}, err
	}
	defer rows.Close()
	page := core.Page{}
	size := 0
	for rows.Next() {
		var payload, projection []byte
		if err = rows.Scan(&payload, &projection); err != nil {
			return core.Page{}, err
		}
		st.rows.Add(1)
		recordBytes := len(payload) + len(`{"entry":}`)
		if len(projection) > 0 {
			recordBytes += len(projection) + len(`,"projection":`)
		}
		if len(page.Records) == limit || size+recordBytes > maxBytes {
			if len(page.Records) == 0 {
				return core.Page{}, core.ErrBudget
			}
			last := page.Records[len(page.Records)-1].Entry
			if q.PathHead != "" {
				page.Next = last.ParentID
			} else {
				page.Next = strconv.FormatUint(last.Sequence, 10)
			}
			break
		}
		r, err := decodeRecord(payload, projection)
		if err != nil {
			return core.Page{}, err
		}
		if q.PathHead != "" && r.Projection == nil {
			return core.Page{}, core.ErrNotFound
		}
		page.Records = append(page.Records, r)
		size += recordBytes
		if q.PathHead != "" && r.Entry.ID == q.StopID {
			break
		}
	}
	if err = rows.Err(); err != nil {
		return core.Page{}, err
	}
	if len(page.Records) == 0 {
		if err = st.activeRollout(ctx, id); err != nil {
			return core.Page{}, err
		}
		if q.PathHead != "" {
			return core.Page{}, core.ErrNotFound
		}
	}
	return page, nil
}
func (st *Store) Pending(ctx context.Context, id string, q core.PendingQuery) (core.Page, error) {
	limit, maxBytes := core.PageLimits(q.Limit, q.MaxBytes)
	query := "SELECT p.payload FROM pending p JOIN sessions s ON s.id=p.session_id JOIN rollouts r ON r.id=s.rollout_id WHERE p.session_id=? AND s.ready=1 AND r.status='active'"
	args := []any{id}
	if q.Kind != "" {
		query += " AND p.kind=?"
		args = append(args, string(q.Kind))
	}
	if q.InputKind != "" {
		query += " AND p.input_kind=?"
		args = append(args, string(q.InputKind))
	}
	if q.InputID != "" {
		query += " AND p.kind=? AND p.key=?"
		args = append(args, string(core.EntryInputQueued), q.InputID)
	}
	if q.Cursor != "" {
		cursor, err := strconv.ParseUint(q.Cursor, 10, 63)
		if err != nil {
			return core.Page{}, core.ErrInvalid
		}
		query += " AND p.seq>?"
		args = append(args, cursor)
	}
	query += " ORDER BY p.seq LIMIT ?"
	args = append(args, limit+1)
	rows, err := st.db.QueryContext(ctx, query, args...)
	if err != nil {
		return core.Page{}, err
	}
	defer rows.Close()
	page := core.Page{}
	size := 0
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return core.Page{}, err
		}
		st.rows.Add(1)
		recordBytes := len(raw) + len(`{"entry":}`)
		if len(page.Records) == limit || size+recordBytes > maxBytes {
			if len(page.Records) == 0 {
				return core.Page{}, core.ErrBudget
			}
			page.Next = strconv.FormatUint(page.Records[len(page.Records)-1].Entry.Sequence, 10)
			break
		}
		var e core.Entry
		if json.Unmarshal(raw, &e) != nil {
			return core.Page{}, core.ErrCorrupt
		}
		page.Records = append(page.Records, core.Record{Entry: e})
		size += recordBytes
	}
	if err = rows.Err(); err != nil {
		return core.Page{}, err
	}
	if len(page.Records) == 0 {
		_, err = st.LoadState(ctx, id)
	}
	return page, err
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
	if id == "" || c.ID == core.ForkCommitID || len(c.Entries) == 0 {
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
		state, err = decodeState(raw, id)
		if err != nil {
			return receipt, err
		}
		st.rows.Add(1)
	} else {
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

	raw, err = json.Marshal(next)
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
	if err = saveReceipt(ctx, tx, rolloutID, id, receipt); err != nil {
		return receipt, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE rollouts SET seq=? WHERE id=?", receipt.LastSequence, rolloutID); err != nil {
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
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO entries(rollout_id,session_id,id,seq,version,parent_id,kind,reference,payload,projection) VALUES(?,?,?,?,?,?,?,?,?,?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range records {
		e := r.Entry
		payload, err := json.Marshal(e)
		if err != nil {
			return err
		}
		var projection any
		if r.Projection != nil {
			b, err := json.Marshal(r.Projection)
			if err != nil {
				return err
			}
			projection = b
		}
		if _, err = stmt.ExecContext(ctx, id, e.SessionID, e.ID, e.Sequence, e.Version, e.ParentID, string(e.Kind), core.Reference(e), payload, projection); err != nil {
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
			frozen, encodeErr := json.Marshal(e.Input)
			if encodeErr != nil {
				return encodeErr
			}
			if e.Kind == core.EntryInputRequeued {
				_, err = tx.ExecContext(ctx, "UPDATE inputs SET kind=?,payload=?,state=? WHERE session_id=? AND id=?", string(e.Input.Kind), frozen, string(core.InputPending), e.SessionID, e.Input.ID)
			} else {
				_, err = tx.ExecContext(ctx, "INSERT INTO inputs(session_id,id,kind,digest,accepted_at,payload,state) VALUES(?,?,?,?,?,?,?)", e.SessionID, e.Input.ID, string(e.Input.Kind), e.Input.Digest, e.Time.Format(time.RFC3339Nano), frozen, string(core.InputPending))
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
			_, err = tx.ExecContext(ctx, "INSERT INTO pending(rollout_id,session_id,kind,input_kind,key,owner,seq,payload) VALUES(?,?,?,?,?,?,?,?)", id, e.SessionID, string(pendingKind), kind, key, owner, e.Sequence, payload)
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
	var ready int
	var raw []byte
	var digest, rolloutID string
	err := st.db.QueryRowContext(ctx, "SELECT ready,state,fork_digest,rollout_id FROM sessions WHERE id=?", r.TargetID).Scan(&ready, &raw, &digest, &rolloutID)
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
	state, err := decodeState(raw, r.TargetID)
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
			raw, _ := json.Marshal(state)
			digest, _ := r.Digest()
			_, e = tx.ExecContext(ctx, "INSERT INTO rollouts(id,status,seq) VALUES(?,'building',?)", r.RolloutID(), head.Entry.Sequence)
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
	raw, err := json.Marshal(state)
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
		result, e := tx.ExecContext(ctx, "DELETE FROM entries WHERE rollout_id=? AND id IN (SELECT id FROM entries WHERE rollout_id=? LIMIT ?)", rolloutID, rolloutID, core.DefaultPageSize)
		var n int64
		if e == nil {
			n, e = result.RowsAffected()
		}
		if e == nil && n == 0 {
			for _, query := range []string{"DELETE FROM pending WHERE rollout_id=?", "DELETE FROM receipts WHERE rollout_id=?", "DELETE FROM sessions WHERE rollout_id=?", "DELETE FROM rollouts WHERE id=?"} {
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

func (st *Store) LookupInput(ctx context.Context, id, key string) (core.InputReceipt, error) {
	var receipt core.InputReceipt
	var raw []byte
	var accepted string
	err := st.db.QueryRowContext(ctx, "SELECT i.payload,i.accepted_at,i.state,i.entry_id FROM inputs i JOIN sessions ss ON ss.id=i.session_id JOIN rollouts r ON r.id=ss.rollout_id WHERE i.session_id=? AND i.id=? AND ss.ready=1 AND r.status='active'", id, key).Scan(&raw, &accepted, &receipt.State, &receipt.EntryID)
	if errors.Is(err, sql.ErrNoRows) {
		return receipt, core.ErrNotFound
	}
	if err != nil {
		return receipt, err
	}
	st.rows.Add(1)
	if err = json.Unmarshal(raw, &receipt.InputRecord); err != nil {
		return receipt, core.ErrCorrupt
	}
	receipt.AcceptedAt, err = time.Parse(time.RFC3339Nano, accepted)
	return receipt, err
}
