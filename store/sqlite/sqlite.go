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
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	core "github.com/oines/astral-core"
	s "github.com/oines/astral-core/session"
	_ "modernc.org/sqlite"
)

type Store struct {
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

const schema = `
CREATE TABLE format(version INTEGER NOT NULL);
INSERT INTO format VALUES(2);
CREATE TABLE sessions(id TEXT PRIMARY KEY, ready INTEGER NOT NULL, state BLOB NOT NULL);
CREATE TABLE entries(session_id TEXT NOT NULL,id TEXT NOT NULL,seq INTEGER NOT NULL,version INTEGER NOT NULL,parent_id TEXT NOT NULL,kind TEXT NOT NULL,reference TEXT NOT NULL,payload BLOB NOT NULL,projection BLOB,PRIMARY KEY(session_id,id),UNIQUE(session_id,seq));
CREATE INDEX entry_parent ON entries(session_id,parent_id,seq);
CREATE INDEX entry_reference ON entries(session_id,kind,reference,seq);
CREATE TABLE receipts(session_id TEXT NOT NULL,commit_id TEXT NOT NULL,version INTEGER NOT NULL,digest TEXT NOT NULL,PRIMARY KEY(session_id,commit_id));
CREATE TABLE pending(session_id TEXT NOT NULL,kind TEXT NOT NULL,input_kind TEXT NOT NULL,key TEXT NOT NULL,owner TEXT NOT NULL,seq INTEGER NOT NULL,payload BLOB NOT NULL,PRIMARY KEY(session_id,kind,key));
CREATE INDEX pending_order ON pending(session_id,kind,input_kind,seq);
CREATE INDEX pending_owner ON pending(session_id,kind,owner);
PRAGMA user_version=2;
`

// CROSS JOIN keeps the bounded chain on the outside of each indexed lookup.
// Without it SQLite can scan every entry in the session before joining a page.
const historyPathSQL = `WITH RECURSIVE chain(id,parent_id,seq,depth) AS (
 SELECT e.id,e.parent_id,e.seq,0 FROM entries e JOIN sessions s ON s.id=e.session_id WHERE e.session_id=? AND e.id=? AND s.ready=1
 UNION ALL SELECT e.id,e.parent_id,e.seq,c.depth+1 FROM chain c CROSS JOIN entries e ON e.id=c.parent_id WHERE e.session_id=? AND c.depth+1<? AND c.id<>?
 ) SELECT e.payload,e.projection FROM chain c CROSS JOIN entries e ON e.session_id=? AND e.id=c.id ORDER BY c.depth`

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, core.ErrInvalid
	}
	abs, err := filepath.Abs(path)
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
			return nil, s.ErrUnsupportedFormat
		}
	} else if !errors.Is(openErr, os.ErrNotExist) {
		return nil, openErr
	}
	uri := url.URL{Scheme: "file", Path: abs}
	// Inspect an existing database before any schema or journal mutation.
	if existing {
		check, err := sql.Open("sqlite", uri.String()+"?mode=ro")
		if err != nil {
			return nil, err
		}
		var version int
		err = check.QueryRow("SELECT version FROM format").Scan(&version)
		check.Close()
		if err != nil || version != s.FormatVersion {
			return nil, s.ErrUnsupportedFormat
		}
	}
	if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return nil, err
	}
	values := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "synchronous(FULL)", "busy_timeout(5000)", "temp_store(FILE)", "cache_size(-2048)"} {
		values.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", uri.String()+"?"+values.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	st := &Store{db: db}
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
func decodeState(raw []byte, id string) (s.RuntimeState, error) {
	var state s.RuntimeState
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, s.ErrCorrupt
	}
	if state.FormatVersion != s.FormatVersion {
		return state, s.ErrUnsupportedFormat
	}
	if state.SessionID != id || state.Version == 0 {
		return state, s.ErrCorrupt
	}
	return state, nil
}
func (st *Store) LoadState(ctx context.Context, id string) (s.RuntimeState, error) {
	var raw []byte
	err := st.db.QueryRowContext(ctx, "SELECT state FROM sessions WHERE id=? AND ready=1", id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return s.RuntimeState{}, s.ErrNotFound
	}
	if err != nil {
		return s.RuntimeState{}, err
	}
	st.rows.Add(1)
	return decodeState(raw, id)
}
func decodeRecord(payload, projection []byte) (s.Record, error) {
	var r s.Record
	if json.Unmarshal(payload, &r.Entry) != nil {
		return r, s.ErrCorrupt
	}
	if len(projection) > 0 {
		if json.Unmarshal(projection, &r.Projection) != nil {
			return r, s.ErrCorrupt
		}
	}
	return r, nil
}
func (st *Store) Get(ctx context.Context, id, key string) (s.Record, error) {
	var payload, projection []byte
	err := st.db.QueryRowContext(ctx, "SELECT e.payload,e.projection FROM entries e JOIN sessions s ON s.id=e.session_id WHERE e.session_id=? AND e.id=? AND s.ready=1", id, key).Scan(&payload, &projection)
	if errors.Is(err, sql.ErrNoRows) {
		return s.Record{}, s.ErrNotFound
	}
	if err != nil {
		return s.Record{}, err
	}
	st.rows.Add(1)
	return decodeRecord(payload, projection)
}
func (st *Store) LookupCommit(ctx context.Context, id, key string) (s.Receipt, error) {
	var r s.Receipt
	var raw []byte
	r.CommitID = key
	err := st.db.QueryRowContext(ctx, "SELECT r.version,r.digest,s.state FROM receipts r JOIN sessions s ON s.id=r.session_id WHERE r.session_id=? AND r.commit_id=? AND s.ready=1", id, key).Scan(&r.Version, &r.Digest, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return r, s.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	state, err := decodeState(raw, id)
	r.CurrentVersion = state.Version
	st.rows.Add(1)
	return r, err
}
func (st *Store) Scan(ctx context.Context, id string, q s.Query) (s.Page, error) {
	limit, maxBytes := s.PageLimits(q.Limit, q.MaxBytes)
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
		query = "SELECT e.payload,e.projection FROM entries e JOIN sessions s ON s.id=e.session_id WHERE e.session_id=? AND s.ready=1"
		args = []any{id}
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
				return s.Page{}, core.ErrInvalid
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
		return s.Page{}, err
	}
	defer rows.Close()
	page := s.Page{}
	size := 0
	for rows.Next() {
		var payload, projection []byte
		if err = rows.Scan(&payload, &projection); err != nil {
			return s.Page{}, err
		}
		st.rows.Add(1)
		recordBytes := len(payload) + len(`{"entry":}`)
		if len(projection) > 0 {
			recordBytes += len(projection) + len(`,"projection":`)
		}
		if len(page.Records) == limit || size+recordBytes > maxBytes {
			if len(page.Records) == 0 {
				return s.Page{}, s.ErrBudget
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
			return s.Page{}, err
		}
		if q.PathHead != "" && r.Projection == nil {
			return s.Page{}, s.ErrNotFound
		}
		page.Records = append(page.Records, r)
		size += recordBytes
		if q.PathHead != "" && r.Entry.ID == q.StopID {
			break
		}
	}
	if err = rows.Err(); err != nil {
		return s.Page{}, err
	}
	if len(page.Records) == 0 {
		if _, err = st.LoadState(ctx, id); err != nil {
			return s.Page{}, err
		}
		if q.PathHead != "" {
			return s.Page{}, s.ErrNotFound
		}
	}
	return page, nil
}
func (st *Store) Pending(ctx context.Context, id string, q s.PendingQuery) (s.Page, error) {
	limit, maxBytes := s.PageLimits(q.Limit, q.MaxBytes)
	query := "SELECT p.payload FROM pending p JOIN sessions s ON s.id=p.session_id WHERE p.session_id=? AND s.ready=1"
	args := []any{id}
	if q.Kind != "" {
		query += " AND p.kind=?"
		args = append(args, string(q.Kind))
	}
	if q.InputKind != "" {
		query += " AND p.input_kind=?"
		args = append(args, string(q.InputKind))
	}
	if q.Cursor != "" {
		cursor, err := strconv.ParseUint(q.Cursor, 10, 63)
		if err != nil {
			return s.Page{}, core.ErrInvalid
		}
		query += " AND p.seq>?"
		args = append(args, cursor)
	}
	query += " ORDER BY p.seq LIMIT ?"
	args = append(args, limit+1)
	rows, err := st.db.QueryContext(ctx, query, args...)
	if err != nil {
		return s.Page{}, err
	}
	defer rows.Close()
	page := s.Page{}
	size := 0
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return s.Page{}, err
		}
		st.rows.Add(1)
		recordBytes := len(raw) + len(`{"entry":}`)
		if len(page.Records) == limit || size+recordBytes > maxBytes {
			if len(page.Records) == 0 {
				return s.Page{}, s.ErrBudget
			}
			page.Next = strconv.FormatUint(page.Records[len(page.Records)-1].Entry.Sequence, 10)
			break
		}
		var e s.Entry
		if json.Unmarshal(raw, &e) != nil {
			return s.Page{}, s.ErrCorrupt
		}
		page.Records = append(page.Records, s.Record{Entry: e})
		size += recordBytes
	}
	if err = rows.Err(); err != nil {
		return s.Page{}, err
	}
	if len(page.Records) == 0 {
		_, err = st.LoadState(ctx, id)
	}
	return page, err
}
func conflict(err error) error {
	var code interface{ Code() int }
	if errors.As(err, &code) && (code.Code()&255 == 5 || code.Code()&255 == 6 || code.Code()&255 == 19) {
		return fmt.Errorf("%w: %v", s.ErrConflict, err)
	}
	return err
}
func equal(a, b any) bool {
	x, e := json.Marshal(a)
	y, f := json.Marshal(b)
	return e == nil && f == nil && string(x) == string(y)
}
func (st *Store) Commit(ctx context.Context, id string, c s.Commit) (receipt s.Receipt, err error) {
	if id == "" {
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
	var ready int
	state := s.RuntimeState{}
	err = tx.QueryRowContext(ctx, "SELECT ready,state FROM sessions WHERE id=?", id).Scan(&ready, &raw)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return receipt, err
	}
	if exists {
		if ready == 0 {
			return receipt, s.ErrConflict
		}
		state, err = decodeState(raw, id)
		if err != nil {
			return receipt, err
		}
		st.rows.Add(1)
	}
	old := s.Receipt{CommitID: c.ID}
	err = tx.QueryRowContext(ctx, "SELECT version,digest FROM receipts WHERE session_id=? AND commit_id=?", id, c.ID).Scan(&old.Version, &old.Digest)
	if err == nil {
		if old.Digest != digest {
			return receipt, s.ErrConflict
		}
		old.CurrentVersion = state.Version
		return old, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return receipt, err
	}
	if state.Version != c.ExpectedVersion {
		return receipt, s.ErrConflict
	}
	next, records, err := s.Reduce(state, c)
	if err != nil {
		return receipt, err
	}
	if next.SessionID != id {
		return receipt, s.ErrCorrupt
	}
	for _, e := range c.Entries {
		var one int
		check := tx.QueryRowContext(ctx, "SELECT 1 FROM entries WHERE session_id=? AND id=?", id, e.ID).Scan(&one)
		if check == nil {
			return receipt, s.ErrCorrupt
		}
		if !errors.Is(check, sql.ErrNoRows) {
			return receipt, check
		}
		if e.Head != nil {
			var projection []byte
			if err = tx.QueryRowContext(ctx, "SELECT projection FROM entries WHERE session_id=? AND id=?", id, e.Head.EntryID).Scan(&projection); err != nil {
				return receipt, s.ErrCorrupt
			}
			var p s.Projection
			if json.Unmarshal(projection, &p) != nil || !equal(&p, c.Selection) {
				return receipt, s.ErrCorrupt
			}
		}
		if e.Compaction != nil {
			var one int
			if err = tx.QueryRowContext(ctx, "SELECT 1 FROM entries WHERE session_id=? AND id=? AND projection IS NOT NULL", id, e.Compaction.ThroughID).Scan(&one); err != nil {
				return receipt, s.ErrCorrupt
			}
		}
		if e.Kind == s.EntryInputConsumed {
			var kind string
			if err = tx.QueryRowContext(ctx, "SELECT input_kind FROM pending WHERE session_id=? AND kind=? AND key=?", id, string(s.EntryInputQueued), e.Input.ID).Scan(&kind); err != nil || kind != string(e.Input.Kind) {
				return receipt, s.ErrCorrupt
			}
		}
	}
	raw, err = json.Marshal(next)
	if err != nil {
		return receipt, err
	}
	if !exists {
		_, err = tx.ExecContext(ctx, "INSERT INTO sessions(id,ready,state) VALUES(?,1,?)", id, raw)
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE sessions SET state=? WHERE id=?", raw, id)
	}
	if err != nil {
		return receipt, err
	}
	if err = insert(ctx, tx, id, records, true); err != nil {
		return receipt, err
	}
	receipt = s.Receipt{CommitID: c.ID, Version: next.Version, CurrentVersion: next.Version, Digest: digest}
	if _, err = tx.ExecContext(ctx, "INSERT INTO receipts(session_id,commit_id,version,digest) VALUES(?,?,?,?)", id, c.ID, next.Version, digest); err != nil {
		return receipt, err
	}
	if err = tx.Commit(); err != nil {
		return receipt, err
	}
	st.commits.Add(1)
	return receipt, nil
}
func insert(ctx context.Context, tx *sql.Tx, id string, records []s.Record, pending bool) error {
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO entries(session_id,id,seq,version,parent_id,kind,reference,payload,projection) VALUES(?,?,?,?,?,?,?,?,?)")
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
		if _, err = stmt.ExecContext(ctx, id, e.ID, e.Sequence, e.Version, e.ParentID, string(e.Kind), s.Reference(e), payload, projection); err != nil {
			return err
		}
		if !pending {
			continue
		}
		switch e.Kind {
		case s.EntryInputQueued, s.EntryApprovalRequested:
			kind, key, owner := "", s.Reference(e), ""
			if e.Input != nil {
				kind = string(e.Input.Kind)
			}
			if e.Approval != nil {
				owner = e.Approval.ExecutionID
			}
			_, err = tx.ExecContext(ctx, "INSERT INTO pending(session_id,kind,input_kind,key,owner,seq,payload) VALUES(?,?,?,?,?,?,?)", id, string(e.Kind), kind, key, owner, e.Sequence, payload)
		case s.EntryInputConsumed:
			_, err = tx.ExecContext(ctx, "DELETE FROM pending WHERE session_id=? AND kind=? AND key=?", id, string(s.EntryInputQueued), e.Input.ID)
		case s.EntryApprovalDecided:
			_, err = tx.ExecContext(ctx, "DELETE FROM pending WHERE session_id=? AND kind=? AND key=?", id, string(s.EntryApprovalRequested), e.Approval.ID)
		case s.EntryToolResult, s.EntryToolResolution:
			_, err = tx.ExecContext(ctx, "DELETE FROM pending WHERE session_id=? AND kind=? AND owner=?", id, string(s.EntryApprovalRequested), e.Tool.Invocation.ExecutionID)
		case s.EntryMessage, s.EntryHeadSelected:
			_, err = tx.ExecContext(ctx, "DELETE FROM pending WHERE session_id=? AND kind=?", id, string(s.EntryApprovalRequested))
		}
		if err != nil {
			return err
		}
	}
	return nil
}
func (st *Store) writeForkPage(ctx context.Context, id string, records []s.Record) error {
	st.write.Lock()
	defer st.write.Unlock()
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = insert(ctx, tx, id, records, false); err != nil {
		return err
	}
	return tx.Commit()
}
func (st *Store) Fork(ctx context.Context, r s.ForkRequest) (state s.RuntimeState, err error) {
	return st.fork(ctx, r, nil)
}

