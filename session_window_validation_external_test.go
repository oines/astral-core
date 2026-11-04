package astral_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/memory"
)

// A custom Store can return JSON-decodable history that violates the message
// contract. Its isolated returned records must be checked before Session use.
type invalidWindowReadStore struct {
	core.Store
	invalid bool
}

func (s *invalidWindowReadStore) change(record *core.Record) {
	if s.invalid && record.Entry.Kind == core.EntryContext {
		record.Entry.Message.Content[0].Type = ""
	}
}

func (s *invalidWindowReadStore) Scan(ctx context.Context, rollout string, query core.Query) (core.Page, error) {
	page, err := s.Store.Scan(ctx, rollout, query)
	for i := range page.Records {
		s.change(&page.Records[i])
	}
	return page, err
}

func (s *invalidWindowReadStore) Get(ctx context.Context, rollout, id string) (core.Record, error) {
	record, err := s.Store.Get(ctx, rollout, id)
	if err == nil {
		s.change(&record)
	}
	return record, err
}

func TestSessionWindowLoadingRejectsCustomStoreInvalidBody(t *testing.T) {
	for _, operation := range []string{"reopen", "reload", "shared-fork", "detached-fork"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			store := &invalidWindowReadStore{Store: memory.New()}
			config := core.Config{Model: &contractModel{}, Store: store, EstimateTokens: func(context.Context, core.Model, core.Request) (int, error) { return 100, nil }}
			s, err := core.NewSession(ctx, config, core.SessionOptions{ID: "parent"})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(ctx)
			if _, err = s.InjectContext(ctx, core.Input{ID: "context", Contexts: []core.Message{core.User("observation")}}); err != nil {
				t.Fatal(err)
			}
			before, err := s.ReadContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			state := s.State()
			store.invalid = true
			if _, err := core.ReadContext(ctx, store, s.RolloutID(), s.Head(), core.DefaultContextBytes); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("public store projection stopped validating: %v", err)
			}
			switch operation {
			case "reopen":
				_, err = core.OpenSession(ctx, config, s.ID())
			case "reload":
				err = s.Reload(ctx)
			case "shared-fork", "detached-fork":
				mode := core.ForkShared
				if operation == "detached-fork" {
					mode = core.ForkDetached
				}
				_, err = s.Fork(ctx, s.Head(), core.ForkOptions{ID: "child", Mode: mode})
				if _, lookupErr := store.LoadState(ctx, "child"); !errors.Is(lookupErr, core.ErrNotFound) {
					t.Fatalf("invalid fork was published: %v", lookupErr)
				}
			}
			if !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("invalid body admitted by %s: %v", operation, err)
			}
			after, err := s.ReadContext(ctx)
			if err != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(state, s.State()) {
				t.Fatalf("failed loading changed owned history/state: %v", err)
			}
		})
	}
}
