package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	core "github.com/oines/astral-core"
)

func (st *Store) LoadState(ctx context.Context, id string) (core.RuntimeState, error) {
	tx, err := st.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return core.RuntimeState{}, err
	}
	defer tx.Rollback()
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT s.state FROM sessions s JOIN rollouts r ON r.id=s.rollout_id WHERE s.id=? AND s.ready=1 AND r.status='active'`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return core.RuntimeState{}, core.ErrNotFound
	}
	if err != nil {
		return core.RuntimeState{}, err
	}
	st.rows.Add(1)
	return readState(ctx, tx, raw, id)
}

func (st *Store) Get(ctx context.Context, id, key string) (core.Record, error) {
	tx, err := st.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return core.Record{}, err
	}
	defer tx.Rollback()
	var payload, projection []byte
	err = tx.QueryRowContext(ctx, `SELECT e.payload,e.projection FROM entries e JOIN rollouts r ON r.id=e.rollout_id WHERE e.rollout_id=? AND e.id=? AND r.status='active'`, id, key).Scan(&payload, &projection)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Record{}, core.ErrNotFound
	}
	if err != nil {
		return core.Record{}, err
	}
	st.rows.Add(1)
	raw, err := hydrate(ctx, tx, packedValue{payload, entry12}, packedValue{projection, projection12})
	if err != nil {
		return core.Record{}, err
	}
	return decodeRecord(raw[0], raw[1])
}

func (st *Store) LookupCommit(ctx context.Context, id, key string) (core.Receipt, error) {
	r := core.Receipt{CommitID: key}
	err := st.readDB.QueryRowContext(ctx, `SELECT r.version,r.digest,r.first_seq,r.last_seq,json_extract(s.state,'$.body.version') FROM receipts r JOIN sessions s ON s.id=r.session_id JOIN rollouts o ON o.id=s.rollout_id WHERE r.session_id=? AND r.commit_id=? AND s.ready=1 AND o.status='active'`, id, key).Scan(&r.Version, &r.Digest, &r.FirstSequence, &r.LastSequence, &r.CurrentVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return r, core.ErrNotFound
	}
	st.rows.Add(1)
	return r, err
}

const recordColumns = `e.payload,e.projection,e.payload_size,e.projection_size,e.id,e.seq`

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
		query = `SELECT ` + recordColumns + ` FROM entries e JOIN rollouts r ON r.id=e.rollout_id WHERE e.rollout_id=? AND r.status='active'`
		args = []any{id}
		if q.SessionID != "" {
			query += ` AND e.session_id=?`
			args = append(args, q.SessionID)
		}
		if q.ParentID != nil {
			query += ` AND e.parent_id=? AND e.projection IS NOT NULL`
			args = append(args, *q.ParentID)
		}
		if q.Kind != "" {
			query += ` AND e.kind=?`
			args = append(args, string(q.Kind))
		}
		if q.Reference != "" {
			query += ` AND e.reference=?`
			args = append(args, q.Reference)
		}
		if q.HistoryOnly {
			query += ` AND e.projection IS NOT NULL`
		}
		if q.Cursor != "" {
			seq, err := strconv.ParseUint(q.Cursor, 10, 63)
			if err != nil {
				return core.Page{}, core.ErrInvalid
			}
			if q.Reverse {
				query += ` AND e.seq<?`
			} else {
				query += ` AND e.seq>?`
			}
			args = append(args, seq)
		}
		query += ` ORDER BY e.seq`
		if q.Reverse {
			query += ` DESC`
		}
		query += ` LIMIT ?`
		args = append(args, limit+1)
	}
	tx, err := st.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return core.Page{}, err
	}
	defer tx.Rollback()
	page, err := st.readRecords(ctx, tx, query, args, limit, maxBytes, q.PathHead != "", q.StopID)
	if err != nil {
		return core.Page{}, err
	}
	if len(page.Records) == 0 {
		var one int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM rollouts WHERE id=? AND status='active'`, id).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) || q.PathHead != "" && err == nil {
			return core.Page{}, core.ErrNotFound
		}
	}
	return page, err
}

