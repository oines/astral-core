package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	core "github.com/oines/astral-core"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
)

const firstMigrationFormat int64 = 10

//go:embed migrations/*.sql
var migrationFiles embed.FS

// MigrationStatus describes the durable Core format, rather than an application
// release number. NeedsMigration also covers adopting the format-10 baseline
// into Goose's ledger without changing the data format.
type MigrationStatus struct {
	Current        int64 `json:"current"`
	Target         int64 `json:"target"`
	NeedsMigration bool  `json:"needs_migration"`
	Missing        bool  `json:"missing"`
}

type migrationPlan struct {
	files        fs.FS
	goMigrations []*goose.Migration
	target       int64
}

func currentMigrationPlan() migrationPlan {
	files, _ := fs.Sub(migrationFiles, "migrations")
	return migrationPlan{files: files, target: core.FormatVersion}
}

// InspectMigration reads an existing database without initializing a schema,
// creating a file or preparing its parent directories. A missing file is not an
// error: Open initializes new databases directly at the current format.
//
// The caller must stop all database users before inspecting for an upgrade and
// keep them stopped through backup, Migrate and any restoration. This API does
// not supply the Host's process ownership lock or a multi-database transaction.
func InspectMigration(ctx context.Context, path string) (MigrationStatus, error) {
	if err := ctx.Err(); err != nil {
		return MigrationStatus{}, err
	}
	plan := currentMigrationPlan()
	abs, missing, err := migrationPath(path)
	if err != nil {
		return MigrationStatus{}, err
	}
	if missing {
		return MigrationStatus{Target: plan.target, Missing: true}, nil
	}
	source, immutable, cleanup, err := inspectionSource(ctx, abs)
	if err != nil {
		return MigrationStatus{}, err
	}
	defer cleanup()
	uri := databaseURI(source, runtime.GOOS)
	query := "?mode=ro"
	if immutable {
		query += "&immutable=1"
	}
	db, err := sql.Open("sqlite", uri.String()+query)
	if err != nil {
		return MigrationStatus{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	return inspectMigration(ctx, db, plan)
}

// SQLite mode=ro can create WAL auxiliaries; immutable=1 ignores existing WAL
// content. Use the fast immutable path only for checkpointed databases. A crash
// WAL is inspected on a temporary copy, so the original database and every
// sidecar remain untouched. The Host's offline ownership makes the copy stable.
func inspectionSource(ctx context.Context, path string) (string, bool, func(), error) {
	info, err := os.Stat(path + "-wal")
	if errors.Is(err, os.ErrNotExist) || (err == nil && info.Size() == 0) {
		return path, true, func() {}, nil
	}
	if err != nil {
		return "", false, nil, err
	}
	dir, err := os.MkdirTemp("", "astral-core-inspect-")
	if err != nil {
		return "", false, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	copyPath := filepath.Join(dir, "core.db")
	for _, suffix := range []string{"", "-wal"} {
		if err = copyInspectionFile(ctx, path+suffix, copyPath+suffix); err != nil {
			cleanup()
			return "", false, nil, err
		}
	}
	return copyPath, false, cleanup, nil
}

func copyInspectionFile(ctx context.Context, source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, inspectionReader{ctx: ctx, Reader: in})
	closeErr := out.Close()
	return errors.Join(err, closeErr)
}

type inspectionReader struct {
	ctx context.Context
	io.Reader
}

func (r inspectionReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

// Migrate applies every missing Core migration in ascending order. Migrations
// are embedded in the latest binary; intermediate releases are not required.
// Each step and its format markers are committed with its Goose ledger entry
// in one SQLite transaction. On failure, that step is rolled back, and no later
// step runs. Earlier successful steps remain committed for retry or restoration.
//
// Migrate is an offline operation. The Host owns shutdown, exclusive process
// ownership, backup and coordinated recovery of Core, Host and external files.
// Unknown formats (including pre-baseline formats) are never relabelled. Missing
// databases are left missing, and no downgrade operation is provided.
func Migrate(ctx context.Context, path string) error {
	status, err := InspectMigration(ctx, path)
	if err != nil || status.Missing || !status.NeedsMigration {
		return err
	}
	abs, missing, err := migrationPath(path)
	if err != nil || missing {
		if missing {
			return fmt.Errorf("core migration database disappeared: %w", os.ErrNotExist)
		}
		return err
	}
	uri := databaseURI(abs, runtime.GOOS)
	db, err := sql.Open("sqlite", uri.String()+"?mode=rw&_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	return migrateDatabase(ctx, db, currentMigrationPlan())
}

// Unlike databasePath, this resolver must not create directories or databases.
func migrationPath(path string) (string, bool, error) {
	if path == "" {
		return "", false, core.ErrInvalid
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if errors.Is(err, os.ErrNotExist) {
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	var head [16]byte
	if _, err = io.ReadFull(f, head[:]); err != nil || string(head[:]) != "SQLite format 3\x00" {
		return "", false, core.ErrUnsupportedFormat
	}
	return abs, false, nil
}

func newMigrationProvider(db *sql.DB, plan migrationPlan) (*goose.Provider, error) {
	for _, migration := range plan.goMigrations {
		if migration == nil || !migration.UseTx || migration.UpFnNoTxContext != nil {
			return nil, errors.New("core Go migrations require a transaction-scoped up function")
		}
	}
	store, err := database.NewStore(goose.DialectSQLite3, goose.DefaultTablename)
	if err != nil {
		return nil, err
	}
	p, err := goose.NewProvider(goose.DialectCustom, db, plan.files,
		goose.WithStore(migrationStore{Store: store}),
		goose.WithDisableGlobalRegistry(true),
		goose.WithGoMigrations(plan.goMigrations...),
	)
	if err != nil {
		return nil, err
	}
	sources := p.ListSources()
	// Formats are a sequential, immutable chain, independent of release tags.
	// Reject missing steps in the binary before opening a migration transaction.
	for i, source := range sources {
		if source.Version != firstMigrationFormat+int64(i) {
			return nil, fmt.Errorf("core migration chain is not sequential at %d", source.Version)
		}
		if source.Type == goose.TypeSQL {
			contents, err := fs.ReadFile(plan.files, source.Path)
			if err != nil {
				return nil, err
			}
			for _, line := range strings.Split(string(contents), "\n") {
				if !strings.Contains(line, "+goose") {
					continue
				}
				annotation := strings.TrimSpace(strings.Replace(strings.ReplaceAll(line, "--", ""), "+goose", "", 1))
				if strings.EqualFold(annotation, "NO TRANSACTION") {
					return nil, fmt.Errorf("core SQL migration %d disables its transaction", source.Version)
				}
				if strings.EqualFold(annotation, "ENVSUB ON") {
					return nil, fmt.Errorf("core SQL migration %d depends on environment substitution", source.Version)
				}
			}
		}
	}
	if len(sources) == 0 || sources[len(sources)-1].Version != plan.target {
		return nil, fmt.Errorf("core migration target does not match format %d", plan.target)
	}
	return p, nil
}

func readFormatMarkers(ctx context.Context, db database.DBTxConn) (int64, error) {
	var count int
	var format sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT count(*), min(version) FROM format").Scan(&count, &format); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("core format marker: %w: %v", core.ErrUnsupportedFormat, err)
	}
	var userVersion int64
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		return 0, err
	}
	if count != 1 || !format.Valid || format.Int64 != userVersion {
		return 0, fmt.Errorf("core format markers disagree: %w", core.ErrUnsupportedFormat)
	}
	return format.Int64, nil
}

func inspectMigration(ctx context.Context, db *sql.DB, plan migrationPlan) (MigrationStatus, error) {
	status := MigrationStatus{Target: plan.target}
	if _, err := newMigrationProvider(db, plan); err != nil {
		return status, err
	}
	format, err := readFormatMarkers(ctx, db)
	if err != nil {
		return status, err
	}
	status.Current = format
	if format < firstMigrationFormat || format > plan.target {
		return status, fmt.Errorf("core format %d cannot migrate to %d: %w", format, plan.target, core.ErrUnsupportedFormat)
	}
	var hasLedger bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name=?)", goose.DefaultTablename).Scan(&hasLedger); err != nil {
		return status, err
	}
	if !hasLedger {
		if format != firstMigrationFormat {
			return status, fmt.Errorf("core format %d has no migration ledger: %w", format, core.ErrCorrupt)
		}
		status.NeedsMigration = true
		return status, nil
	}
	rows, err := db.QueryContext(ctx, "SELECT version_id, is_applied FROM goose_db_version ORDER BY id")
	if err != nil {
		return status, fmt.Errorf("core migration ledger: %w: %v", core.ErrCorrupt, err)
	}
	defer rows.Close()
	previous := int64(-1)
	for rows.Next() {
		var version int64
		var applied bool
		if err := rows.Scan(&version, &applied); err != nil {
			return status, err
		}
		expected := previous + 1
		if previous == 0 {
			expected = firstMigrationFormat
		}
		if !applied || version != expected || version > format {
			return status, fmt.Errorf("core migration ledger is not an applied prefix at %d: %w", version, core.ErrCorrupt)
		}
		previous = version
	}
	if err := rows.Err(); err != nil {
		return status, err
	}
	if previous != format && !(previous == 0 && format == firstMigrationFormat) {
		return status, fmt.Errorf("core format %d and migration ledger %d disagree: %w", format, previous, core.ErrCorrupt)
	}
	status.NeedsMigration = previous < plan.target
	return status, nil
}

func migrateDatabase(ctx context.Context, db *sql.DB, plan migrationPlan) error {
	status, err := inspectMigration(ctx, db, plan)
	if err != nil || !status.NeedsMigration {
		return err
	}
	p, err := newMigrationProvider(db, plan)
	if err != nil {
		return err
	}
	if _, err = p.Up(ctx); err != nil {
		return fmt.Errorf("core migration: %w", err)
	}
	status, err = inspectMigration(ctx, db, plan)
	if err != nil {
		return err
	}
	if status.NeedsMigration {
		return errors.New("core migration did not reach the target format")
	}
	return nil
}

// A newly created database already has the target schema. Record the complete
// chain without applying historical transformations to current-format data.
func initializeMigrationHistory(ctx context.Context, db *sql.DB, tx *sql.Tx, plan migrationPlan) error {
	p, err := newMigrationProvider(db, plan)
	if err != nil {
		return err
	}
	store, err := database.NewStore(goose.DialectSQLite3, goose.DefaultTablename)
	if err != nil {
		return err
	}
	if err = store.CreateVersionTable(ctx, tx); err != nil {
		return err
	}
	if err = store.Insert(ctx, tx, database.InsertRequest{Version: 0}); err != nil {
		return err
	}
	for _, source := range p.ListSources() {
		if err = store.Insert(ctx, tx, database.InsertRequest{Version: source.Version}); err != nil {
			return err
		}
	}
	return nil
}

// Goose inserts the ledger entry using the same transaction as the migration.
// Keeping the two Core format markers here prevents a migration author from
// committing converted data with an old marker or advancing only one marker.
type migrationStore struct{ database.Store }

func (s migrationStore) Insert(ctx context.Context, db database.DBTxConn, req database.InsertRequest) error {
	if req.Version != 0 {
		if _, ok := db.(*sql.Tx); !ok {
			return errors.New("core migration requires a transaction")
		}
		format, err := readFormatMarkers(ctx, db)
		if err != nil {
			return err
		}
		expected := req.Version - 1
		if req.Version == firstMigrationFormat {
			expected = firstMigrationFormat
		}
		if format != expected {
			return fmt.Errorf("core migration %d starts from format %d: %w", req.Version, format, core.ErrUnsupportedFormat)
		}
		if _, err = db.ExecContext(ctx, fmt.Sprintf("UPDATE format SET version=%d; PRAGMA user_version=%d;", req.Version, req.Version)); err != nil {
			return err
		}
	}
	return s.Store.Insert(ctx, db, req)
}
