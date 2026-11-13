// Package db opens the taskpilot SQLite database and applies migrations.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go, no CGO)
)

//go:embed migrations/*.sql
var migrations embed.FS

// DB wraps the connection pool.
type DB struct {
	*sql.DB
	Path string
}

// FileName is the database file inside the taskpilot home.
const FileName = "taskpilot.db"

// Open opens (creating if needed) <home>/taskpilot.db with WAL, a busy
// timeout and foreign keys, then applies pending migrations.
func Open(ctx context.Context, home string) (*DB, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(home, FileName)
	// Create the file with owner-only permissions before the driver does.
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600); err != nil {
		return nil, err
	} else {
		f.Close()
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	d := &DB{DB: sqldb, Path: path}
	if err := d.migrate(ctx); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return d, nil
}

// SchemaVersion returns PRAGMA user_version.
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := d.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

// migrate applies migrations/NNNN_*.sql files numbered above user_version,
// each in its own transaction.
func (d *DB) migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	type mig struct {
		n    int
		name string
	}
	var ms []mig
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		n, err := strconv.Atoi(num)
		if !ok || err != nil {
			return fmt.Errorf("bad migration name %q", e.Name())
		}
		ms = append(ms, mig{n, e.Name()})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].n < ms[j].n })

	cur, err := d.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if m.n <= cur {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + m.name)
		if err != nil {
			return err
		}
		tx, err := d.BeginTx(ctx, nil) // _txlock=immediate: holds the write lock from here
		if err != nil {
			return err
		}
		// Another process may have applied it while this one waited for the lock.
		var now int
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&now); err != nil || now >= m.n {
			tx.Rollback()
			if err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.n)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Now is the timestamp format used in every table.
func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
