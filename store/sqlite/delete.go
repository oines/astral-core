package sqlite

import (
	"context"
	"database/sql"
	"errors"

	core "github.com/oines/astral-core"
)

// DeleteSession removes a privately owned Rollout after its host has stopped
// all users. Shared ancestry is never erased as a side effect of this action.
func (st *Store) DeleteSession(ctx context.Context, id string) error {
	unlock, err := st.lockFork(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	st.write.Lock()
	defer st.write.Unlock()
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var rollout string
	err = tx.QueryRowContext(ctx, "SELECT rollout_id FROM sessions WHERE id=?", id).Scan(&rollout)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var shared int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE rollout_id=? AND id<>?", rollout, id).Scan(&shared); err != nil {
		return err
	}
	if shared != 0 {
		return core.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM inputs WHERE session_id=?", id); err != nil {
		return err
	}
	for _, table := range []string{"entries", "pending", "receipts", "attempts", "sessions"} {
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE rollout_id=?", rollout); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM rollouts WHERE id=?", rollout); err != nil {
		return err
	}
	if err = removeRolloutPayloads(ctx, tx, rollout); err != nil {
		return err
	}
	return tx.Commit()
}

var _ core.SessionDeleter = (*Store)(nil)

func discardEntryPage(ctx context.Context, tx *sql.Tx, rollout string) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM entries WHERE rollout_id=? LIMIT ?`, rollout, core.DefaultPageSize)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if err = payloadCandidates(ctx, tx); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err = replacePayloadRefs(ctx, tx, rollout, "entry", ownerKey(rollout, id), nil); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM entries WHERE rollout_id=? AND id=?`, rollout, id); err != nil {
			return 0, err
		}
	}
	return len(ids), collectPayloads(ctx, tx)
}
