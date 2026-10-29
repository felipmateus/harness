package ledger

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"runtime"
	"strconv"
	"strings"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

const (
	driverName   = "sqlite"
	busyTimeout  = 5000 // milliseconds
	readerConns  = 4
	versionTable = "goose_db_version"
)

// migrationsFS holds the append-only schema history. A recorded file is never edited
// after a release (Safety Invariant 15).
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// binaryVersion is the highest migration this binary embeds.
var binaryVersion = embeddedVersion()

// embeddedVersion returns the highest NNNNN prefix among the embedded migrations.
func embeddedVersion() int {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		panic(fmt.Sprintf("ledger: embedded migrations: %v", err))
	}
	highest := 0
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			continue
		}
		if v, err := strconv.Atoi(prefix); err == nil && v > highest {
			highest = v
		}
	}
	return highest
}

// SchemaAheadError is returned by Open when the database was written by a newer binary
// than this one. It wraps ErrSchemaAhead.
type SchemaAheadError struct {
	DB     int // schema version recorded in the database
	Binary int // highest schema version this binary embeds
}

func (e *SchemaAheadError) Error() string {
	return fmt.Sprintf("ledger schema %d is newer than this binary supports (%d)", e.DB, e.Binary)
}

// Unwrap lets errors.Is(err, ErrSchemaAhead) match.
func (e *SchemaAheadError) Unwrap() error { return ErrSchemaAhead }

// pragmas applied to every connection, writer and readers alike. fullfsync matters on
// macOS only: there a plain fsync does not reach stable storage, so both the commit and
// the checkpoint path must ask for F_FULLFSYNC.
func pragmas() []string {
	p := []string{
		"busy_timeout(" + strconv.Itoa(busyTimeout) + ")",
		"journal_mode(WAL)",
		"synchronous(FULL)",
		"foreign_keys(ON)",
	}
	if runtime.GOOS == "darwin" {
		p = append(p, "fullfsync(ON)", "checkpoint_fullfsync(ON)")
	}
	return p
}

// fileDSN builds a SQLite URI for path with the given query parameters.
func fileDSN(path string, q url.Values) string {
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	return u.String()
}

// writerDSN opens the single writer connection: BEGIN IMMEDIATE on every transaction.
func writerDSN(path string) string {
	q := url.Values{"_txlock": {"immediate"}}
	for _, p := range pragmas() {
		q.Add("_pragma", p)
	}
	return fileDSN(path, q)
}

// readerDSN opens a reader connection: same durability pragmas, writes refused.
func readerDSN(path string) string {
	q := url.Values{}
	for _, p := range append(pragmas(), "query_only(ON)") {
		q.Add("_pragma", p)
	}
	return fileDSN(path, q)
}

// readOnlyDSN opens path with mode=ro: it can never write the database file. It is used
// for the schema-ahead check and for Inspect, and carries no journal or durability
// pragma, since those would write.
func readOnlyDSN(path string) string {
	q := url.Values{"mode": {"ro"}}
	q.Add("_pragma", "busy_timeout("+strconv.Itoa(busyTimeout)+")")
	return fileDSN(path, q)
}

// immutableDSN opens a database file nobody else has open and whose WAL is empty or gone, as
// immutable: SQLite then reads the main file alone and creates no -wal or -shm file.
func immutableDSN(path string) string {
	q := url.Values{"mode": {"ro"}, "immutable": {"1"}}
	return fileDSN(path, q)
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// schemaVersion returns the highest migration version recorded in the database, as goose
// does, or 0 when the goose table does not exist.
func schemaVersion(ctx context.Context, q rowQuerier) (int, error) {
	var name string
	err := q.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, versionTable).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var v sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT MAX(version_id) FROM `+versionTable).Scan(&v); err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

// readSchemaVersion reports the schema version of an existing database file over a
// mode=ro connection. hook, when set, receives the DSN used.
func readSchemaVersion(ctx context.Context, path string, hook func(dsn string)) (int, error) {
	dsn := readOnlyDSN(path)
	if hook != nil {
		hook(dsn)
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	return schemaVersion(ctx, db)
}

// migrate brings the schema to the embedded head. It runs on the writer connection, only
// after the schema-ahead check has passed: goose itself does not refuse a newer database.
func migrate(ctx context.Context, db *sql.DB) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, sub, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
}
