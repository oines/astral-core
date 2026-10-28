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
	var raw []byte
	err := st.db.QueryRowContext(ctx, `SELECT a.payload FROM attempts a JOIN sessions s ON s.id=a.session_id JOIN rollouts r ON r.id=s.rollout_id WHERE a.session_id=? AND a.id=? AND s.ready=1 AND r.status='active'`, id, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ModelAttempt{}, core.ErrNotFound
	}
	if err != nil {
		return core.ModelAttempt{}, err
	}
	st.rows.Add(1)
	var attempt core.ModelAttempt
	if json.Unmarshal(raw, &attempt) != nil || attempt.ID != key || attempt.SessionID != id {
		return attempt, core.ErrCorrupt
	}
	return attempt, nil
}

func (st *Store) ScanAttempts(ctx context.Context, id string, q core.AttemptQuery) (core.AttemptPage, error) {
	if _, err := st.LoadState(ctx, id); err != nil {
		return core.AttemptPage{}, err
	}
	limit, maxBytes := core.PageLimits(q.Limit, q.MaxBytes)
	query := "SELECT payload FROM attempts WHERE session_id=?"
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
	rows, err := st.db.QueryContext(ctx, query, args...)
	if err != nil {
		return core.AttemptPage{}, err
	}
	defer rows.Close()
	page, size := core.AttemptPage{}, 0
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return core.AttemptPage{}, err
		}
		st.rows.Add(1)
		if len(page.Attempts) == limit || size+len(raw) > maxBytes {
			if len(page.Attempts) == 0 {
				return core.AttemptPage{}, core.ErrBudget
			}
			page.Next = strconv.FormatUint(page.Attempts[len(page.Attempts)-1].Version, 10)
			break
		}
		var attempt core.ModelAttempt
		if json.Unmarshal(raw, &attempt) != nil || attempt.SessionID != id {
			return core.AttemptPage{}, core.ErrCorrupt
		}
		page.Attempts = append(page.Attempts, attempt)
		size += len(raw)
	}
	return page, rows.Err()
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
			if json.Unmarshal(raw, &previous) != nil {
				return core.ErrCorrupt
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
			if json.Unmarshal(projection, &head) != nil || !head.Complete() || head.ConfigurationID != attempt.Input.ConfigurationID {
				return core.ErrCorrupt
			}
		}
		raw, err = json.Marshal(attempt)
		if err != nil {
			return err
		}
		if previous == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO attempts(session_id,id,rollout_id,version,purpose,payload) VALUES(?,?,?,?,?,?)", id, attempt.ID, rolloutID, attempt.Version, attempt.Purpose, raw)
		} else {
			_, err = tx.ExecContext(ctx, "UPDATE attempts SET payload=? WHERE session_id=? AND id=?", raw, id, attempt.ID)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
