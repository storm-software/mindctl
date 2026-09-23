package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/storm-software/mindctl/internal/storage"
	sqlitedriver "modernc.org/sqlite"
)

// Using the caller's cancellable context for COMMIT can report failure after
// durable success. Force that interleaving, then compare the API result with
// actual request/content/telemetry rows, not with mock call counts.
func TestWithTxCancellationAtCommitReturnsDurableOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.db")
	seed := openTestDB(t, path)
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	base, err := sqlitedriver.NewConnector(path + "?_pragma=foreign_keys(ON)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sqlDB := sql.OpenDB(commitCancellationConnector{Connector: base, cancel: cancel})
	sqlDB.SetMaxOpenConns(1)
	db := &DB{db: sqlDB, keyring: testKeyring(t)}
	t.Cleanup(func() { _ = db.Close() })
	record := sampleRecord()
	err = db.WithTx(ctx, func(tx storage.Tx) error { return tx.InsertRequest(record) })
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("caller cancellation did not occur at COMMIT")
	}
	if err != nil {
		t.Errorf("WithTx reported failure after SQLite committed: %v", err)
	}
	for _, table := range []string{"requests", "routing_decisions", "classifier_judgments"} {
		assertCount(t, db.SQL(), table, 1)
	}
	assertCount(t, db.SQL(), "candidate_scores", 2)
	assertCount(t, db.SQL(), "content_blobs", 5)
	got, err := db.GetRequest(context.Background(), record.ID)
	if err != nil || !reflect.DeepEqual(got, record) {
		t.Fatalf("durable record differs (content omitted): err=%v", err)
	}
}

// This local connector wraps real SQLite connections without driver registration
// or global hooks. It fixes the otherwise scheduling-dependent cancellation
// point immediately after successful COMMIT, reproducing modernc's behavior in
// stmt.exec: a late ctx.Err can replace statement success.
type commitCancellationConnector struct {
	driver.Connector
	cancel context.CancelFunc
}

func (c commitCancellationConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &commitCancellationConn{Conn: conn, cancel: c.cancel}, nil
}

type commitCancellationConn struct {
	driver.Conn
	cancel context.CancelFunc
}

func (c *commitCancellationConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	if err == nil && query == "COMMIT" {
		c.cancel()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return result, err
}
