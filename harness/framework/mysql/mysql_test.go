package mysql

import (
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

var (
	_ func(Config) error   = DBInit
	_ func(string) *sql.DB = GetDB
	_ dbInitializer        = defaultDBInit
	_ dbGetter             = defaultGetDB
)

func TestDBInitCallsInitializerExactlyOnce(t *testing.T) {
	calls := 0
	err := dbInit(validDirectConfig(), func(map[string]*DBConf) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("dbInit() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("initializer calls = %d, want 1", calls)
	}
}

func TestDBInitRejectsBlankRequiredNamesBeforeInitializer(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		field  string
	}{
		{name: "empty name", config: Config{DBName: "db", Host: "localhost", User: "user", Port: 3306}, field: "Name"},
		{name: "blank name", config: Config{Name: "  ", DBName: "db", Host: "localhost", User: "user", Port: 3306}, field: "Name"},
		{name: "empty database", config: Config{Name: "main", Host: "localhost", User: "user", Port: 3306}, field: "DBName"},
		{name: "blank database", config: Config{Name: "main", DBName: "  ", Host: "localhost", User: "user", Port: 3306}, field: "DBName"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertDBInitRejectedBeforeInitializer(t, test.config, test.field)
		})
	}
}

func TestDBInitDirectModeRequiresHostUserAndValidPort(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		field  string
	}{
		{name: "host", mutate: func(c *Config) { c.Host = "" }, field: "Host"},
		{name: "user", mutate: func(c *Config) { c.User = "" }, field: "User"},
		{name: "zero port", mutate: func(c *Config) { c.Port = 0 }, field: "Port"},
		{name: "large port", mutate: func(c *Config) { c.Port = 65536 }, field: "Port"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validDirectConfig()
			test.mutate(&config)
			assertDBInitRejectedBeforeInitializer(t, config, test.field)
		})
	}
}

func TestDBInitRejectsNegativeTimeoutAndPoolValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		field  string
	}{
		{name: "connect timeout", mutate: func(c *Config) { c.ConnectTimeout = -time.Second }, field: "ConnectTimeout"},
		{name: "read timeout", mutate: func(c *Config) { c.ReadTimeout = -time.Second }, field: "ReadTimeout"},
		{name: "write timeout", mutate: func(c *Config) { c.WriteTimeout = -time.Second }, field: "WriteTimeout"},
		{name: "max open", mutate: func(c *Config) { c.MaxOpenConns = -1 }, field: "MaxOpenConns"},
		{name: "max idle", mutate: func(c *Config) { c.MaxIdleConns = -1 }, field: "MaxIdleConns"},
		{name: "max lifetime", mutate: func(c *Config) { c.ConnMaxLifetime = -time.Second }, field: "ConnMaxLifetime"},
		{name: "max idle time", mutate: func(c *Config) { c.ConnMaxIdleTime = -time.Second }, field: "ConnMaxIdleTime"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validDirectConfig()
			test.mutate(&config)
			assertDBInitRejectedBeforeInitializer(t, config, test.field)
		})
	}
}

func TestDBInitWrapsAndSanitizesInitializerError(t *testing.T) {
	config := validDirectConfig()
	connectionText := config.User + ":" + config.Password + "@tcp(" + config.Host + ")/" + config.DBName
	cause := errors.New("DBInit rejected " + connectionText)
	err := dbInit(config, func(map[string]*DBConf) error { return cause })
	if !errors.Is(err, cause) {
		t.Fatalf("dbInit() error = %v, want wrapped initializer cause", err)
	}
	if strings.Contains(err.Error(), config.Password) || strings.Contains(err.Error(), config.User) || strings.Contains(err.Error(), config.Host) {
		t.Fatalf("dbInit() public error leaked connection details: %q", err)
	}
}

func TestDBInitMapsDirectConfigurationExactly(t *testing.T) {
	config := Config{
		Name: "harness", DBName: "harness_db", User: "harness_user", Password: "fixture-password",
		Host: "mysql.internal", Port: 3307, Charset: "latin1",
		ConnectTimeout: 3 * time.Second, ReadTimeout: 4 * time.Second, WriteTimeout: 5 * time.Second,
		MaxOpenConns: 11, MaxIdleConns: 7, ConnMaxLifetime: 30 * time.Minute,
		ConnMaxIdleTime: 2 * time.Minute, InterpolateParams: true,
	}
	var got *DBConf
	err := dbInit(config, func(settings map[string]*DBConf) error {
		got = settings[config.Name]
		return nil
	})
	if err != nil {
		t.Fatalf("dbInit() error = %v", err)
	}
	want := &DBConf{
		Dbname: "harness_db", User: "harness_user", Password: "fixture-password",
		Host: "mysql.internal", Port: 3307, Charset: "latin1",
		ConnectTimeout: 3 * time.Second, ReadTimeout: 4 * time.Second, WriteTimeout: 5 * time.Second,
		MaxOpenConns: 11, MaxIdleConns: 7, ConnMaxLifeTime: 30 * time.Minute,
		ConnMaxIdleTime: 2 * time.Minute, InterpolateParams: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("direct DBConf = %#v, want %#v", got, want)
	}
}

func TestGetDBForwardsNameAndReturnsExactPool(t *testing.T) {
	db := newMockDB(t)
	var gotName string
	got := getDB("harness", func(name string) *sql.DB {
		gotName = name
		return db
	})
	if gotName != "harness" {
		t.Fatalf("getter name = %q, want harness", gotName)
	}
	if got != db {
		t.Fatalf("getDB() = %p, want %p", got, db)
	}
}

func TestGetDBPreservesMissingPoolSemantics(t *testing.T) {
	calls := 0
	got := getDB("missing", func(name string) *sql.DB {
		calls++
		if name != "missing" {
			t.Fatalf("getter name = %q, want missing", name)
		}
		return nil
	})
	if got != nil {
		t.Fatalf("getDB() = %p, want nil", got)
	}
	if calls != 1 {
		t.Fatalf("getter calls = %d, want 1", calls)
	}
}

func validDirectConfig() Config {
	return Config{
		Name: "harness", DBName: "harness_db", User: "harness_user", Password: "fixture-password",
		Host: "mysql.internal", Port: 3306,
	}
}

func assertDBInitRejectedBeforeInitializer(t *testing.T, config Config, field string) {
	t.Helper()
	calls := 0
	err := dbInit(config, func(map[string]*DBConf) error {
		calls++
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), field) {
		t.Fatalf("dbInit() error = %v, want %s validation error", err, field)
	}
	if calls != 0 {
		t.Fatalf("initializer calls = %d, want zero", calls)
	}
}

func newMockDB(t *testing.T) *sql.DB {
	t.Helper()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
