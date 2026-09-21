package app

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	storagesqlite "github.com/AGenUI/agenui-studio/harness/internal/storage/sqlite"
)

// newSharedSQLTestDatabase returns an in-memory *sql.DB backed by a custom
// driver that records any Exec/Query attempt. It exists to support internal
// preflight tests (agent_registry_composition_test.go and friends) that need
// a real *sql.DB handle to pass into buildFormalComposition without ever
// executing SQL.
//
// The helper is strictly for internal-composition unit tests. Public SDK
// ownership semantics are covered by external_sql_database_test.go.
func newSharedSQLTestDatabase(t *testing.T) (*sql.DB, *sharedSQLTestDriver) {
	t.Helper()

	recorder := &sharedSQLTestDriver{}
	name := fmt.Sprintf("harness-app-shared-sql-%d", sharedSQLTestDriverID.Add(1))
	sql.Register(name, recorder)
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, recorder
}

var sharedSQLTestDriverID atomic.Uint64

type sharedSQLTestDriver struct {
	execCalls atomic.Int64
}

func (d *sharedSQLTestDriver) Open(string) (driver.Conn, error) {
	return &sharedSQLTestConn{driver: d}, nil
}

type sharedSQLTestConn struct {
	driver *sharedSQLTestDriver
}

func (c *sharedSQLTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("shared SQL test driver does not support prepared statements")
}

func (c *sharedSQLTestConn) Close() error { return nil }

func (c *sharedSQLTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("shared SQL test driver does not support transactions")
}

func (c *sharedSQLTestConn) Ping(context.Context) error { return nil }

func (c *sharedSQLTestConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	c.driver.execCalls.Add(1)
	return nil, errors.New("unexpected SQL execution during shared database assembly")
}

var _ driver.Pinger = (*sharedSQLTestConn)(nil)
var _ driver.ExecerContext = (*sharedSQLTestConn)(nil)

func TestBuildStoresAcceptsCallerOwnedSQLitePool(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "shared.sqlite")
	raw, err := json.Marshal(SQLiteConfig{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	config := StorageConfig{Backend: "sqlite", Config: raw}
	if _, err := migrateStorage(context.Background(), config, false, storageSchemaUp, defaultBuildDependencies()); err != nil {
		t.Fatalf("migrate shared sqlite: %v", err)
	}
	sharedDB, err := storagesqlite.OpenDatabase(dbPath)
	if err != nil {
		t.Fatalf("open shared sqlite: %v", err)
	}
	t.Cleanup(func() { _ = sharedDB.Close() })

	built, err := buildStoresWithSharedSQLDatabase(
		context.Background(), config, observability.NoopLogger{}, false,
		defaultBuildDependencies(), sharedDB,
	)
	if err != nil {
		t.Fatalf("build stores with shared sqlite: %v", err)
	}
	if built.SQLDB != sharedDB {
		t.Fatalf("SQLDB = %p, want caller pool %p", built.SQLDB, sharedDB)
	}
	if err := built.Close(); err != nil {
		t.Fatalf("close shared stores: %v", err)
	}
	if err := sharedDB.PingContext(context.Background()); err != nil {
		t.Fatalf("Harness closed caller-owned sqlite pool: %v", err)
	}
}
