package integration_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	astral "github.com/oines/astral-core"
	sessions "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/store/jsonl"
	"github.com/oines/astral-core/store/memory"
	"github.com/oines/astral-core/store/sqlite"
)

func storeHeader() []sessions.Entry {
	return []sessions.Entry{
		{ID: "created", Kind: sessions.EntryCreated, Created: &sessions.CreationRecord{FormatVersion: sessions.FormatVersion, SessionID: "session", RolloutID: "session"}},
		{ID: "config", Kind: sessions.EntryConfiguration, Configuration: &sessions.Configuration{ModelID: "model"}},
	}
}
func storeMessage(id, parent string) sessions.Entry {
	m := astral.User(id)
	return sessions.Entry{ID: id, ParentID: parent, Kind: sessions.EntryMessage, Message: &m}
}

func TestStoreConformance(t *testing.T) {
	factories := map[string]func(*testing.T) sessions.Store{
		"sqlite": func(t *testing.T) sessions.Store {
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "history.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			return store
		},
		"memory": func(*testing.T) sessions.Store { return memory.New() },
		"jsonl": func(t *testing.T) sessions.Store {
			s, err := jsonl.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			return s
		},
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			s := factory(t)
			ctx := context.Background()
			if _, err := readStore(ctx, s, "session"); !errors.Is(err, sessions.ErrNotFound) {
				t.Fatalf("absent: %v", err)
			}
			batch := storeHeader()
			if err := appendStore(ctx, s, "session", 0, batch); err != nil {
				t.Fatal(err)
			}
			batch[1].Configuration.ModelID = "mutated"
			r, err := readStore(ctx, s, "session")
			if err != nil {
				t.Fatal(err)
			}
			if r.Version != 1 || len(r.Entries) != 2 || r.Entries[1].Configuration.ModelID != "model" {
				t.Fatalf("append isolation: %+v", r)
			}
			r.Entries[1].Configuration.ModelID = "mutated read"
			original, err := readStore(ctx, s, "session")
			if err != nil {
				t.Fatal(err)
			}
			if original.Entries[1].Configuration.ModelID != "model" {
				t.Fatal("read alias")
			}
			if err = appendStore(ctx, s, "session", 0, []sessions.Entry{storeMessage("conflict", "config")}); !errors.Is(err, sessions.ErrConflict) {
				t.Fatalf("CAS: %v", err)
			}
			bad := []sessions.Entry{storeMessage("one", "config"), storeMessage("two", "missing-parent")}
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
			if err = appendStore(ctx, s, "session", 1, []sessions.Entry{storeMessage("one", "config"), storeMessage("two", "one")}); err != nil {
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
			if err = appendStore(cancelled, s, "session", 2, []sessions.Entry{storeMessage("cancelled", "two")}); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel: %v", err)
			}
		})
	}
}

func TestJSONLSeparateHandlesShareAtomicCompareAndAppend(t *testing.T) {
	directory := t.TempDir()
	first, err := jsonl.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err = os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	second, err := jsonl.Open(alias)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = appendStore(ctx, first, "session", 0, storeHeader()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	start := make(chan struct{})
	for i, store := range []*jsonl.Store{first, second} {
		wg.Add(1)
		go func(i int, s *jsonl.Store) {
			defer wg.Done()
			<-start
			errs <- appendStore(ctx, s, "session", 1, []sessions.Entry{storeMessage(string(rune('a'+i)), "config")})
		}(i, store)
	}
	close(start)
	wg.Wait()
	close(errs)
	success, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, sessions.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d", success, conflicts)
	}
	reopened, err := jsonl.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	r, err := readStore(ctx, reopened, "session")
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != 2 || len(r.Entries) != 3 {
		t.Fatalf("concurrent commits: %+v", r)
	}
}

func TestJSONLRecoversOnlyUnterminatedTail(t *testing.T) {
	directory := t.TempDir()
	s, err := jsonl.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = appendStore(ctx, s, "session", 0, storeHeader()); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(directory, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	filename := files[0]
	prefix, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err = appendStore(ctx, s, "session", 1, []sessions.Entry{storeMessage("lost", "config"), storeMessage("lost-too", "lost")}); err != nil {
		t.Fatal(err)
	}
	complete, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Truncate(filename, int64(len(prefix)+(len(complete)-len(prefix))/2)); err != nil {
		t.Fatal(err)
	}
	s, err = jsonl.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := readStore(ctx, s, "session")
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != 1 || len(r.Entries) != 2 {
		t.Fatal("partial commit exposed")
	}
	if err = appendStore(ctx, s, "session", 1, []sessions.Entry{storeMessage("replacement", "config")}); err != nil {
		t.Fatal(err)
	}
	recovered, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(recovered, prefix) || bytes.Contains(recovered, []byte("lost-too")) {
		t.Fatal("committed prefix changed or crash tail survived")
	}
	r, err = readStore(ctx, s, "session")
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != 2 || r.Entries[2].ID != "replacement" {
		t.Fatal("replacement missing")
	}
}

func TestJSONLRejectsCommittedCorruptionWithoutRepairingIt(t *testing.T) {
	directory := t.TempDir()
	s, err := jsonl.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = appendStore(ctx, s, "session", 0, storeHeader()); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(directory, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	filename := files[0]
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Replace(data, []byte(`"model"`), []byte(`"other"`), 1)
	if bytes.Equal(data, corrupt) {
		t.Fatal("fixture did not change")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filename, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = jsonl.Open(directory); !errors.Is(err, sessions.ErrCorrupt) {
		t.Fatalf("open corruption: %v", err)
	}
	after, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, corrupt) {
		t.Fatal("corrupt committed history rewritten")
	}
}

func TestContextRejectsMessageInsertedBeforeToolBatchFinishes(t *testing.T) {
	batch := storeHeader()
	toolCall := call("call", "work", `{}`)
	assistant := calls(toolCall).Message
	inv := astral.Invocation{SessionID: "session", AssistantID: "assistant", ExecutionID: "execution", Call: toolCall}
	result := astral.ToolResult{Content: astral.Text("done"), Outcome: astral.OutcomeSucceeded}
	batch = append(batch, sessions.Entry{ID: "assistant", ParentID: "config", Kind: sessions.EntryMessage, Message: &assistant}, storeMessage("interleaved", "assistant"), sessions.Entry{ID: "result", ParentID: "interleaved", Kind: sessions.EntryToolResult, Tool: &sessions.ToolRecord{Invocation: inv, Result: &result}})
	store := memory.New()
	if err := appendStore(context.Background(), store, "session", 0, batch); !errors.Is(err, astral.ErrIncompleteBatch) {
		t.Fatalf("malformed chronology accepted: %v", err)
	}
	if _, err := store.LoadState(context.Background(), "session"); !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("partial invalid batch: %v", err)
	}
}
