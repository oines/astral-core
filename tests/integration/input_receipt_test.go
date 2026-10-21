package integration_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	core "github.com/oines/astral-core"
)

func TestInputReceiptsRemainIndexedFrozenAndIdempotentAfterReopen(t *testing.T) {
	for _, kind := range []core.InputKind{core.InputContext, core.InputSteer, core.InputFollowUp} {
		t.Run(string(kind), func(t *testing.T) {
			eachStore(t, func(t *testing.T, underlying core.Store) {
				ctx := context.Background()
				// The queue write succeeds but its acknowledgement is lost.
				store := &faultStore{Store: underlying, kind: core.EntryInputQueued, after: true}
				m := model(func(context.Context, int, core.Request, func(core.ModelEvent)) (core.Reply, error) {
					return answer("done"), nil
				})
				c := testConfig(m, store)
				s := newSession(t, newSessions(t, c), core.SessionOptions{})
				input := core.Input{ID: "stable", Contexts: []core.Message{snapshotMessage("frozen context"), snapshotMessage("second context")}}
				if kind != core.InputContext {
					input.Message = core.User("frozen driver")
				}
				enqueue := func(session *core.Session, input core.Input) (core.InputReceipt, error) {
					switch kind {
					case core.InputContext:
						return session.InjectContext(ctx, input)
					case core.InputSteer:
						return session.Steer(ctx, input)
					default:
						return session.FollowUp(ctx, input)
					}
				}
				accepted, err := enqueue(s, input)
				if err != nil || accepted.AcceptedAt.IsZero() || accepted.Digest == "" || len(m.observed()) != 0 {
					t.Fatalf("admission lost or activated: %+v %v", accepted, err)
				}
				if kind != core.InputContext {
					run, err := s.Continue(ctx)
					if err != nil {
						t.Fatal(err)
					}
					waitOK(t, run)
				}
				applied, err := s.LookupInput(ctx, input.ID)
				if err != nil || applied.State != core.InputApplied || applied.EntryID == "" || !applied.AcceptedAt.Equal(accepted.AcceptedAt) {
					t.Fatalf("application receipt incomplete: %+v %v", applied, err)
				}
				entry, err := s.History().Get(ctx, applied.EntryID)
				wanted := "frozen driver"
				if kind == core.InputContext {
					wanted = "frozen context"
				}
				if err != nil || entry.InputID != input.ID || entry.Message == nil || entry.Message.Content[0].Text != wanted {
					t.Fatalf("receipt does not point at its committed input: %+v %v", entry, err)
				}
				if err = s.Close(ctx); err != nil {
					t.Fatal(err)
				}
				c.Store = underlying
				reopened, err := core.OpenSession(ctx, c, s.ID())
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close(ctx)
				version, calls := reopened.State().Version, len(m.observed())
				again, err := enqueue(reopened, input)
				if err != nil || !reflect.DeepEqual(applied, again) || reopened.State().Version != version || len(m.observed()) != calls {
					t.Fatalf("duplicate changed persisted input or activated work: %+v %v", again, err)
				}
				again.Contexts[0].Content[0].Text = "mutated receipt"
				frozen, err := reopened.LookupInput(ctx, input.ID)
				if err != nil || frozen.Contexts[0].Content[0].Text != "frozen context" {
					t.Fatalf("receipt caller mutated frozen payload: %+v %v", frozen, err)
				}
				input.Contexts[0].Content[0].Text = "changed content"
				if _, err = enqueue(reopened, input); !errors.Is(err, core.ErrConflict) {
					t.Fatalf("changed duplicate accepted: %v", err)
				}
			})
		})
	}
}