// afterPage is an internal crash-test seam. It runs outside every transaction
// and writer lock, at the same yield boundary used by ordinary Fork.
func (st *Store) fork(ctx context.Context, r s.ForkRequest, afterPage func()) (state s.RuntimeState, err error) {
	if err = s.ValidateFork(r); err != nil {
		return state, err
	}
	head, err := st.Get(ctx, r.SourceID, r.HeadID)
	if err != nil {
		return state, err
	}
	state, err = s.ForkState(r, head)
	if err != nil {
		return state, err
	}
	raw, _ := json.Marshal(state)
	st.write.Lock()
	_, err = st.db.ExecContext(ctx, "INSERT INTO sessions(id,ready,state) VALUES(?,0,?)", r.TargetID, raw)
	st.write.Unlock()
	if err != nil {
		return state, conflict(err)
	}
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_ = st.DiscardUnpublished(cleanup, r.TargetID)
		}
	}()
	if err = st.writeForkPage(ctx, r.TargetID, []s.Record{{Entry: r.Creation}}); err != nil {
		return state, err
	}
	cursor := ""
	for {
		var page s.Page
		page, err = st.Scan(ctx, r.SourceID, s.Query{PathHead: r.HeadID, Cursor: cursor, Limit: s.DefaultPageSize, MaxBytes: s.DefaultContextBytes})
		if err != nil {
			return state, err
		}
		for i := range page.Records {
			page.Records[i].Entry.Version = 1
		}
		if err = st.writeForkPage(ctx, r.TargetID, page.Records); err != nil {
			return state, err
		}
		if afterPage != nil {
			afterPage()
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if r.Environment != nil {
		var records []s.Record
		state, records, err = s.Reduce(state, s.Commit{ID: r.Environment.ID, ExpectedVersion: state.Version, Entries: []s.Entry{*r.Environment}})
		if err != nil {
			return state, err
		}
		if err = st.writeForkPage(ctx, r.TargetID, records); err != nil {
			return state, err
		}
	}
	raw, err = json.Marshal(state)
	if err != nil {
		return state, err
	}
	st.write.Lock()
	defer st.write.Unlock()
	_, err = st.db.ExecContext(ctx, "UPDATE sessions SET ready=1,state=? WHERE id=? AND ready=0", raw, r.TargetID)
	return state, err
}

// DiscardUnpublished removes one interrupted fork in short transactions. It
// refuses published sessions. It is explicit cleanup, not background history GC.
func (st *Store) DiscardUnpublished(ctx context.Context, id string) error {
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
		var ready int
		err = tx.QueryRowContext(ctx, "SELECT ready FROM sessions WHERE id=?", id).Scan(&ready)
		if errors.Is(err, sql.ErrNoRows) {
			tx.Rollback()
			st.write.Unlock()
			return nil
		}
		if err != nil || ready != 0 {
			tx.Rollback()
			st.write.Unlock()
			if err != nil {
				return err
			}
			return s.ErrConflict
		}
		result, err := tx.ExecContext(ctx, "DELETE FROM entries WHERE session_id=? AND id IN (SELECT id FROM entries WHERE session_id=? LIMIT ?)", id, id, s.DefaultPageSize)
		var n int64
		if err == nil {
			n, err = result.RowsAffected()
		}
		if err == nil && n == 0 {
			_, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE id=? AND ready=0", id)
		}
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
		st.write.Unlock()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

var _ s.Store = (*Store)(nil)
