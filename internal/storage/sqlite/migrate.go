package sqlite

import (
	"context"
	"database/sql"
	"io/fs"
	"sort"
	"time"
)

func migrate(ctx context.Context, db *sql.DB, files fs.FS) error {
	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return failure("list migrations", err)
	}
	sort.Strings(names)
	return inTransaction(ctx, db, true, func(conn *sql.Conn) error {
		// Bootstrap the ledger within the same transaction as migration success.
		if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
			return failure("initialize migration ledger", err)
		}
		for _, name := range names {
			var applied int
			if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE version = ?", name).Scan(&applied); err != nil {
				return failure("read migration ledger", err)
			}
			if applied != 0 {
				continue
			}
			body, err := fs.ReadFile(files, name)
			if err != nil {
				return failure("read migration", err)
			}
			if _, err := conn.ExecContext(ctx, string(body)); err != nil {
				return failure("apply migration", err)
			}
			if _, err := conn.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)", name, time.Now().UTC().UnixNano()); err != nil {
				return failure("record migration", err)
			}
		}
		return nil
	})
}
