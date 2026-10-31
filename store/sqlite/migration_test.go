package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	core "github.com/oines/astral-core"
	"github.com/pressly/goose/v3"
)

func migrationFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "core.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/schema11.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(raw) + `CREATE TABLE goose_db_version(id INTEGER PRIMARY KEY AUTOINCREMENT,version_id INTEGER NOT NULL,is_applied BOOLEAN NOT NULL,tstamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP); INSERT INTO goose_db_version(version_id,is_applied) VALUES(0,1),(11,1); PRAGMA journal_mode=WAL;`); err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return path, db
}

func execMigrationSQL(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationNewDatabaseAndReadOnlyInspection(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "core.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	filesBefore, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	status, err := InspectMigration(ctx, path)
	if err != nil || status != (MigrationStatus{Current: 11, Target: 11, SchemaCurrent: 12, SchemaTarget: 12}) {
		t.Fatalf("new database: %+v %v", status, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("inspection changed the database: %v", err)
	}
	filesAfter, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(filesBefore) != len(filesAfter) {
		t.Fatalf("inspection created files: %v -> %v", filesBefore, filesAfter)
	}
	missing := filepath.Join(t.TempDir(), "uncreated", "core.db")
	status, err = InspectMigration(ctx, missing)
	if err != nil || status != (MigrationStatus{Target: 11, SchemaTarget: 12, Missing: true}) {
		t.Fatalf("missing database: %+v %v", status, err)
	}
	if err = Migrate(ctx, missing); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Dir(missing)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspection or migration created a missing parent: %v", err)
	}
}

func TestMigrationAdoptsOnlySupportedBaselineAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path, db := migrationFixture(t)
	execMigrationSQL(t, db, "DROP TABLE goose_db_version")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	status, err := InspectMigration(ctx, path)
	if err != nil || status != (MigrationStatus{Current: 11, Target: 11, SchemaCurrent: 11, SchemaTarget: 12, NeedsMigration: true}) {
		t.Fatalf("baseline inspection: %+v %v", status, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("inspection initialized the ledger: %v", err)
	}
	for range 2 {
		if err = Migrate(ctx, path); err != nil {
			t.Fatal(err)
		}
	}
	status, err = InspectMigration(ctx, path)
	if err != nil || status.NeedsMigration || status.Current != 11 {
		t.Fatalf("adopted baseline: %+v %v", status, err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var count int
	if err = st.db.QueryRow("SELECT count(*) FROM goose_db_version").Scan(&count); err != nil || count != 3 {
		t.Fatalf("baseline applied more than once: %d %v", count, err)
	}
}

func testMigrationPlan(failLast bool) migrationPlan {
	return migrationPlan{
		files: fstest.MapFS{
			"00011_baseline.sql": {Data: []byte("-- +goose Up\nSELECT 1;\n")},
			"00012_order.sql":    {Data: []byte("-- +goose Up\nCREATE TABLE migration_order(step INTEGER);\nINSERT INTO migration_order VALUES(12);\n")},
		},
		goMigrations: []*goose.Migration{goose.NewGoMigration(13, &goose.GoFunc{RunTx: func(ctx context.Context, tx *sql.Tx) error {
			var steps int
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM migration_order WHERE step=12").Scan(&steps); err != nil {
				return err
			}
			if steps != 1 {
				return errors.New("Go migration did not see the preceding SQL migration")
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO migration_order VALUES(13)"); err != nil {
				return err
			}
			if failLast {
				return errors.New("injected conversion failure")
			}
			return nil
		}}, nil)},
		target: 13,
	}
}

func migrationSteps(t *testing.T, db *sql.DB) []int {
	t.Helper()
	rows, err := db.Query("SELECT step FROM migration_order ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var steps []int
	for rows.Next() {
		var step int
		if err = rows.Scan(&step); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, step)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return steps
}

func TestMigrationRunsAllMissingSQLAndGoStepsInOrder(t *testing.T) {
	ctx := context.Background()
	path, db := migrationFixture(t)
	plan := testMigrationPlan(false)
	status, err := inspectMigration(ctx, db, plan)
	if err != nil || status.Current != 11 || status.Target != 13 || !status.NeedsMigration {
		t.Fatalf("before: %+v %v", status, err)
	}
	for range 2 {
		if err = migrateDatabase(ctx, db, plan); err != nil {
			t.Fatal(err)
		}
	}
	if steps := migrationSteps(t, db); !reflect.DeepEqual(steps, []int{12, 13}) {
		t.Fatalf("migration order or duplicate application: %v", steps)
	}
	status, err = inspectMigration(ctx, db, plan)
	if err != nil || status.Current != 13 || status.NeedsMigration {
		t.Fatalf("after: %+v %v", status, err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if st, err := Open(path); !errors.Is(err, core.ErrUnsupportedFormat) {
		if st != nil {
			st.Close()
		}
		t.Fatalf("normal Open accepted a future format: %v", err)
	}
}

func TestMigrationFailureRollsBackDataMarkersAndLedgerThenResumes(t *testing.T) {
	ctx := context.Background()
	_, db := migrationFixture(t)
	plan := testMigrationPlan(true)
	if err := migrateDatabase(ctx, db, plan); err == nil || !strings.Contains(err.Error(), "injected conversion failure") {
		t.Fatalf("expected injected failure, got %v", err)
	}
	status, err := inspectMigration(ctx, db, plan)
	if err != nil || status.Current != 12 || !status.NeedsMigration {
		t.Fatalf("failed step advanced markers or ledger: %+v %v", status, err)
	}
	if steps := migrationSteps(t, db); !reflect.DeepEqual(steps, []int{12}) {
		t.Fatalf("failed step did not roll back: %v", steps)
	}
	if err = migrateDatabase(ctx, db, testMigrationPlan(false)); err != nil {
		t.Fatal(err)
	}
	if steps := migrationSteps(t, db); !reflect.DeepEqual(steps, []int{12, 13}) {
		t.Fatalf("resume repeated or skipped a step: %v", steps)
	}
}

func TestMigrationRejectsUnknownAndInconsistentFormatsBeforeWriting(t *testing.T) {
	for _, mutation := range []string{
		"UPDATE format SET version=9; PRAGMA user_version=9;",
		"UPDATE format SET version=10; PRAGMA user_version=10;",
		"UPDATE format SET version=12; PRAGMA user_version=12;",
		"PRAGMA user_version=0;",
		"INSERT INTO format VALUES(11);",
	} {
		t.Run(mutation, func(t *testing.T) {
			path, db := migrationFixture(t)
			execMigrationSQL(t, db, mutation)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = InspectMigration(context.Background(), path); !errors.Is(err, core.ErrUnsupportedFormat) {
				t.Fatalf("inspection accepted unknown format: %v", err)
			}
			if err = Migrate(context.Background(), path); !errors.Is(err, core.ErrUnsupportedFormat) {
				t.Fatalf("migration accepted unknown format: %v", err)
			}
			if st, err := Open(path); !errors.Is(err, core.ErrUnsupportedFormat) {
				if st != nil {
					st.Close()
				}
				t.Fatalf("Open accepted unknown format: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("unsupported database was changed: %v", err)
			}
		})
	}
}

func TestMigrationRejectsLedgerGapsAndUnregisteredSteps(t *testing.T) {
	ctx := context.Background()
	for _, mutation := range []string{
		"INSERT INTO goose_db_version(version_id,is_applied) VALUES(12,1);",
		"DELETE FROM goose_db_version WHERE version_id=0;",
		"UPDATE goose_db_version SET is_applied=0 WHERE version_id=11;",
		"DELETE FROM goose_db_version;",
	} {
		t.Run(mutation, func(t *testing.T) {
			_, db := migrationFixture(t)
			execMigrationSQL(t, db, mutation)
			if _, err := inspectMigration(ctx, db, currentMigrationPlan()); !errors.Is(err, core.ErrCorrupt) {
				t.Fatalf("invalid ledger was accepted: %v", err)
			}
		})
	}
	_, db := migrationFixture(t)
	plan := testMigrationPlan(false)
	delete(plan.files.(fstest.MapFS), "00012_order.sql")
	if err := migrateDatabase(ctx, db, plan); err == nil || !strings.Contains(err.Error(), "not sequential") {
		t.Fatalf("binary with missing migration was accepted: %v", err)
	}
}

func TestMigrationRejectsNonDatabaseAndHonorsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.db")
	if err := os.WriteFile(path, []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectMigration(context.Background(), path); !errors.Is(err, core.ErrUnsupportedFormat) {
		t.Fatalf("invalid header: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := InspectMigration(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("ignored cancellation: %v", err)
	}
}

func TestMigrationInspectionSeesUncheckpointedWAL(t *testing.T) {
	path, db := migrationFixture(t)
	execMigrationSQL(t, db, "PRAGMA wal_autocheckpoint=0; UPDATE format SET version=12; PRAGMA user_version=12;")
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("fixture did not retain a WAL: %v", err)
	}
	before := make(map[string][]byte)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		before[suffix] = data
	}
	status, err := InspectMigration(context.Background(), path)
	if !errors.Is(err, core.ErrUnsupportedFormat) || status.Current != 12 {
		t.Fatalf("inspection ignored the WAL: %+v %v", status, err)
	}
	for suffix, data := range before {
		after, err := os.ReadFile(path + suffix)
		if err != nil || !bytes.Equal(data, after) {
			t.Fatalf("inspection changed original %q: %v", suffix, err)
		}
	}
}

func TestMigrationRejectsNonTransactionalAndEnvironmentDependentSteps(t *testing.T) {
	ctx := context.Background()
	for _, annotation := range []string{"NO TRANSACTION", "no transaction", "ENVSUB ON"} {
		t.Run(annotation, func(t *testing.T) {
			_, db := migrationFixture(t)
			plan := testMigrationPlan(false)
			plan.files.(fstest.MapFS)["00012_order.sql"].Data = []byte("-- +goose " + annotation + "\n-- +goose Up\nCREATE TABLE migration_order(step INTEGER);\n")
			if err := migrateDatabase(ctx, db, plan); err == nil {
				t.Fatal("unsafe SQL migration accepted")
			}
			var count int
			if err := db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name='migration_order'").Scan(&count); err != nil || count != 0 {
				t.Fatalf("unsafe migration ran before rejection: %d %v", count, err)
			}
		})
	}
	_, db := migrationFixture(t)
	plan := testMigrationPlan(false)
	plan.goMigrations = []*goose.Migration{goose.NewGoMigration(13, &goose.GoFunc{RunDB: func(context.Context, *sql.DB) error {
		t.Fatal("nontransactional Go migration executed")
		return nil
	}}, nil)}
	if err := migrateDatabase(ctx, db, plan); err == nil {
		t.Fatal("nontransactional Go migration accepted")
	}
}
