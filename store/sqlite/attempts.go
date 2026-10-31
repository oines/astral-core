package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	core "github.com/oines/astral-core"
)

func (st *Store) GetAttempt(ctx context.Context, id, key string) (core.ModelAttempt, error) {
	tx, err := st.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return core.ModelAttempt{}, err
	}
	defer tx.Rollback()
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT a.payload FROM attempts a JOIN sessions s ON s.id=a.session_id JOIN rollouts r ON r.id=s.rollout_id WHERE a.session_id=? AND a.id=? AND s.ready=1 AND r.status='active'`, id, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ModelAttempt{}, core.ErrNotFound
	}
	if err != nil {
		return core.ModelAttempt{}, err
	}
	st.rows.Add(1)
	var attempt core.ModelAttempt
	if err = decodeValue(ctx, tx, raw, attempt12, &attempt); err != nil {
		return attempt, err
	}
	if attempt.ID != key || attempt.SessionID != id {
		return attempt, core.ErrCorrupt
	}
	return attempt, nil
}

func (st *Store) ScanAttempts(ctx context.Context, id string, q core.AttemptQuery) (core.AttemptPage, error) {
	tx, err := st.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return core.AttemptPage{}, err
	}
	defer tx.Rollback()
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM sessions s JOIN rollouts r ON r.id=s.rollout_id WHERE s.id=? AND s.ready=1 AND r.status='active'`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return core.AttemptPage{}, core.ErrNotFound
	}
	if err != nil {
		return core.AttemptPage{}, err
	}
	limit, maxBytes := core.PageLimits(q.Limit, q.MaxBytes)
	query := "SELECT payload,payload_size FROM attempts WHERE session_id=?"
	args := []any{id}
	if q.Purpose != "" {
		query += " AND purpose=?"
		args = append(args, q.Purpose)
	}
	if q.Cursor != "" {
		cursor, err := strconv.ParseUint(q.Cursor, 10, 63)
		if err != nil {
			return core.AttemptPage{}, core.ErrInvalid
		}
		if q.Reverse {
			query += " AND version<?"
		} else {
			query += " AND version>?"
		}
		args = append(args, cursor)
	}
	query += " ORDER BY version"
	if q.Reverse {
		query += " DESC"
	}
	query += " LIMIT ?"
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return core.AttemptPage{}, err
	}
	defer rows.Close()
	page, size := core.AttemptPage{}, 0
	var values []packedValue
	more := false
	for rows.Next() {
		var raw []byte
		var logicalSize int
		if err := rows.Scan(&raw, &logicalSize); err != nil {
			return core.AttemptPage{}, err
		}
		st.rows.Add(1)
		if len(values) == limit || logicalSize > maxBytes-size {
			if len(values) == 0 {
				return core.AttemptPage{}, core.ErrBudget
			}
			more = true
			break
		}
		if logicalSize <= 0 {
			return core.AttemptPage{}, core.ErrCorrupt
		}
		values = append(values, packedValue{raw, attempt12})
		size += logicalSize
	}
	if err = rows.Err(); err != nil {
		return core.AttemptPage{}, err
	}
	if err = rows.Close(); err != nil {
		return core.AttemptPage{}, err
	}
	decoded, err := hydrate(ctx, tx, values...)
	if err != nil {
		return core.AttemptPage{}, err
	}
	for _, raw := range decoded {
		var attempt core.ModelAttempt
		if json.Unmarshal(raw, &attempt) != nil || attempt.SessionID != id {
			return core.AttemptPage{}, core.ErrCorrupt
		}
		page.Attempts = append(page.Attempts, attempt)
	}
	if more {
		page.Next = strconv.FormatUint(page.Attempts[len(page.Attempts)-1].Version, 10)
	}
	return page, nil
}

func saveAttempts(ctx context.Context, tx *sql.Tx, id, rolloutID string, mutations []core.AttemptMutation, entries []core.Entry) error {
	seen := map[string]bool{}
	for _, mutation := range mutations {
		if seen[mutation.ID] {
			return core.ErrCorrupt
		}
		seen[mutation.ID] = true
		var previous *core.ModelAttempt
		var raw []byte
		err := tx.QueryRowContext(ctx, "SELECT payload FROM attempts WHERE session_id=? AND id=?", id, mutation.ID).Scan(&raw)
		if err == nil {
			if err = decodeValue(ctx, tx, raw, attempt12, &previous); err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		attempt, err := core.ApplyAttemptMutation(previous, mutation, entries)
		if err != nil {
			return err
		}
		if attempt.SessionID != id || attempt.Input.RolloutID != rolloutID {
			return core.ErrCorrupt
		}
		if mutation.Create != nil {
			var projection []byte
			err = tx.QueryRowContext(ctx, "SELECT projection FROM entries WHERE rollout_id=? AND id=?", rolloutID, attempt.Input.HeadID).Scan(&projection)
			if err != nil {
				return core.ErrCorrupt
			}
			var head core.Projection
			if err = decodeValue(ctx, tx, projection, projection12, &head); err != nil {
				return err
			}
			if !head.Complete() || head.ConfigurationID != attempt.Input.ConfigurationID {
				return core.ErrCorrupt
			}
		}
		var logicalSize int
		raw, logicalSize, err = encodeValue(ctx, tx, rolloutID, "attempt", ownerKey(id, attempt.ID), attempt, attempt12)
		if err != nil {
			return err
		}
		if previous == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO attempts(session_id,id,rollout_id,version,purpose,payload,payload_size) VALUES(?,?,?,?,?,?,?)", id, attempt.ID, rolloutID, attempt.Version, attempt.Purpose, raw, logicalSize)
		} else {
			_, err = tx.ExecContext(ctx, "UPDATE attempts SET payload=?,payload_size=? WHERE session_id=? AND id=?", raw, logicalSize, id, attempt.ID)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
