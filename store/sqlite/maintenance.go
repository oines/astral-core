package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
)

// Vacuum reclaims free pages without changing logical history. This is offline:
// callers must own the database, close every other connection, retain a backup
// and reserve space on the data and SQLite temporary volumes before calling.
// It never migrates old databases or initializes a missing database.
func Vacuum(ctx context.Context, path string) error {
	status, err := InspectMigration(ctx, path)
	if err != nil {
		return err
	}
	if status.Missing || status.NeedsMigration {
		return fmt.Errorf("vacuum requires a current, existing database")
	}
	abs, _, err := migrationPath(path)
	if err != nil {
		return err
	}
	uri := databaseURI(abs, runtime.GOOS)
	db, err := sql.Open("sqlite", uri.String()+"?mode=rw&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	checkpoint := func() error {
		var busy, log, done int
		if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &log, &done); err != nil {
			return err
		}
		if busy != 0 || log != done {
			return fmt.Errorf("database still in use; checkpoint incomplete")
		}
		return nil
	}
	if err = checkpoint(); err != nil {
		return err
	}
	if _, err = db.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	if err = checkpoint(); err != nil {
		return err
	}
	var result string
	if err = db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("vacuum integrity check: %s", result)
	}
	return db.Close()
}
