package integration_test

import (
	"context"
	"errors"

	"path/filepath"
	"reflect"

	"testing"

	astral "github.com/oines/astral-core"

	"github.com/oines/astral-core/store/memory"
	"github.com/oines/astral-core/store/sqlite"
)

func storeHeader() []astral.Entry {
	return []astral.Entry{
		{ID: "created", Kind: astral.EntryCreated, Created: &astral.CreationRecord{FormatVersion: astral.FormatVersion, SessionID: "session", RolloutID: "session"}},
		{ID: "config", Kind: astral.EntryConfiguration, Configuration: &astral.Configuration{Selection: astral.ModelSelection{ID: "model"}}},
	}
}
func storeMessage(id, parent string) astral.Entry {
	m := astral.User(id)
	return astral.Entry{ID: id, ParentID: parent, Kind: astral.EntryMessage, Message: &m}
}

func TestStoreConformance(t *testing.T) {
	factories := map[string]func(*testing.T) astral.Store{
		"sqlite": func(t *testing.T) astral.Store {
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "history.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			return store
		},
		"memory": func(*testing.T) astral.Store { return memory.New() },
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			s := factory(t)
			ctx := context.Background()
			if _, err := readStore(ctx, s, "session"); !errors.Is(err, astral.ErrNotFound) {
				t.Fatalf("absent: %v", err)
			}
			batch := storeHeader()
			if err := appendStore(ctx, s, "session", 0, batch); err != nil {
				t.Fatal(err)
			}
			batch[1].Configuration.Selection.ID = "mutated"
			r, err := readStore(ctx, s, "session")
			if err != nil {
				t.Fatal(err)
			}
			if r.Version != 1 || len(r.Entries) != 2 || r.Entries[1].Configuration.Selection.ID != "model" {
				t.Fatalf("append isolation: %+v", r)
			}
			r.Entries[1].Configuration.Selection.ID = "mutated read"
			original, err := readStore(ctx, s, "session")
			if err != nil {
				t.Fatal(err)
			}
			if original.Entries[1].Configuration.Selection.ID != "model" {
				t.Fatal("read alias")
			}
			if err = appendStore(ctx, s, "session", 0, []astral.Entry{storeMessage("conflict", "config")}); !errors.Is(err, astral.ErrConflict) {
				t.Fatalf("CAS: %v", err)
			}
			bad := []astral.Entry{storeMessage("one", "config"), storeMessage("two", "missing-parent")}
			if err = appendStore(ctx, s, "session", 1, bad); err == nil {
				t.Fatal("invalid batch accepted")
			}
			after, err := readStore(ctx, s, "session")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(original, after) {
				t.Fatal("partial batch committed")
			}
			if err = appendStore(ctx, s, "session", 1, []astral.Entry{storeMessage("one", "config"), storeMessage("two", "one")}); err != nil {
				t.Fatal(err)
			}
			after, err = readStore(ctx, s, "session")
			if err != nil {
				t.Fatal(err)
			}
			if after.Version != 2 || len(after.Entries) != 4 {
				t.Fatalf("batch version: %+v", after)
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if err = appendStore(cancelled, s, "session", 2, []astral.Entry{storeMessage("cancelled", "two")}); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel: %v", err)
			}
		})
	}
}

func TestContextRejectsMessageInsertedBeforeToolBatchFinishes(t *testing.T) {
	batch := storeHeader()
	toolCall := call("call", "work", `{}`)
	assistant := calls(toolCall).Message
	inv := astral.Invocation{SessionID: "session", AssistantID: "assistant", ExecutionID: "execution", Call: toolCall}
	result := astral.ToolResult{Content: astral.Text("done"), Outcome: astral.OutcomeSucceeded}
	batch = append(batch, astral.Entry{ID: "assistant", ParentID: "config", Kind: astral.EntryMessage, Message: &assistant}, storeMessage("interleaved", "assistant"), astral.Entry{ID: "result", ParentID: "interleaved", Kind: astral.EntryToolResult, Tool: &astral.ToolRecord{Invocation: inv, Result: &result}})
	store := memory.New()
	if err := appendStore(context.Background(), store, "session", 0, batch); !errors.Is(err, astral.ErrIncompleteBatch) {
		t.Fatalf("malformed chronology accepted: %v", err)
	}
	if _, err := store.LoadState(context.Background(), "session"); !errors.Is(err, astral.ErrNotFound) {
		t.Fatalf("partial invalid batch: %v", err)
	}
}
