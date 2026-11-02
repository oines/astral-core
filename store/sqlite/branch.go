package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/internal/branchdelete"
)

// DeleteBranch keeps planning, dependency validation and all removals inside
// one immediate transaction. Other handles cannot publish a Fork or Commit
// between the selected tree snapshot and its deletion.
func (st *Store) DeleteBranch(ctx context.Context, request core.BranchDeleteRequest) (core.BranchDeleteResult, error) {
	if err := branchdelete.Validate(request); err != nil {
		return core.BranchDeleteResult{}, err
	}
	if request.SessionID != "" {
		unlock, err := st.lockFork(ctx, request.SessionID)
		if err != nil {
			return core.BranchDeleteResult{}, err
		}
		defer unlock()
	}
	st.write.Lock()
	defer st.write.Unlock()
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return core.BranchDeleteResult{}, err
	}
	defer tx.Rollback()
	rollout := request.RolloutID
	if request.SessionID != "" {
		err = tx.QueryRowContext(ctx, `SELECT rollout_id FROM sessions WHERE id=?`, request.SessionID).Scan(&rollout)
		if errors.Is(err, sql.ErrNoRows) {
			return core.BranchDeleteResult{}, nil
		}
		if err != nil {
			return core.BranchDeleteResult{}, err
		}
	}
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM rollouts WHERE id=?`, rollout).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return core.BranchDeleteResult{RolloutID: rollout}, nil
	}
	if err != nil {
		return core.BranchDeleteResult{}, err
	}
	states := map[string]core.RuntimeState{}
	rows, err := tx.QueryContext(ctx, `SELECT id,state FROM sessions WHERE rollout_id=?`, rollout)
	if err != nil {
		return core.BranchDeleteResult{}, err
	}
	for rows.Next() {
		var id string
		var raw []byte
		var state core.RuntimeState
		if err = rows.Scan(&id, &raw); err == nil {
			err = branchStructure(raw, &state)
		}
		if err != nil || state.SessionID != id || state.RolloutID != rollout {
			rows.Close()
			if err == nil {
				err = core.ErrCorrupt
			}
			return core.BranchDeleteResult{}, err
		}
		states[id] = state
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return core.BranchDeleteResult{}, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT id,parent_id,session_id,projection IS NOT NULL FROM entries WHERE rollout_id=?`, rollout)
	if err != nil {
		return core.BranchDeleteResult{}, err
	}
	var nodes []branchdelete.Node
	for rows.Next() {
		var node branchdelete.Node
		if err = rows.Scan(&node.ID, &node.ParentID, &node.SessionID, &node.History); err != nil {
			rows.Close()
			return core.BranchDeleteResult{}, err
		}
		nodes = append(nodes, node)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return core.BranchDeleteResult{}, err
	}
	plan, err := branchdelete.Build(request, rollout, states, nodes)
	if err != nil {
		return core.BranchDeleteResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS branch_entries(id TEXT PRIMARY KEY); DELETE FROM branch_entries;
CREATE TEMP TABLE IF NOT EXISTS branch_sessions(id TEXT PRIMARY KEY); DELETE FROM branch_sessions;`); err != nil {
		return core.BranchDeleteResult{}, err
	}
	for id := range plan.Entries {
		if _, err = tx.ExecContext(ctx, `INSERT INTO branch_entries(id) VALUES(?)`, id); err != nil {
			return core.BranchDeleteResult{}, err
		}
	}
	for id := range plan.Sessions {
		if _, err = tx.ExecContext(ctx, `INSERT INTO branch_sessions(id) VALUES(?)`, id); err != nil {
			return core.BranchDeleteResult{}, err
		}
	}
	if !plan.Entire {
		if err = validateBranchReferences(ctx, tx, plan, states); err != nil {
			return core.BranchDeleteResult{}, err
		}
	}
	if err = payloadCandidates(ctx, tx); err != nil {
		return core.BranchDeleteResult{}, err
	}
	// Delete payload references by owner, retaining bodies shared with surviving
	// states, entries, attempts or independent detached Rollouts.
	for id := range plan.Sessions {
		if err = replacePayloadRefs(ctx, tx, rollout, "state", ownerKey(id), nil); err != nil {
			return core.BranchDeleteResult{}, err
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT session_id,id FROM attempts WHERE session_id IN (SELECT id FROM branch_sessions)`)
	if err != nil {
		return core.BranchDeleteResult{}, err
	}
	var attemptOwners [][2]string
	for rows.Next() {
		var owner [2]string
		if err = rows.Scan(&owner[0], &owner[1]); err != nil {
			rows.Close()
			return core.BranchDeleteResult{}, err
		}
		attemptOwners = append(attemptOwners, owner)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return core.BranchDeleteResult{}, err
	}
	for _, owner := range attemptOwners {
		if err = replacePayloadRefs(ctx, tx, rollout, "attempt", ownerKey(owner[0], owner[1]), nil); err != nil {
			return core.BranchDeleteResult{}, err
		}
	}
	for id := range plan.Entries {
		if err = replacePayloadRefs(ctx, tx, rollout, "entry", ownerKey(rollout, id), nil); err != nil {
			return core.BranchDeleteResult{}, err
		}
	}
	for _, table := range []string{"inputs", "pending", "receipts", "attempts"} {
		if _, err = tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE session_id IN (SELECT id FROM branch_sessions)`); err != nil {
			return core.BranchDeleteResult{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE id IN (SELECT id FROM branch_sessions)`); err != nil {
		return core.BranchDeleteResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM entries WHERE rollout_id=? AND id IN (SELECT id FROM branch_entries)`, rollout); err != nil {
		return core.BranchDeleteResult{}, err
	}
	if plan.Entire {
		if _, err = tx.ExecContext(ctx, `DELETE FROM rollouts WHERE id=?`, rollout); err != nil {
			return core.BranchDeleteResult{}, err
		}
	}
	if err = collectPayloads(ctx, tx); err != nil {
		return core.BranchDeleteResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return core.BranchDeleteResult{}, err
	}
	return plan.Result(), nil
}

// Schema 12 externalizes only conversation bodies and opaque JSON. All typed
// history references remain in Body, so maintenance need not hydrate large
// messages or model snapshots simply to inspect their dependency structure.
func branchStructure(raw []byte, value any) error {
	var pack packed12
	if json.Unmarshal(raw, &pack) != nil || len(pack.Body) == 0 || json.Unmarshal(pack.Body, value) != nil {
		return core.ErrCorrupt
	}
	return nil
}

func validateBranchReferences(ctx context.Context, tx *sql.Tx, plan branchdelete.Plan, states map[string]core.RuntimeState) error {
	for id, state := range states {
		if !plan.Sessions[id] {
			if err := plan.State(state); err != nil {
				return err
			}
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT payload,projection FROM entries WHERE rollout_id=? AND id NOT IN (SELECT id FROM branch_entries)`, plan.RolloutID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var payload, projection []byte
		var record core.Record
		if err = rows.Scan(&payload, &projection); err == nil {
			err = branchStructure(payload, &record.Entry)
		}
		if err == nil && len(projection) > 0 {
			err = branchStructure(projection, &record.Projection)
		}
		if err == nil {
			err = plan.Record(record)
		}
		if err != nil {
			rows.Close()
			return err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT payload FROM attempts WHERE rollout_id=? AND session_id NOT IN (SELECT id FROM branch_sessions)`, plan.RolloutID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw []byte
		var attempt core.ModelAttempt
		if err = rows.Scan(&raw); err == nil {
			err = branchStructure(raw, &attempt)
		}
		if err == nil {
			err = plan.Attempt(attempt)
		}
		if err != nil {
			rows.Close()
			return err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// Queue and input receipts point back to immutable entries even when their
	// text is separately frozen in the runtime's current execution state.
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM pending p WHERE p.rollout_id=? AND p.session_id NOT IN (SELECT id FROM branch_sessions) AND p.entry_id IN (SELECT id FROM branch_entries)
UNION ALL SELECT 1 FROM inputs i JOIN sessions s ON s.id=i.session_id WHERE s.rollout_id=? AND i.session_id NOT IN (SELECT id FROM branch_sessions) AND (i.current_queue_entry_id IN (SELECT id FROM branch_entries) OR i.entry_id IN (SELECT id FROM branch_entries)) LIMIT 1`, plan.RolloutID, plan.RolloutID).Scan(new(int))
	if err == nil {
		return core.ErrConflict
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

var _ core.BranchDeleter = (*Store)(nil)
