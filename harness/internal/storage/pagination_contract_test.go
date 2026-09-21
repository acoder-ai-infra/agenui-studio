package storage_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	frameworkmysql "github.com/AGenUI/agenui-studio/harness/framework/mysql"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/storage/memory"
	storagemysql "github.com/AGenUI/agenui-studio/harness/internal/storage/mysql"
	storagesqlite "github.com/AGenUI/agenui-studio/harness/internal/storage/sqlite"
	"github.com/DATA-DOG/go-sqlmock"
)

const mysqlPaginationFixtureDelete = `DELETE FROM messages
	WHERE tenant_id = ? AND session_id = ? AND id IN (?, ?, ?, ?, ?)`

func clearMySQLPaginationFixture(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, mysqlPaginationFixtureDelete,
		"t1", "s", "m1", "m2", "m3", "m4", "m5",
	)
	return err
}

func TestClearMySQLPaginationFixtureTargetsOnlyContractRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectExec(regexp.QuoteMeta(mysqlPaginationFixtureDelete)).
		WithArgs("t1", "s", "m1", "m2", "m3", "m4", "m5").
		WillReturnResult(sqlmock.NewResult(0, 5))
	if err := clearMySQLPaginationFixture(context.Background(), db); err != nil {
		t.Fatalf("clearMySQLPaginationFixture() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// P1-9: all storage backends must agree on history pagination semantics. The
// initial page is the newest window in chronological order; before_message_id
// pages upward to the next older window; after_message_id supports forward sync.
func runPaginationContract(t *testing.T, store storage.MessageStore) {
	ctx := ctxT("t1")
	base := time.Unix(1_700_000_000, 0).UTC()
	ids := []string{"m1", "m2", "m3", "m4", "m5"}
	for i, id := range ids {
		if err := store.Append(ctx, &storage.Message{
			ID: id, SessionID: "s", TenantID: "t1", Role: "user",
			ContentPreview: id, CreatedAt: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
	previews := func(p storage.MessagePage) []string {
		out := make([]string, len(p.Items))
		for i, m := range p.Items {
			out[i] = m.ContentPreview
		}
		return out
	}
	eq := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	// before m5, limit 2 → the two most recent messages older than m5, ascending.
	page, err := store.List(ctx, storage.MessageListQuery{SessionID: "s", BeforeMessageID: "m5", Limit: 2})
	if err != nil {
		t.Fatalf("list before m5: %v", err)
	}
	if got := previews(page); !eq(got, []string{"m3", "m4"}) {
		t.Fatalf("before m5 limit 2 = %v, want [m3 m4] (most-recent window before cursor)", got)
	}
	if !page.HasMore {
		t.Fatalf("before m5 limit 2 should report HasMore (m1,m2 remain)")
	}

	// before m3, limit 5 → all older, ascending, no more.
	page, err = store.List(ctx, storage.MessageListQuery{SessionID: "s", BeforeMessageID: "m3", Limit: 5})
	if err != nil {
		t.Fatalf("list before m3: %v", err)
	}
	if got := previews(page); !eq(got, []string{"m1", "m2"}) {
		t.Fatalf("before m3 = %v, want [m1 m2]", got)
	}
	if page.HasMore {
		t.Fatalf("before m3 should not report HasMore")
	}

	// no cursor → newest window, still returned oldest-first within the page.
	page, err = store.List(ctx, storage.MessageListQuery{SessionID: "s", Limit: 3})
	if err != nil {
		t.Fatalf("list no cursor: %v", err)
	}
	if got := previews(page); !eq(got, []string{"m3", "m4", "m5"}) {
		t.Fatalf("no cursor limit 3 = %v, want [m3 m4 m5]", got)
	}
	if !page.HasMore || page.NextBeforeMessageID != "m3" || page.NextAfterMessageID != "m5" {
		t.Fatalf("initial page cursors = %#v, want older and forward cursors", page)
	}

	// after m3 → newer messages in chronological order for multi-device sync.
	page, err = store.List(ctx, storage.MessageListQuery{SessionID: "s", AfterMessageID: "m3", Limit: 5})
	if err != nil {
		t.Fatalf("list after m3: %v", err)
	}
	if got := previews(page); !eq(got, []string{"m4", "m5"}) {
		t.Fatalf("after m3 = %v, want [m4 m5]", got)
	}
}

func TestPaginationContract_Memory(t *testing.T) {
	runPaginationContract(t, memory.New().Stores().Messages)
}

func TestPaginationContract_SQLite(t *testing.T) {
	sq, err := storagesqlite.Open(filepath.Join(t.TempDir(), "pg.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := sq.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	runPaginationContract(t, sq.Stores().Messages)
}

func TestMySQLPaginationConfig(t *testing.T) {
	t.Run("maps structured environment", func(t *testing.T) {
		cfg, configured, err := paginationMySQLConfig(paginationMapLookup(map[string]string{
			"HARNESS_MYSQL_TEST_HOST":     "mysql.test",
			"HARNESS_MYSQL_TEST_PORT":     "3308",
			"HARNESS_MYSQL_TEST_USER":     "pagination_user",
			"HARNESS_MYSQL_TEST_PASSWORD": "fixture-secret",
			"HARNESS_MYSQL_TEST_DATABASE": "harness_test",
		}))
		if err != nil {
			t.Fatalf("paginationMySQLConfig() error = %v", err)
		}
		if !configured {
			t.Fatal("paginationMySQLConfig() configured = false, want true")
		}
		if cfg.Name != "ledger-pagination-test" || cfg.DBName != "harness_test" || cfg.Host != "mysql.test" || cfg.Port != 3308 {
			t.Fatalf("fixture identity/address was not preserved: name=%q database=%q host=%q port=%d", cfg.Name, cfg.DBName, cfg.Host, cfg.Port)
		}
		if cfg.User != "pagination_user" || cfg.Password != "fixture-secret" {
			t.Fatal("fixture credentials were not preserved")
		}
		if cfg.Charset != "utf8mb4" || cfg.MaxOpenConns != 4 || cfg.MaxIdleConns != 4 {
			t.Fatalf("fixture defaults = (charset %q, maxOpen %d, maxIdle %d), want (utf8mb4, 4, 4)", cfg.Charset, cfg.MaxOpenConns, cfg.MaxIdleConns)
		}
	})

	t.Run("accepts explicitly empty password", func(t *testing.T) {
		cfg, configured, err := paginationMySQLConfig(paginationMapLookup(map[string]string{
			"HARNESS_MYSQL_TEST_HOST":     "127.0.0.1",
			"HARNESS_MYSQL_TEST_PORT":     "3306",
			"HARNESS_MYSQL_TEST_USER":     "root",
			"HARNESS_MYSQL_TEST_PASSWORD": "",
			"HARNESS_MYSQL_TEST_DATABASE": "harness_test",
		}))
		if err != nil || !configured {
			t.Fatalf("paginationMySQLConfig() = (_, %t, %v), want configured without error", configured, err)
		}
		if cfg.Password != "" {
			t.Fatal("paginationMySQLConfig() did not preserve the explicitly empty password")
		}
	})

	t.Run("all variables unset disables fixture", func(t *testing.T) {
		_, configured, err := paginationMySQLConfig(paginationMapLookup(nil))
		if err != nil || configured {
			t.Fatalf("paginationMySQLConfig() = (_, %t, %v), want (_, false, nil)", configured, err)
		}
	})

	t.Run("rejects partial and invalid configuration with a fixed redacted error", func(t *testing.T) {
		const wantError = "invalid pagination MySQL fixture configuration"
		tests := []struct {
			name string
			env  map[string]string
		}{
			{name: "partial", env: map[string]string{"HARNESS_MYSQL_TEST_PASSWORD": "fixture-secret"}},
			{name: "password unset", env: map[string]string{
				"HARNESS_MYSQL_TEST_HOST": "mysql.test", "HARNESS_MYSQL_TEST_PORT": "3308",
				"HARNESS_MYSQL_TEST_USER": "pagination_user", "HARNESS_MYSQL_TEST_DATABASE": "harness_test",
			}},
			{name: "invalid port", env: map[string]string{
				"HARNESS_MYSQL_TEST_HOST": "mysql.test", "HARNESS_MYSQL_TEST_PORT": "not-a-port",
				"HARNESS_MYSQL_TEST_USER": "pagination_user", "HARNESS_MYSQL_TEST_PASSWORD": "fixture-secret",
				"HARNESS_MYSQL_TEST_DATABASE": "harness_test",
			}},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				_, configured, err := paginationMySQLConfig(paginationMapLookup(test.env))
				if !configured || !errors.Is(err, errInvalidPaginationMySQLFixture) {
					t.Fatalf("paginationMySQLConfig() = (_, %t, %v), want configured with fixed error", configured, err)
				}
				if err.Error() != wantError || strings.Contains(err.Error(), "fixture-secret") {
					t.Fatalf("paginationMySQLConfig() error = %q, want fixed redacted error", err)
				}
			})
		}
	})
}

var errInvalidPaginationMySQLFixture = errors.New("invalid pagination MySQL fixture configuration")

func paginationMySQLConfig(lookup func(string) (string, bool)) (frameworkmysql.Config, bool, error) {
	keys := [...]string{
		"HARNESS_MYSQL_TEST_HOST",
		"HARNESS_MYSQL_TEST_PORT",
		"HARNESS_MYSQL_TEST_USER",
		"HARNESS_MYSQL_TEST_PASSWORD",
		"HARNESS_MYSQL_TEST_DATABASE",
	}
	values := make(map[string]string, len(keys))
	present := make(map[string]bool, len(keys))
	configured := false
	for _, key := range keys {
		value, ok := lookup(key)
		values[key] = value
		present[key] = ok
		configured = configured || ok
	}
	if !configured {
		return frameworkmysql.Config{}, false, nil
	}
	for _, key := range keys {
		if !present[key] || (key != "HARNESS_MYSQL_TEST_PASSWORD" && strings.TrimSpace(values[key]) == "") {
			return frameworkmysql.Config{}, true, errInvalidPaginationMySQLFixture
		}
	}
	port, err := strconv.Atoi(strings.TrimSpace(values["HARNESS_MYSQL_TEST_PORT"]))
	if err != nil || port < 1 || port > 65535 {
		return frameworkmysql.Config{}, true, errInvalidPaginationMySQLFixture
	}

	return frameworkmysql.Config{
		Name:         "ledger-pagination-test",
		DBName:       strings.TrimSpace(values["HARNESS_MYSQL_TEST_DATABASE"]),
		User:         strings.TrimSpace(values["HARNESS_MYSQL_TEST_USER"]),
		Password:     values["HARNESS_MYSQL_TEST_PASSWORD"],
		Host:         strings.TrimSpace(values["HARNESS_MYSQL_TEST_HOST"]),
		Port:         port,
		Charset:      "utf8mb4",
		MaxOpenConns: 4,
		MaxIdleConns: 4,
	}, true, nil
}

func paginationMapLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

// TestPaginationContract_MySQL runs the same contract against MySQL when the
// HARNESS_MYSQL_TEST_* variables are provided, so CI can exercise the real backend.
// It is skipped when all variables are unset so local `go test ./...` stays hermetic.
func TestPaginationContract_MySQL(t *testing.T) {
	config, configured, err := paginationMySQLConfig(os.LookupEnv)
	if err != nil {
		t.Fatalf("MySQL pagination fixture configuration is invalid: %v", err)
	}
	if !configured {
		t.Skip("HARNESS_MYSQL_TEST_HOST/PORT/USER/PASSWORD/DATABASE not set")
	}
	if err := frameworkmysql.DBInit(config); err != nil {
		t.Fatalf("framework/mysql.DBInit() error = %v", err)
	}
	db := frameworkmysql.GetDB(config.Name)
	if db == nil {
		t.Fatalf("framework/mysql.GetDB(%q) returned nil", config.Name)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("MySQL pagination DB Close() error = %v", err)
		}
	})

	my := storagemysql.New(db)
	if err := my.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := clearMySQLPaginationFixture(context.Background(), db); err != nil {
		t.Fatalf("clear stale MySQL pagination fixture: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := clearMySQLPaginationFixture(ctx, db); err != nil {
			t.Errorf("clear MySQL pagination fixture after test: %v", err)
		}
	})
	runPaginationContract(t, my.Stores().Messages)
}
