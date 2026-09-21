// Package sqlite implements transactional routing storage with encrypted content.
package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/url"
	"path/filepath"
	"time"

	"github.com/storm-software/mindctl/internal/contentcrypto"
	"github.com/storm-software/mindctl/internal/storage"
	"github.com/storm-software/mindctl/migrations"
	_ "modernc.org/sqlite"
)

type Options struct {
	// Path is a filesystem path, not an arbitrary SQLite DSN.
	Path    string
	Keyring *contentcrypto.Keyring
}

type DB struct {
	db      *sql.DB
	keyring *contentcrypto.Keyring
}

var _ storage.Repository = (*DB)(nil)

func failure(op string, err error) error { return &storage.Error{Op: op, Err: err} }

func Open(ctx context.Context, opts Options) (*DB, error) {
	if opts.Path == "" || opts.Keyring == nil {
		return nil, failure("open", errors.New("path and initialized keyring are required"))
	}
	path, err := filepath.Abs(opts.Path)
	if err != nil {
		return nil, failure("resolve database path", err)
	}
	// URI escaping prevents path characters from injecting DSN parameters. These
	// pragmas run whenever the driver opens a physical connection, including when
	// database/sql replaces the one connection after cancellation or an idle close.
	dsn := url.URL{Scheme: "file", Path: path}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "journal_mode(WAL)")
	dsn.RawQuery = q.Encode()
	sqlDB, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, failure("open", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	db := &DB{db: sqlDB, keyring: opts.Keyring}
	if err := db.Ready(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	if err := migrate(ctx, sqlDB, migrations.Files); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// SQL exposes diagnostics and administration. Callers must not change connection
// settings or write captured content directly; application writes go through Tx.
func (db *DB) SQL() *sql.DB { return db.db }

func (db *DB) Close() error {
	if err := db.db.Close(); err != nil {
		return failure("close", err)
	}
	return nil
}

// Ready observes usability and the required pragma state without repairing or
// otherwise mutating the database. All probes use the same physical connection.
func (db *DB) Ready(ctx context.Context) error {
	conn, err := db.db.Conn(ctx)
	if err != nil {
		return failure("readiness connection", err)
	}
	defer conn.Close()
	var mode string
	var fk, timeout int
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return failure("readiness journal mode", err)
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		return failure("readiness foreign keys", err)
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil {
		return failure("readiness busy timeout", err)
	}
	if mode != "wal" || fk != 1 || timeout <= 0 {
		return failure("readiness settings", errors.New("required SQLite pragmas are not enabled"))
	}
	return nil
}

// inTransaction owns a physical connection until COMMIT or ROLLBACK completes.
// A failed SQLite COMMIT can leave the transaction active. Using explicit SQL
// here permits rollback even after commit failure (sql.Tx is already marked
// done then). It also guarantees cleanup after callback panic or cancellation.
func inTransaction(ctx context.Context, db *sql.DB, write bool, fn func(*sql.Conn) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return failure("transaction connection", err)
	}
	defer conn.Close()
	begin := "BEGIN"
	if write {
		begin = "BEGIN IMMEDIATE"
	}
	if _, err := conn.ExecContext(ctx, begin); err != nil {
		return failure("begin transaction", err)
	}
	committed := false
	defer func() {
		if !committed {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if _, err := conn.ExecContext(cleanup, "ROLLBACK"); err != nil {
				// Never return a connection with uncertain transactional state to
				// the pool, even if rollback failed or the request was canceled.
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}
	}()
	if err := fn(conn); err != nil {
		return failure("transaction callback", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return failure("commit transaction", err)
	}
	committed = true
	return nil
}
