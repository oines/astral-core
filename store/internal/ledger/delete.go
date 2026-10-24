package ledger

import (
	"context"

	core "github.com/oines/astral-core"
)

func (st *Store) DeleteSession(ctx context.Context, id string) error {
	unlock, err := st.LockFork(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if err = ctx.Err(); err != nil {
		return err
	}
	data := st.Sessions[id]
	if data == nil {
		return nil
	}
	for otherID, other := range st.Sessions {
		if otherID != id && other.State.RolloutID == data.State.RolloutID {
			return core.ErrConflict
		}
	}
	delete(st.Rollouts, data.State.RolloutID)
	delete(st.Sessions, id)
	return nil
}
