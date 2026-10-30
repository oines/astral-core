package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/oines/astral-core"
)

func payloadStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "core.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	payloadCommit(t, st, "s", []core.Entry{
		{ID: "created", Kind: core.EntryCreated, Created: &core.CreationRecord{FormatVersion: 11, SessionID: "s", RolloutID: "r"}},
		{ID: "config", Kind: core.EntryConfiguration, Configuration: &core.Configuration{Selection: core.ModelSelection{ID: "model"}}},
	}, nil)
	return st
}
func payloadCommit(t *testing.T, st *Store, id string, entries []core.Entry, attempts []core.AttemptMutation) core.Receipt {
	t.Helper()
	state, err := st.LoadState(context.Background(), id)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	for i := range entries {
		entries[i].SessionID = id
		entries[i].Version = state.Version + 1
		entries[i].Time = time.Unix(100+int64(state.Version), 0).UTC()
	}
	r, err := st.Commit(context.Background(), id, core.Commit{ID: fmt.Sprintf("commit-%d", state.Version), ExpectedVersion: state.Version, Entries: entries, Attempts: attempts})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func payloadCount(t *testing.T, st *Store) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM payloads`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPayloadSharingQueueRevisionAndDetachedOwnership(t *testing.T) {
	ctx := context.Background()
	st := payloadStore(t)
	text := strings.Repeat("你好<&>🌙", 1200)
	message := core.User(text)
	message.Metadata = json.RawMessage(`{"source":"first"}`)
	input := core.InputRecord{ID: "input", Kind: core.InputSteer, Digest: strings.Repeat("a", 64), Message: &message}
	payloadCommit(t, st, "s", []core.Entry{{ID: "queued", Kind: core.EntryInputQueued, Input: &input}}, nil)
	first, err := st.LookupInput(ctx, "s", "input")
	if err != nil {
		t.Fatal(err)
	}
	changed := input
	changed.Message = &core.Message{Role: core.RoleUser, Content: core.Text(text), Metadata: json.RawMessage(`{"source":"edited"}`)}
	payloadCommit(t, st, "s", []core.Entry{{ID: "clear", Kind: core.EntryInputCleared, Input: &input}, {ID: "requeued", Kind: core.EntryInputRequeued, Input: &changed}}, nil)
	receipt, err := st.LookupInput(ctx, "s", "input")
	if err != nil || !receipt.AcceptedAt.Equal(first.AcceptedAt) || receipt.Digest != first.Digest || !reflect.DeepEqual(receipt.Message, changed.Message) {
		t.Fatalf("queue revision: %+v %v", receipt, err)
	}
	page, err := st.Pending(ctx, "s", core.PendingQuery{})
	if err != nil || len(page.Records) != 1 || page.Records[0].Entry.ID != "requeued" {
		t.Fatalf("pending revision: %+v %v", page, err)
	}
	if _, err = st.Pending(ctx, "s", core.PendingQuery{MaxBytes: 1024}); !errors.Is(err, core.ErrBudget) {
		t.Fatalf("budget used reference size: %v", err)
	}
	payloadCommit(t, st, "s", []core.Entry{{ID: "message", Kind: core.EntryMessage, ParentID: "config", InputID: input.ID, Message: changed.Message}, {ID: "consumed", Kind: core.EntryInputConsumed, Input: &changed}}, nil)
	if n := payloadCount(t, st); n != 1 {
		t.Fatalf("repeated content stored %d times", n)
	}
	request := core.ForkRequest{SourceID: "s", SourceRolloutID: "r", HeadID: "message", TargetID: "child", Mode: core.ForkDetached}
	child, err := st.Fork(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if n := payloadCount(t, st); n != 1 {
		t.Fatalf("fork copied payload: %d", n)
	}
	if err = st.DeleteSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	r, err := st.Get(ctx, child.RolloutID, "message")
	if err != nil || r.Entry.Message.Content[0].Text != text {
		t.Fatalf("deleted shared bytes: %v", err)
	}
	r.Entry.Message.Content[0].Text = "caller mutation"
	r, err = st.Get(ctx, child.RolloutID, "message")
	if err != nil || r.Entry.Message.Content[0].Text != text {
		t.Fatal("returned value aliases stored data")
	}
	if err = st.DeleteSession(ctx, "child"); err != nil {
		t.Fatal(err)
	}
	if n := payloadCount(t, st); n != 0 {
		t.Fatalf("unreachable payloads remain: %d", n)
	}
}

func TestPayloadAttemptAdoptionAndAtomicRollback(t *testing.T) {
	ctx := context.Background()
	st := payloadStore(t)
	base, err := core.ReadContext(ctx, st, "r", "config", core.DefaultContextBytes)
	if err != nil {
		t.Fatal(err)
	}
	input, err := core.FreezeRequest("r", base, base.Request, core.ModelSpec{ID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	attempt := core.ModelAttempt{ID: "a", SessionID: "s", Purpose: "compaction", Number: 1, Version: 2, StartedAt: time.Now().UTC(), Input: input}
	payloadCommit(t, st, "s", nil, []core.AttemptMutation{{ID: "a", Create: &attempt}})
	reply := core.Reply{Message: core.Message{Role: core.RoleAssistant, Content: core.Text(strings.Repeat("summary", 2000))}}
	reply.Message.Metadata = json.RawMessage(`"` + strings.Repeat("temporary metadata", 500) + `"`)
	payloadCommit(t, st, "s", nil, []core.AttemptMutation{{ID: "a", Finish: &core.AttemptOutcome{Status: "completed", FinishedAt: time.Now().UTC(), Reply: &reply}}})
	if n := payloadCount(t, st); n != 2 {
		t.Fatalf("attempt payloads: %d", n)
	}
	if _, err = st.ScanAttempts(ctx, "s", core.AttemptQuery{MaxBytes: 1024}); !errors.Is(err, core.ErrBudget) {
		t.Fatalf("attempt ignored hydrated budget: %v", err)
	}
	payloadCommit(t, st, "s", []core.Entry{{ID: "summary", Kind: core.EntryCompaction, ParentID: "config", Compaction: &core.CompactionRecord{AttemptID: "a", ThroughID: "config", Summary: reply.Message.Content[0].Text, ModelID: "model"}}}, []core.AttemptMutation{{ID: "a", AdoptEntryID: "summary"}})
	if n := payloadCount(t, st); n != 1 {
		t.Fatalf("temporary metadata not collected: %d", n)
	}
	got, err := st.Get(ctx, "r", "summary")
	if err != nil || got.Entry.Compaction.Summary != reply.Message.Content[0].Text {
		t.Fatalf("adoption deleted shared summary: %v", err)
	}
	state, err := st.LoadState(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	newMessage := core.User(strings.Repeat("rollback", 2000))
	_, err = st.Commit(ctx, "s", core.Commit{ID: "bad", ExpectedVersion: state.Version, Entries: []core.Entry{{ID: "bad-message", SessionID: "s", ParentID: "summary", Kind: core.EntryMessage, Message: &newMessage}}, Attempts: []core.AttemptMutation{{ID: "missing", AdoptEntryID: "bad-message"}}})
	if err == nil {
		t.Fatal("invalid commit succeeded")
	}
	if n := payloadCount(t, st); n != 1 {
		t.Fatalf("failed commit leaked payload: %d", n)
	}
}

func TestPayloadCorruptionIsNotEmptyContent(t *testing.T) {
	for _, mutation := range []string{`UPDATE payloads SET bytes=x'31'`, `PRAGMA foreign_keys=OFF; DELETE FROM payloads;`} {
		t.Run(mutation, func(t *testing.T) {
			st := payloadStore(t)
			message := core.User(strings.Repeat("data", 2000))
			payloadCommit(t, st, "s", []core.Entry{{ID: "message", Kind: core.EntryMessage, ParentID: "config", Message: &message}}, nil)
			if _, err := st.db.Exec(mutation); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Get(context.Background(), "r", "message"); !errors.Is(err, core.ErrCorrupt) {
				t.Fatalf("corruption hidden: %v", err)
			}
		})
	}
}

func TestCodec12OpaqueFieldsAndUnambiguousPaths(t *testing.T) {
	big := strings.Repeat("opaque", 1000)
	input := []byte(`{"message":{"role":"assistant","metadata":{"z":"` + big + `","large_integer":9007199254740993},"content":[{"type":"native","data":{"cipher":"` + big + `","refs":[{"hash":"not-a-reference"}]}},{"type":"text","text":"` + big + `"}]},"unknown_future":{"huge":"` + big + `"}}`)
	encoded, blobs, err := encode12(input, entry12)
	if err != nil {
		t.Fatal(err)
	}
	var pack packed12
	if err = json.Unmarshal(encoded, &pack); err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 3 {
		t.Fatalf("unexpected leaf coverage: %d", len(blobs))
	}
	raw, err := decode12(pack, entry12, blobs)
	if err != nil || !sameJSON12(raw, input) {
		t.Fatalf("round trip: %v", err)
	}
	var before, after struct {
		Message struct {
			Metadata json.RawMessage
			Content  []struct{ Data json.RawMessage }
		}
	}
	json.Unmarshal(input, &before)
	json.Unmarshal(raw, &after)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("opaque bytes changed")
	}
	pack.Refs = append(pack.Refs, pack.Refs[0])
	if _, err = decode12(pack, entry12, blobs); !errors.Is(err, core.ErrCorrupt) {
		t.Fatal("duplicate reference accepted")
	}
	if ownerKey("a/b", "c") == ownerKey("a", "b/c") {
		t.Fatal("ambiguous owner identity")
	}
}

// Every opaque Core field must have an explicit codec leaf. Future additions
// fail this check rather than silently restoring a large duplicate JSON field.
func TestCodec12CoversOpaqueCoreFields(t *testing.T) {
	rawType := reflect.TypeFor[json.RawMessage]()
	var visit func(reflect.Type, *field12, string)
	visit = func(typ reflect.Type, shape *field12, path string) {
		if typ == rawType {
			if shape == nil || shape.leaf != 'j' {
				t.Errorf("uncovered opaque field: %s", path)
			}
			return
		}
		if typ.Kind() == reflect.Pointer {
			visit(typ.Elem(), shape, path)
			return
		}
		if shape != nil && shape.leaf != 0 {
			return
		}
		switch typ.Kind() {
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				if !f.IsExported() {
					continue
				}
				key := strings.Split(f.Tag.Get("json"), ",")[0]
				if key == "-" {
					continue
				}
				var child *field12
				if shape != nil {
					child = shape.fields[key]
				}
				visit(f.Type, child, path+"."+key)
			}
		case reflect.Slice, reflect.Array:
			var child *field12
			if shape != nil {
				child = shape.items
			}
			visit(typ.Elem(), child, path+"[]")
		case reflect.Map:
			var child *field12
			if shape != nil {
				child = shape.values
			}
			visit(typ.Elem(), child, path+"{}")
		}
	}
	visit(reflect.TypeFor[core.Entry](), entry12, "Entry")
	visit(reflect.TypeFor[core.Projection](), projection12, "Projection")
	visit(reflect.TypeFor[core.RuntimeState](), state12, "State")
	visit(reflect.TypeFor[core.ModelAttempt](), attempt12, "Attempt")
}

// Build a true schema-11 database from logical values; the frozen SQL fixture
// has no payload tables and never calls the new Store for its old layout.
func legacyCopy12(t *testing.T, source *Store) (string, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	path, target := migrationFixture(t)
	tx, err := source.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, table := range []string{"rollouts", "sessions", "entries", "receipts", "pending", "inputs", "attempts"} {
		rows, err := tx.QueryContext(ctx, "SELECT * FROM "+table)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var records []map[string]any
		for rows.Next() {
			values := make([]any, len(columns))
			ptrs := make([]any, len(columns))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err = rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			r := map[string]any{}
			for i, key := range columns {
				r[key] = values[i]
			}
			records = append(records, r)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		for _, r := range records {
			decode := func(column string, shape *field12) {
				if r[column] == nil {
					return
				}
				raw, err := hydrate(ctx, tx, packedValue{r[column].([]byte), shape})
				if err != nil {
					t.Fatal(err)
				}
				r[column] = raw[0]
			}
			switch table {
			case "sessions":
				decode("state", state12)
			case "entries":
				decode("payload", entry12)
				decode("projection", projection12)
				delete(r, "payload_size")
				delete(r, "projection_size")
			case "attempts":
				decode("payload", attempt12)
				delete(r, "payload_size")
			case "pending", "inputs":
				column := "entry_id"
				rollout := r["rollout_id"]
				if table == "inputs" {
					column = "current_queue_entry_id"
					var id string
					if err = tx.QueryRowContext(ctx, `SELECT rollout_id FROM sessions WHERE id=?`, r["session_id"]).Scan(&id); err != nil {
						t.Fatal(err)
					}
					rollout = id
				}
				var packed []byte
				if err = tx.QueryRowContext(ctx, `SELECT payload FROM entries WHERE rollout_id=? AND id=?`, rollout, r[column]).Scan(&packed); err != nil {
					t.Fatal(err)
				}
				raw, err := hydrate(ctx, tx, packedValue{packed, entry12})
				if err != nil {
					t.Fatal(err)
				}
				r["payload"] = raw[0]
				if table == "inputs" {
					var event map[string]json.RawMessage
					json.Unmarshal(raw[0], &event)
					r["payload"] = []byte(event["input"])
				}
				delete(r, column)
			}
			var names []string
			var args []any
			for key, value := range r {
				names = append(names, key)
				args = append(args, value)
			}
			if _, err = target.Exec(`INSERT INTO `+table+`(`+strings.Join(names, ",")+`) VALUES(`+strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")+`)`, args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	return path, target
}

func TestPayloadMigrationPreservesHistoryQueueAndReceipts(t *testing.T) {
	ctx := context.Background()
	st := payloadStore(t)
	message := core.User(strings.Repeat("历史正文", 2000))
	message.Content = append(message.Content, core.Content{Type: "native", Data: json.RawMessage(`{"encrypted":"` + strings.Repeat("cipher", 1000) + `"}`)})
	input := core.InputRecord{ID: "input", Kind: core.InputSteer, Digest: strings.Repeat("b", 64), Message: &message}
	payloadCommit(t, st, "s", []core.Entry{{ID: "queue", Kind: core.EntryInputQueued, Input: &input}}, nil)
	changed := input
	changed.Message = &core.Message{Role: core.RoleUser, Content: message.Content, Metadata: json.RawMessage(`{"edit":true}`)}
	payloadCommit(t, st, "s", []core.Entry{{ID: "clear", Kind: core.EntryInputCleared, Input: &input}, {ID: "edited", Kind: core.EntryInputRequeued, Input: &changed}}, nil)
	path, old := legacyCopy12(t, st)
	old.Close()
	if opened, err := Open(path); !errors.Is(err, core.ErrUnsupportedFormat) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("Open accepted unmigrated schema: %v", err)
	}
	for range 2 {
		if err := Migrate(ctx, path); err != nil {
			t.Fatal(err)
		}
	}
	converted, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer converted.Close()
	a, err := st.Scan(ctx, "r", core.Query{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := converted.Scan(ctx, "r", core.Query{})
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("history changed: %v", err)
	}
	x, err := st.LookupInput(ctx, "s", "input")
	if err != nil {
		t.Fatal(err)
	}
	y, err := converted.LookupInput(ctx, "s", "input")
	if err != nil || !reflect.DeepEqual(x, y) {
		t.Fatalf("receipt changed: %v", err)
	}
	sa, _ := st.LoadState(ctx, "s")
	sb, err := converted.LoadState(ctx, "s")
	if err != nil || !reflect.DeepEqual(sa, sb) {
		t.Fatalf("state changed: %v", err)
	}
	pa, _ := st.Pending(ctx, "s", core.PendingQuery{})
	pb, err := converted.Pending(ctx, "s", core.PendingQuery{})
	if err != nil || !reflect.DeepEqual(pa, pb) {
		t.Fatalf("pending changed: %v", err)
	}
	for _, id := range []string{"commit-0", "commit-1", "commit-2"} {
		a, _ := st.LookupCommit(ctx, "s", id)
		b, err := converted.LookupCommit(ctx, "s", id)
		if err != nil || a != b {
			t.Fatalf("commit receipt changed: %v", err)
		}
	}
	if n := payloadCount(t, converted); n != 2 {
		t.Fatalf("migration did not share content: %d", n)
	}
}

func TestPayloadMigrationMismatchRollsBackEntireStep(t *testing.T) {
	st := payloadStore(t)
	message := core.User(strings.Repeat("data", 2000))
	input := core.InputRecord{ID: "i", Kind: core.InputSteer, Digest: strings.Repeat("d", 64), Message: &message}
	payloadCommit(t, st, "s", []core.Entry{{ID: "q", Kind: core.EntryInputQueued, Input: &input}}, nil)
	path, db := legacyCopy12(t, st)
	if _, err := db.Exec(`UPDATE inputs SET payload='{"id":"wrong"}'`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), path); err == nil {
		t.Fatal("migrated mismatched input")
	}
	var version, count int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 11 {
		t.Fatalf("failed step advanced schema: %d %v", version, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='payloads'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed migration left partial tables")
	}
}

func TestVacuumPreservesDetachedHistory(t *testing.T) {
	ctx := context.Background()
	st := payloadStore(t)
	message := core.User(strings.Repeat("retained", 1000))
	payloadCommit(t, st, "s", []core.Entry{{ID: "m", Kind: core.EntryMessage, ParentID: "config", Message: &message}}, nil)
	child, err := st.Fork(ctx, core.ForkRequest{SourceID: "s", SourceRolloutID: "r", TargetID: "child", HeadID: "m", Mode: core.ForkDetached})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.DeleteSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	path := st.path
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	if err = Vacuum(ctx, path); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.Get(ctx, child.RolloutID, "m")
	if err != nil || !reflect.DeepEqual(got.Entry.Message, &message) {
		t.Fatalf("vacuum changed history: %v", err)
	}
}

func TestDanglingQueueReferencesAreCorrupt(t *testing.T) {
	st := payloadStore(t)
	message := core.User("input")
	input := core.InputRecord{ID: "i", Kind: core.InputSteer, Digest: strings.Repeat("f", 64), Message: &message}
	payloadCommit(t, st, "s", []core.Entry{{ID: "q", Kind: core.EntryInputQueued, Input: &input}}, nil)
	if _, err := st.db.Exec(`DELETE FROM entries WHERE id='q'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LookupInput(context.Background(), "s", "i"); !errors.Is(err, core.ErrCorrupt) {
		t.Fatalf("missing input body hidden: %v", err)
	}
	if _, err := st.Pending(context.Background(), "s", core.PendingQuery{}); !errors.Is(err, core.ErrCorrupt) {
		t.Fatalf("missing pending body hidden: %v", err)
	}
}
