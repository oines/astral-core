package sqlite

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	core "github.com/oines/astral-core"
)

func format11InputGenerationFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	raw, err := os.ReadFile("testdata/format11-input-generation.sql.gz")
	if err != nil {
		t.Fatal(err)
	}
	zipped, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	dump, err := io.ReadAll(zipped)
	zipped.Close()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "core.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(string(dump)); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func immutableMigrationRecords(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`
SELECT json_array('entry',rollout_id,id,hex(payload),hex(projection)) FROM entries
UNION ALL SELECT json_array('attempt',session_id,id,hex(payload)) FROM attempts
UNION ALL SELECT json_array('receipt',session_id,commit_id,version,digest,first_seq,last_seq) FROM receipts
UNION ALL SELECT json_array('input',session_id,id,kind,digest,accepted_at,hex(payload),state,entry_id) FROM inputs
ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestFormat11InputGenerationMigrationPreservesHistoryAndFrozenRequests(t *testing.T) {
	ctx := context.Background()
	path, db := format11InputGenerationFixture(t)
	before := immutableMigrationRecords(t, db)
	if st, err := Open(path); !errors.Is(err, core.ErrUnsupportedFormat) {
		if st != nil {
			st.Close()
		}
		t.Fatalf("runtime accepted an unmigrated database: %v", err)
	}
	if err := Migrate(ctx, path); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, path); err != nil {
		t.Fatal(err)
	}
	if after := immutableMigrationRecords(t, db); !reflect.DeepEqual(before, after) {
		t.Fatal("migration rewrote immutable history, attempts, receipts or input payloads")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	assertOwner := func(session, input string, wanted bool) {
		t.Helper()
		receipt, err := st.LookupInput(ctx, session, input)
		if err != nil || (receipt.Generation != nil) != wanted {
			t.Fatalf("%s/%s ownership=%+v want=%v: %v", session, input, receipt.Generation, wanted, err)
		}
		if wanted {
			attempt, err := st.GetAttempt(ctx, session, receipt.Generation.AttemptID)
			if err != nil || attempt.Purpose != "generation" || receipt.Generation.RunID != attempt.RunID {
				t.Fatalf("ownership did not resolve to the original generation: %+v %v", receipt, err)
			}
		}
	}
	assertOwner("parent", "seen", true)
	assertOwner("parent", "late", false)
	for i := range 5 {
		assertOwner("parent", fmt.Sprint("unseen-summary-", i), false)
		assertOwner("summary", fmt.Sprint("seen-summary-", i), true)
	}
	assertOwner("omission", "omitted", false)
	assertOwner("omission", "included", true)
	assertOwner("replacement", "replaced", false)
	assertOwner("replacement", "replacement-work", true)
	assertOwner("stopped", "unseen-stopped", false)
	stoppedInput, err := st.LookupInput(ctx, "stopped", "unseen-stopped")
	if err != nil {
		t.Fatal(err)
	}
	stoppedState, err := st.LoadState(ctx, "stopped")
	if err != nil || stoppedState.LastRun == nil || stoppedState.LastRun.Status != "cancelled" || stoppedInput.AcceptedRunID != stoppedState.LastRun.ID {
		t.Fatalf("migration lost admission ownership before Stop: %+v %+v %v", stoppedInput, stoppedState.LastRun, err)
	}
	assertOwner("child", "child-work", true)
	if _, err := st.LookupInput(ctx, "child", "late"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("child inherited parent input ownership: %v", err)
	}
	for _, id := range []string{"parent", "summary", "omission", "replacement", "stopped", "child"} {
		s, err := core.OpenSession(ctx, core.Config{Store: st, EstimateTokens: func(context.Context, core.Model, core.Request) (int, error) {
			t.Fatal("migration/reopen estimated model tokens")
			return 0, nil
		}}, id)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close(ctx)
		if s.State().FormatVersion != core.FormatVersion {
			t.Fatal("checkpoint did not reach the current format")
		}
		page, err := s.Attempts().Scan(ctx, core.AttemptQuery{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		for _, attempt := range page.Attempts {
			if _, err := s.Attempts().Request(ctx, attempt.ID); err != nil {
				t.Fatalf("old frozen request no longer rebuilds at %s: %v", attempt.ID, err)
			}
			base, err := core.ReadContext(ctx, st, attempt.Input.RolloutID, attempt.Input.HeadID, core.DefaultContextBytes)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := st.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			sources, err := generationSourceIDsV11(ctx, tx, attempt.Input.RolloutID, attempt.Input.HeadID)
			tx.Rollback()
			if err != nil || !reflect.DeepEqual(sources, base.Sources) {
				t.Fatalf("frozen v11 source ordering differs at %s: %v\n%v\n%v", attempt.ID, err, sources, base.Sources)
			}
		}
	}
}

func TestFormat11InputGenerationMigrationRollsBackCorruptSelection(t *testing.T) {
	assertFormat11MigrationRollback(t, `UPDATE attempts SET payload=json_set(payload,'$.input.selection',json('[999999]')) WHERE session_id='omission' AND purpose='generation'`)
}

func TestFormat11InputGenerationMigrationRejectsMismatchedAttemptIdentity(t *testing.T) {
	for _, test := range []struct {
		name, path string
		value      any
	}{
		{"session", "$.session_id", "other-session"},
		{"id", "$.id", "other-attempt"},
		{"version", "$.version", 0},
		{"rollout", "$.input.rollout_id", "other-rollout"},
		{"purpose", "$.purpose", "compaction"},
	} {
		t.Run(test.name, func(t *testing.T) {
			value, _ := json.Marshal(test.value)
			assertFormat11MigrationRollback(t, fmt.Sprintf("UPDATE attempts SET payload=json_set(payload,'%s',json('%s')) WHERE session_id='omission' AND purpose='generation'", test.path, value))
		})
	}
}

func assertFormat11MigrationRollback(t *testing.T, mutation string) {
	t.Helper()
	ctx := context.Background()
	path, db := format11InputGenerationFixture(t)
	_, err := db.Exec(mutation)
	if err != nil {
		t.Fatal(err)
	}
	before := immutableMigrationRecords(t, db)
	if err := Migrate(ctx, path); !errors.Is(err, core.ErrCorrupt) {
		t.Fatalf("corrupt provenance was not rejected: %v", err)
	}
	status, err := InspectMigration(ctx, path)
	if err != nil || status.Current != 11 || !status.NeedsMigration {
		t.Fatalf("failed conversion advanced format or ledger: %+v %v", status, err)
	}
	if after := immutableMigrationRecords(t, db); !reflect.DeepEqual(before, after) {
		t.Fatal("failed conversion changed immutable records")
	}
	var columns int
	if err := db.QueryRow("SELECT count(*) FROM pragma_table_info('inputs') WHERE name='generation'").Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("failed conversion retained partially upgraded schema: %d %v", columns, err)
	}
	var checkpoint []byte
	if err := db.QueryRow("SELECT state FROM sessions LIMIT 1").Scan(&checkpoint); err != nil {
		t.Fatal(err)
	}
	var state struct {
		FormatVersion int `json:"format_version"`
	}
	if json.Unmarshal(checkpoint, &state) != nil || state.FormatVersion != 11 {
		t.Fatal("failed conversion retained a partially upgraded checkpoint")
	}
}
