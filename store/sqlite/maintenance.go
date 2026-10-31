package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"time"

	core "github.com/oines/astral-core"
)

// Reclaim compacts this open Store without changing logical history or payload
// ownership. It temporarily blocks this Store's reads and writes, drains current
// reads, performs a full VACUUM and truncates the WAL. It never closes the Store.
// The host must reserve space on the data and SQLite temporary volumes first.
//
// Waiting and SQL execution honor ctx. Other Store handles or processes may
// prevent reclamation; errors wrapping core.ErrBusy can be retried. Cancellation
// or failure leaves the Store usable. A failed final checkpoint can mean VACUUM
// completed but its disk space has not yet been reclaimed. Reclaim does not
// migrate schemas or enable auto_vacuum.
func (st *Store) Reclaim(ctx context.Context) error {
	// The ordinary write mutex also protects page writes and cleanup during a
	// detached Fork. Do not queue an uncancellable goroutine on that mutex.
	for !st.write.TryLock() {
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer st.write.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	// Holding every reader connection drains existing snapshots and keeps new
	// readers queued in database/sql until the final checkpoint has finished.
	// Borrowing is cancellable and every partially acquired set is returned.
	var readers []*sql.Conn
	defer func() {
		for _, conn := range readers {
			conn.Close()
		}
	}()
	for range maxReadConnections {
		conn, err := st.readDB.Conn(ctx)
		if err != nil {
			return reclaimError(ctx, err)
		}
		readers = append(readers, conn)
	}

	// A separate maintenance connection can fail fast on locks held by other
	// handles, without altering the live Store's connection settings or pools.
	uri := databaseURI(st.path, runtime.GOOS)
	db, err := sql.Open("sqlite", uri.String()+"?mode=rw&_pragma=busy_timeout(0)&_pragma=synchronous(FULL)")
	if err != nil {
		return reclaimError(ctx, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	checkpoint := func() error {
		var busy, log, done int
		if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &log, &done); err != nil {
			return reclaimError(ctx, err)
		}
		if busy != 0 || log != done {
			return fmt.Errorf("%w: storage reclaim checkpoint incomplete", core.ErrBusy)
		}
		return nil
	}
	if err = checkpoint(); err != nil {
		return err
	}
	if _, err = db.ExecContext(ctx, `VACUUM`); err != nil {
		return reclaimError(ctx, err)
	}
	return checkpoint()
}

func reclaimError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var coded interface{ Code() int }
	// SQLite's extended result codes retain the primary code in the low byte.
	if errors.As(err, &coded) && (coded.Code()&0xff == 5 || coded.Code()&0xff == 6) { // SQLITE_BUSY / SQLITE_LOCKED
		return fmt.Errorf("%w: storage reclaim: %w", core.ErrBusy, err)
	}
	return err
}

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
