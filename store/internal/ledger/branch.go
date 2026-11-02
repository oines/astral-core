package ledger

import (
	"context"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/internal/branchdelete"
)

func (st *Store) DeleteBranch(ctx context.Context, request core.BranchDeleteRequest) (core.BranchDeleteResult, error) {
	if err := branchdelete.Validate(request); err != nil {
		return core.BranchDeleteResult{}, err
	}
	if request.SessionID != "" {
		unlock, err := st.LockFork(ctx, request.SessionID)
		if err != nil {
			return core.BranchDeleteResult{}, err
		}
		defer unlock()
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if err := ctx.Err(); err != nil {
		return core.BranchDeleteResult{}, err
	}
	rolloutID := request.RolloutID
	if request.SessionID != "" {
		data := st.Sessions[request.SessionID]
		if data == nil {
			return core.BranchDeleteResult{}, nil
		}
		rolloutID = data.State.RolloutID
	}
	rollout := st.Rollouts[rolloutID]
	if rollout == nil {
		return core.BranchDeleteResult{RolloutID: rolloutID}, nil
	}
	states := map[string]core.RuntimeState{}
	for id, data := range st.Sessions {
		if data.State.RolloutID == rolloutID {
			states[id] = data.State
		}
	}
	nodes := make([]branchdelete.Node, 0, len(rollout.Records))
	for _, record := range rollout.Records {
		nodes = append(nodes, branchdelete.Node{ID: record.Entry.ID, ParentID: record.Entry.ParentID, SessionID: record.Entry.SessionID, History: record.Projection != nil})
	}
	plan, err := branchdelete.Build(request, rolloutID, states, nodes)
	if err != nil {
		return core.BranchDeleteResult{}, err
	}
	if !plan.Entire {
		for id, state := range states {
			if plan.Sessions[id] {
				continue
			}
			if err := plan.State(state); err != nil {
				return core.BranchDeleteResult{}, err
			}
			data := st.Sessions[id]
			for _, attempt := range data.Attempts {
				if err := plan.Attempt(attempt); err != nil {
					return core.BranchDeleteResult{}, err
				}
			}
			for _, record := range data.Pending {
				if err := plan.References(record.Entry.ID); err != nil {
					return core.BranchDeleteResult{}, err
				}
				if err := plan.Record(record); err != nil {
					return core.BranchDeleteResult{}, err
				}
			}
			for _, input := range data.Inputs {
				if err := plan.References(input.EntryID); err != nil {
					return core.BranchDeleteResult{}, err
				}
			}
		}
		for id, record := range rollout.Records {
			if !plan.Entries[id] {
				if err := plan.Record(record); err != nil {
					return core.BranchDeleteResult{}, err
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return core.BranchDeleteResult{}, err
	}
	if plan.Entire {
		delete(st.Rollouts, rolloutID)
	} else {
		kept := NewRollout(rolloutID)
		kept.Status, kept.Sequence = rollout.Status, rollout.Sequence
		for _, id := range rollout.Order {
			if !plan.Entries[id] {
				kept.Add(rollout.Records[id])
			}
		}
		st.Rollouts[rolloutID] = kept
	}
	for id := range plan.Sessions {
		delete(st.Sessions, id)
	}
	return plan.Result(), nil
}

var _ core.BranchDeleter = (*Store)(nil)
