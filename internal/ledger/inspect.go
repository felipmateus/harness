package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"os"
)

// Inspection is what Inspect reports about a ledger file.
type Inspection struct {
	SchemaVersion int      // highest migration recorded in the file; 0 when it has no schema
	BinaryVersion int      // highest migration this binary embeds
	QuickCheck    []string // rows of PRAGMA quick_check; exactly ["ok"] when healthy
}

// hasWALFiles reports whether the write-ahead log or its shared-memory index exist.
func hasWALFiles(path string) bool {
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			return true
		}
	}
	return false
}

// IntegrityOK reports whether quick_check found no problem.
func (i Inspection) IntegrityOK() bool {
	return len(i.QuickCheck) == 1 && i.QuickCheck[0] == "ok"
}

// SchemaAhead reports whether the file was written by a newer binary.
func (i Inspection) SchemaAhead() bool { return i.SchemaVersion > i.BinaryVersion }

// Inspect opens path read-only and reports integrity (quick_check) and schema version
// without migrating, creating or writing anything. A file that does not exist is an error
// that wraps os.ErrNotExist. Used by doctor in local mode.
//
// A database that was closed cleanly has no -wal and no -shm file, and then everything is
// in the main file: Inspect reads it as immutable, which makes SQLite neither create those
// two files nor lock anything, so the home is left exactly as it was found. When either
// file exists a daemon is running or died, the WAL may hold committed pages, and the
// connection is a plain mode=ro one that reads through them.
func Inspect(ctx context.Context, path string) (Inspection, error) {
	if _, err := os.Stat(path); err != nil {
		return Inspection{}, fmt.Errorf("ledger: inspect: %w", err)
	}
	dsn := readOnlyDSN(path)
	if !hasWALFiles(path) {
		dsn = immutableDSN(path)
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return Inspection{}, fmt.Errorf("ledger: inspect: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	in := Inspection{BinaryVersion: binaryVersion}
	rows, err := db.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return Inspection{}, fmt.Errorf("ledger: inspect: quick_check: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return Inspection{}, fmt.Errorf("ledger: inspect: quick_check: %w", err)
		}
		in.QuickCheck = append(in.QuickCheck, line)
	}
	if err := rows.Err(); err != nil {
		return Inspection{}, fmt.Errorf("ledger: inspect: quick_check: %w", err)
	}
	if err := rows.Close(); err != nil {
		return Inspection{}, fmt.Errorf("ledger: inspect: %w", err)
	}

	if in.SchemaVersion, err = schemaVersion(ctx, db); err != nil {
		return Inspection{}, fmt.Errorf("ledger: inspect: schema version: %w", err)
	}
	return in, nil
}
