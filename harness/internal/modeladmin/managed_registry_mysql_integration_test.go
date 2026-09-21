//go:build mysql_integration

package modeladmin

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	frameworkmysql "github.com/AGenUI/agenui-studio/harness/framework/mysql"
)

// TestManagedModelRegistryMySQLSerializesConcurrentFirstDefaults proves the
// empty-tenant gap-lock case against a real MySQL/InnoDB driver. Two independent
// pools emulate separate Harness instances; both may create a different first
// default, but the committed tenant snapshot must contain exactly one default.
//
// Run with the repository's standard structured MySQL fixture variables:
//
//	HARNESS_MYSQL_TEST_HOST=127.0.0.1 \
//	HARNESS_MYSQL_TEST_PORT=3306 \
//	HARNESS_MYSQL_TEST_USER=root \
//	HARNESS_MYSQL_TEST_PASSWORD='' \
//	HARNESS_MYSQL_TEST_DATABASE=harness_test \
//	go test -tags mysql_integration ./internal/modeladmin \
//	  -run TestManagedModelRegistryMySQLSerializesConcurrentFirstDefaults -count=10
func TestManagedModelRegistryMySQLSerializesConcurrentFirstDefaults(t *testing.T) {
	base, configured := modelAdminMySQLIntegrationConfig(t)
	if !configured {
		t.Skip("HARNESS_MYSQL_TEST_HOST/PORT/USER/PASSWORD/DATABASE not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	open := func(name string) *SQLManagedRegistry {
		config := base
		config.Name = name
		if err := frameworkmysql.DBInit(config); err != nil {
			t.Fatalf("initialize %s: %v", name, err)
		}
		db := frameworkmysql.GetDB(name)
		if db == nil {
			t.Fatalf("framework/mysql.GetDB(%q) returned nil", name)
		}
		db.SetMaxOpenConns(2)
		db.SetMaxIdleConns(2)
		t.Cleanup(func() { _ = db.Close() })
		return NewSQLManagedRegistry(db)
	}
	registryA := open("modeladmin-default-lock-a")
	registryB := open("modeladmin-default-lock-b")
	if err := ApplyMySQLSchema(ctx, registryA.db); err != nil {
		t.Fatal(err)
	}
	tenantID := fmt.Sprintf("modeladmin-default-lock-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, _ = registryA.db.ExecContext(cleanupCtx, `DELETE FROM model_providers WHERE tenant_id=?`, tenantID)
	})

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, operation := range []func() error{
		func() error {
			_, err := registryA.Save(ctx, tenantID, "instance-a", ProviderDefinition{
				ID: "first-a", Version: "v1", Protocol: "mock", IsDefault: true, DefaultModel: "m1",
			}, 0)
			return err
		},
		func() error {
			_, err := registryB.Save(ctx, tenantID, "instance-b", ProviderDefinition{
				ID: "first-b", Version: "v1", Protocol: "mock", IsDefault: true, DefaultModel: "m2",
			}, 0)
			return err
		},
	} {
		wg.Add(1)
		go func(operation func() error) {
			defer wg.Done()
			<-start
			errs <- operation()
		}(operation)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent first default: %v", err)
		}
	}
	providers, err := registryA.List(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defaults := 0
	for _, provider := range providers {
		if provider.Definition.IsDefault {
			defaults++
		}
	}
	if len(providers) != 2 || defaults != 1 {
		t.Fatalf("committed providers=%d defaults=%d: %#v", len(providers), defaults, providers)
	}
}

func modelAdminMySQLIntegrationConfig(t *testing.T) (frameworkmysql.Config, bool) {
	t.Helper()
	keys := [...]string{
		"HARNESS_MYSQL_TEST_HOST",
		"HARNESS_MYSQL_TEST_PORT",
		"HARNESS_MYSQL_TEST_USER",
		"HARNESS_MYSQL_TEST_PASSWORD",
		"HARNESS_MYSQL_TEST_DATABASE",
	}
	values := make(map[string]string, len(keys))
	configured := false
	for _, key := range keys {
		value, ok := os.LookupEnv(key)
		if ok {
			configured = true
			values[key] = value
		}
	}
	if !configured {
		return frameworkmysql.Config{}, false
	}
	for _, key := range keys {
		value, ok := values[key]
		if !ok || (key != "HARNESS_MYSQL_TEST_PASSWORD" && strings.TrimSpace(value) == "") {
			t.Fatal("invalid modeladmin MySQL fixture configuration")
		}
	}
	port, err := strconv.Atoi(strings.TrimSpace(values["HARNESS_MYSQL_TEST_PORT"]))
	if err != nil || port < 1 || port > 65535 {
		t.Fatal("invalid modeladmin MySQL fixture configuration")
	}
	return frameworkmysql.Config{
		DBName: values["HARNESS_MYSQL_TEST_DATABASE"], User: values["HARNESS_MYSQL_TEST_USER"],
		Password: values["HARNESS_MYSQL_TEST_PASSWORD"], Host: values["HARNESS_MYSQL_TEST_HOST"], Port: port,
		Charset: "utf8mb4", ConnectTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
	}, true
}
