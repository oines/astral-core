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
	"github.com/oines/astral-core/store/jsonl"
	"github.com/oines/astral-core/store/memory"
)

func storeHeader() []astral.Entry {
	return []astral.Entry{
		{ID: "created", Kind: astral.EntryCreated, Created: &astral.CreationRecord{FormatVersion: 1, SessionID: "session"}},
		{ID: "config", Kind: astral.EntryConfiguration, Configuration: &astral.Configuration{ModelID: "model"}},
	}
}
func storeMessage(id, parent string) astral.Entry {
	m := astral.User(id)
	return astral.Entry{ID: id, ParentID: parent, Kind: astral.EntryMessage, Message: &m}
}

func TestStoreConformance(t *testing.T) {
	factories := map[string]func(*testing.T) astral.Store{
		"memory": func(*testing.T) astral.Store { return memory.New() },
		"jsonl": func(t *testing.T) astral.Store {
			s, err := jsonl.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			s := factory(t)
			ctx := context.Background()
			if _, err := s.Read(ctx, "session"); !errors.Is(err, astral.ErrNotFound) {
				t.Fatalf("absent: %v", err)
			}
			batch := storeHeader()
			if err := s.Append(ctx, "session", 0, batch); err != nil {
				t.Fatal(err)
			}
			batch[1].Configuration.ModelID = "mutated"
			r, err := s.Read(ctx, "session")
			if err != nil {
				t.Fatal(err)
			}
			if r.Version != 1 || len(r.Entries) != 2 || r.Entries[1].Configuration.ModelID != "model" {
				t.Fatalf("append isolation: %+v", r)
			}
			r.Entries[1].Configuration.ModelID = "mutated read"
			original, err := s.Read(ctx, "session")
			if err != nil {
				t.Fatal(err)
			}
			if original.Entries[1].Configuration.ModelID != "model" {
				t.Fatal("read alias")
			}
			if err = s.Append(ctx, "session", 0, []astral.Entry{storeMessage("conflict", "config")}); !errors.Is(err, astral.ErrConflict) {
				t.Fatalf("CAS: %v", err)
			}
			bad := []astral.Entry{storeMessage("one", "config"), storeMessage("two", "missing-parent")}
			if err = s.Append(ctx, "session", 1, bad); err == nil {
				t.Fatal("invalid batch accepted")
			}
			after, err := s.Read(ctx, "session")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(original, after) {
				t.Fatal("partial batch committed")
			}
			if err = s.Append(ctx, "session", 1, []astral.Entry{storeMessage("one", "config"), storeMessage("two", "one")}); err != nil {
				t.Fatal(err)
			}
			after, err = s.Read(ctx, "session")
			if err != nil {
				t.Fatal(err)
			}
			if after.Version != 2 || len(after.Entries) != 4 {
				t.Fatalf("batch version: %+v", after)
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if err = s.Append(cancelled, "session", 2, []astral.Entry{storeMessage("cancelled", "two")}); !errors.Is(err, context.Canceled) {
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
	if err = first.Append(ctx, "session", 0, storeHeader()); err != nil {
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
			errs <- s.Append(ctx, "session", 1, []astral.Entry{storeMessage(string(rune('a'+i)), "config")})
		}(i, store)
	}
	close(start)
	wg.Wait()
	close(errs)
	success, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, astral.ErrConflict) {
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
	r, err := reopened.Read(ctx, "session")
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
	if err = s.Append(ctx, "session", 0, storeHeader()); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(directory, files[0].Name())
	prefix, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Append(ctx, "session", 1, []astral.Entry{storeMessage("lost", "config"), storeMessage("lost-too", "lost")}); err != nil {
		t.Fatal(err)
	}
	complete, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Truncate(filename, int64(len(prefix)+(len(complete)-len(prefix))/2)); err != nil {
		t.Fatal(err)
	}
	r, err := s.Read(ctx, "session")
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != 1 || len(r.Entries) != 2 {
		t.Fatal("partial commit exposed")
	}
	if err = s.Append(ctx, "session", 1, []astral.Entry{storeMessage("replacement", "config")}); err != nil {
		t.Fatal(err)
	}
	recovered, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(recovered, prefix) || bytes.Contains(recovered, []byte("lost-too")) {
		t.Fatal("committed prefix changed or crash tail survived")
	}
	r, err = s.Read(ctx, "session")
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
	if err = s.Append(ctx, "session", 0, storeHeader()); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(directory, files[0].Name())
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Replace(data, []byte(`"model"`), []byte(`"other"`), 1)
	if bytes.Equal(data, corrupt) {
		t.Fatal("fixture did not change")
	}
	if err = os.WriteFile(filename, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Read(ctx, "session"); !errors.Is(err, astral.ErrCorrupt) {
		t.Fatalf("read corruption: %v", err)
	}
	if err = s.Append(ctx, "session", 1, []astral.Entry{storeMessage("one", "config")}); !errors.Is(err, astral.ErrCorrupt) {
		t.Fatalf("append corruption: %v", err)
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
	batch = append(batch, astral.Entry{ID: "assistant", ParentID: "config", Kind: astral.EntryMessage, Message: &assistant}, storeMessage("interleaved", "assistant"), astral.Entry{ID: "result", ParentID: "interleaved", Kind: astral.EntryToolResult, Tool: &astral.ToolRecord{Invocation: inv, Result: &result}})
	store := memory.New()
	if err := store.Append(context.Background(), "session", 0, batch); err != nil {
		t.Fatal(err)
	}
	m := model(func(context.Context, int, astral.Request, func(astral.ModelEvent)) (astral.Reply, error) {
		return answer("unexpected"), nil
	})
	m.spec.ID = "model"
	s, err := newAgent(t, testConfig(m, store)).OpenSession(context.Background(), "session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.InspectContext(context.Background()); !errors.Is(err, astral.ErrIncompleteBatch) {
		t.Fatalf("malformed chronology silently repaired: %v", err)
	}
	if len(m.observed()) != 0 {
		t.Fatal("invalid request streamed")
	}
}