func (st *Store) readRecords(ctx context.Context, tx *sql.Tx, query string, args []any, limit, maxBytes int, path bool, stop string) (core.Page, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return core.Page{}, err
	}
	defer rows.Close()
	values := []packedValue{}
	size := 0
	more := false
	for rows.Next() {
		var payload, projection []byte
		var payloadSize, projectionSize sql.NullInt64
		var id sql.NullString
		var seq uint64
		if err = rows.Scan(&payload, &projection, &payloadSize, &projectionSize, &id, &seq); err != nil {
			return core.Page{}, err
		}
		st.rows.Add(1)
		if !id.Valid || !payloadSize.Valid || !projectionSize.Valid || payloadSize.Int64 <= 0 || projectionSize.Int64 < 0 {
			return core.Page{}, core.ErrCorrupt
		}
		n := payloadSize.Int64 + int64(len(`{"entry":}`))
		if projectionSize.Int64 > 0 {
			n += projectionSize.Int64 + int64(len(`,"projection":`))
		}
		if n <= 0 {
			return core.Page{}, core.ErrCorrupt
		}
		if len(values)/2 == limit || n > int64(maxBytes-size) {
			if len(values) == 0 {
				return core.Page{}, core.ErrBudget
			}
			more = true
			break
		}
		if path && len(projection) == 0 {
			return core.Page{}, core.ErrNotFound
		}
		values = append(values, packedValue{payload, entry12}, packedValue{projection, projection12})
		size += int(n)
		if path && id.String == stop {
			break
		}
	}
	if err = rows.Err(); err != nil {
		return core.Page{}, err
	}
	if err = rows.Close(); err != nil {
		return core.Page{}, err
	}
	raw, err := hydrate(ctx, tx, values...)
	if err != nil {
		return core.Page{}, err
	}
	page := core.Page{}
	for i := 0; i < len(raw); i += 2 {
		r, err := decodeRecord(raw[i], raw[i+1])
		if err != nil {
			return core.Page{}, err
		}
		page.Records = append(page.Records, r)
	}
	if more {
		last := page.Records[len(page.Records)-1].Entry
		if path {
			page.Next = last.ParentID
		} else {
			page.Next = strconv.FormatUint(last.Sequence, 10)
		}
	}
	return page, nil
}

func (st *Store) Pending(ctx context.Context, id string, q core.PendingQuery) (core.Page, error) {
	limit, maxBytes := core.PageLimits(q.Limit, q.MaxBytes)
	query := `SELECT e.payload,NULL,e.payload_size,0,e.id,p.seq FROM pending p LEFT JOIN entries e ON e.rollout_id=p.rollout_id AND e.id=p.entry_id JOIN sessions s ON s.id=p.session_id JOIN rollouts r ON r.id=s.rollout_id WHERE p.session_id=? AND s.ready=1 AND r.status='active'`
	args := []any{id}
	if q.Kind != "" {
		query += ` AND p.kind=?`
		args = append(args, string(q.Kind))
	}
	if q.InputKind != "" {
		query += ` AND p.input_kind=?`
		args = append(args, string(q.InputKind))
	}
	if q.InputID != "" {
		query += ` AND p.kind=? AND p.key=?`
		args = append(args, string(core.EntryInputQueued), q.InputID)
	}
	if q.Cursor != "" {
		seq, err := strconv.ParseUint(q.Cursor, 10, 63)
		if err != nil {
			return core.Page{}, core.ErrInvalid
		}
		query += ` AND p.seq>?`
		args = append(args, seq)
	}
	query += ` ORDER BY p.seq LIMIT ?`
	args = append(args, limit+1)
	tx, err := st.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return core.Page{}, err
	}
	defer tx.Rollback()
	page, err := st.readRecords(ctx, tx, query, args, limit, maxBytes, false, "")
	if err == nil && len(page.Records) == 0 {
		var one int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM sessions s JOIN rollouts r ON r.id=s.rollout_id WHERE s.id=? AND s.ready=1 AND r.status='active'`, id).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			err = core.ErrNotFound
		}
	}
	return page, err
}

func (st *Store) LookupInput(ctx context.Context, id, key string) (core.InputReceipt, error) {
	var receipt core.InputReceipt
	var raw []byte
	var accepted string
	tx, err := st.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT e.payload,i.accepted_at,i.state,i.entry_id FROM inputs i JOIN sessions s ON s.id=i.session_id JOIN rollouts r ON r.id=s.rollout_id LEFT JOIN entries e ON e.rollout_id=s.rollout_id AND e.id=i.current_queue_entry_id WHERE i.session_id=? AND i.id=? AND s.ready=1 AND r.status='active'`, id, key).Scan(&raw, &accepted, &receipt.State, &receipt.EntryID)
	if errors.Is(err, sql.ErrNoRows) {
		return receipt, core.ErrNotFound
	}
	if err != nil {
		return receipt, err
	}
	st.rows.Add(1)
	var entry core.Entry
	if err = decodeValue(ctx, tx, raw, entry12, &entry); err != nil {
		return receipt, err
	}
	if entry.Input == nil || entry.Input.ID != key || entry.SessionID != id {
		return receipt, core.ErrCorrupt
	}
	receipt.InputRecord = *entry.Input
	receipt.AcceptedAt, err = time.Parse(time.RFC3339Nano, accepted)
	return receipt, err
}
