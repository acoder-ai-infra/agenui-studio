package agentregistry

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

func TestSQLStoreCreateUsesSharedTransaction(t *testing.T) {
	db, state := openScriptDB(t,
		dbStep{kind: "exec", contains: "agent_version_digest, type_version_digest", affected: 1},
		dbStep{kind: "exec", contains: "snapshot_ref_digest, agent_mode_digest, agent_hash_digest", affected: 1},
		dbStep{kind: "exec", contains: "event_id_digest", affected: 1},
	)
	store, err := NewSQLStore(db)
	if err != nil {
		t.Fatalf("new sql store: %v", err)
	}
	record := sqlTestRecord(t)
	if err := store.Create(context.Background(), record, sqlTestEvent(EventAgentRegistered, record)); err != nil {
		t.Fatalf("create: %v", err)
	}
	state.assertDone(t)
	if state.begins != 1 || state.commits != 1 || state.rollbacks != 0 {
		t.Fatalf("unexpected transaction lifecycle: begin=%d commit=%d rollback=%d", state.begins, state.commits, state.rollbacks)
	}
}

func TestSQLStoreCreateRollsBackWhenAuditWriteFails(t *testing.T) {
	record := sqlTestRecord(t)
	db, state := openScriptDB(t,
		dbStep{kind: "exec", contains: "INSERT INTO agent_registry_entries", affected: 1},
		dbStep{kind: "exec", contains: "INSERT INTO agent_registry_config_snapshots", affected: 1},
		dbStep{kind: "exec", contains: "INSERT INTO agent_registry_audit_events", err: errors.New("audit unavailable")},
	)
	store, _ := NewSQLStore(db)
	err := store.Create(context.Background(), record, sqlTestEvent(EventAgentRegistered, record))
	if !errors.Is(err, ErrAuditWrite) {
		t.Fatalf("create error = %v, want ErrAuditWrite", err)
	}
	state.assertDone(t)
	if state.commits != 0 || state.rollbacks != 1 {
		t.Fatalf("audit failure must rollback registration: commit=%d rollback=%d", state.commits, state.rollbacks)
	}
}

func TestSQLStoreCreateRollsBackWhenAnySnapshotFails(t *testing.T) {
	svc := NewService(WithClock(func() time.Time { return time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC) }))
	cfg := extendedConfig("multi_snapshot", "v1")
	cfg.Capability.ExecutionModes = []ExecutionMode{ExecutionModeSingleAgent, ExecutionModeDeepAgent}
	record, err := svc.compile(cfg, ResolvedDependencies{}, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	db, state := openScriptDB(t,
		dbStep{kind: "exec", contains: "INSERT INTO agent_registry_entries", affected: 1},
		dbStep{kind: "exec", contains: "INSERT INTO agent_registry_config_snapshots", affected: 1},
		dbStep{kind: "exec", contains: "INSERT INTO agent_registry_config_snapshots", err: errors.New("snapshot unavailable")},
	)
	store, _ := NewSQLStore(db)
	err = store.Create(context.Background(), record, sqlTestEvent(EventAgentRegistered, record))
	if err == nil {
		t.Fatal("snapshot failure must fail registration")
	}
	state.assertDone(t)
	if state.commits != 0 || state.rollbacks != 1 {
		t.Fatalf("snapshot failure must rollback registration: commit=%d rollback=%d", state.commits, state.rollbacks)
	}
}

func TestSQLStoreDisableKeepsActivationAndWritesAudit(t *testing.T) {
	record := sqlTestRecord(t)
	db, state := openScriptDB(t,
		dbStep{kind: "query", contains: "SELECT agent_id", columns: strings.Split(sqlRecordColumns, ", "), rows: [][]driver.Value{sqlTestRow(record)}},
		dbStep{kind: "exec", contains: "UPDATE agent_registry_entries SET status=", notContains: "activated_at_ms=", affected: 1},
		dbStep{kind: "exec", contains: "INSERT INTO agent_registry_audit_events", affected: 1},
	)
	store, _ := NewSQLStore(db)
	if err := store.UpdateStatus(context.Background(), AgentRef{AgentID: record.Config.AgentID, Version: record.Config.Version}, AgentStatusDisabled, record.ActivatedAtMS+1000, sqlTestEvent(EventAgentDisabled, record)); err != nil {
		t.Fatalf("disable: %v", err)
	}
	state.assertDone(t)
	if state.commits != 1 || state.rollbacks != 0 {
		t.Fatalf("disable transaction lifecycle: commit=%d rollback=%d", state.commits, state.rollbacks)
	}
}

func TestSQLStoreEnableRefreshesActivationAndWritesAudit(t *testing.T) {
	record := sqlTestRecord(t)
	record.Config.Status = AgentStatusDisabled
	record.Card.Status = AgentStatusDisabled
	record.Card.CardHash, _ = computeCardHash(record.Card)
	db, state := openScriptDB(t,
		dbStep{kind: "query", contains: "SELECT agent_id", columns: strings.Split(sqlRecordColumns, ", "), rows: [][]driver.Value{sqlTestRow(record)}},
		dbStep{kind: "exec", contains: "activated_at_ms=?", affected: 1},
		dbStep{kind: "exec", contains: "INSERT INTO agent_registry_audit_events", affected: 1},
	)
	store, _ := NewSQLStore(db)
	if err := store.UpdateStatus(context.Background(), AgentRef{AgentID: record.Config.AgentID, Version: record.Config.Version}, AgentStatusEnabled, record.ActivatedAtMS+1000, sqlTestEvent(EventAgentEnabled, record)); err != nil {
		t.Fatalf("enable: %v", err)
	}
	state.assertDone(t)
	if state.commits != 1 || state.rollbacks != 0 {
		t.Fatalf("enable transaction lifecycle: commit=%d rollback=%d", state.commits, state.rollbacks)
	}
}

func TestSQLStoreGrayAuditFailureRollsBackMutation(t *testing.T) {
	record := sqlTestRecord(t)
	db, state := openScriptDB(t,
		dbStep{kind: "query", contains: "SELECT agent_id", columns: strings.Split(sqlRecordColumns, ", "), rows: [][]driver.Value{sqlTestRow(record)}},
		dbStep{kind: "exec", contains: "SET gray_percent=?", affected: 1},
		dbStep{kind: "exec", contains: "INSERT INTO agent_registry_audit_events", err: errors.New("audit unavailable")},
	)
	store, _ := NewSQLStore(db)
	err := store.UpdateGrayPercent(context.Background(), AgentRef{AgentID: record.Config.AgentID, Version: record.Config.Version}, 25, sqlTestEvent(EventGrayPercentChanged, record))
	if !errors.Is(err, ErrAuditWrite) {
		t.Fatalf("gray update error = %v, want ErrAuditWrite", err)
	}
	state.assertDone(t)
	if state.commits != 0 || state.rollbacks != 1 {
		t.Fatalf("audit failure must rollback gray update: commit=%d rollback=%d", state.commits, state.rollbacks)
	}
}

func TestSQLStoreOptimisticUpdateConflict(t *testing.T) {
	record := sqlTestRecord(t)
	db, state := openScriptDB(t,
		dbStep{kind: "query", contains: "SELECT agent_id", columns: strings.Split(sqlRecordColumns, ", "), rows: [][]driver.Value{sqlTestRow(record)}},
		dbStep{kind: "exec", contains: "AND revision=?", affected: 0},
	)
	store, err := NewSQLStore(db)
	if err != nil {
		t.Fatalf("new sql store: %v", err)
	}
	err = store.UpdateStatus(context.Background(), AgentRef{AgentID: record.Config.AgentID, Version: record.Config.Version}, AgentStatusDisabled, time.Now().UnixMilli(), sqlTestEvent(EventAgentDisabled, record))
	if !errors.Is(err, ErrStoreConflict) {
		t.Fatalf("expected optimistic conflict, got %v", err)
	}
	state.assertDone(t)
	if state.commits != 0 || state.rollbacks != 1 {
		t.Fatalf("conflicted transaction must rollback: commit=%d rollback=%d", state.commits, state.rollbacks)
	}
}

func TestSQLStoreGetDecodesCanonicalRecord(t *testing.T) {
	record := sqlTestRecord(t)
	columns := strings.Split(sqlRecordColumns, ", ")
	row := sqlTestRow(record)
	db, state := openScriptDB(t, dbStep{kind: "query", contains: "status=?", columns: columns, rows: [][]driver.Value{row}})
	store, _ := NewSQLStore(db)
	got, err := store.Get(context.Background(), StoreLookup{AgentID: record.Config.AgentID})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Effective.ConfigHash != record.Effective.ConfigHash || got.Card.ConfigHash != record.Card.ConfigHash {
		t.Fatalf("stored contract drift: got=%#v want=%#v", got, record)
	}
	state.assertDone(t)
}

func TestSQLStoreListsCanonicalReadModels(t *testing.T) {
	first := sqlTestRecord(t)
	secondService := NewService(WithClock(func() time.Time { return first.RegisteredAt.Add(time.Second) }))
	second, err := secondService.compile(extendedConfig("sql_agent_2", "v1"), ResolvedDependencies{}, nil)
	if err != nil {
		t.Fatalf("compile second record: %v", err)
	}
	db, state := openScriptDB(t, dbStep{
		kind: "query", contains: "ORDER BY agent_id, version", columns: strings.Split(sqlRecordColumns, ", "),
		rows: [][]driver.Value{sqlTestRow(first), sqlTestRow(second)},
	})
	store, _ := NewSQLStore(db)
	records, err := store.List(context.Background())
	if err != nil || len(records) != 2 {
		t.Fatalf("list records = %#v, %v", records, err)
	}
	for _, record := range records {
		if len(record.ConfigSnapshots) != 0 {
			t.Fatalf("SQL read model leaked write-side snapshots: %#v", record.ConfigSnapshots)
		}
	}
	state.assertDone(t)
}

func TestSQLStoreReadsFrozenConfigSnapshot(t *testing.T) {
	record := sqlTestRecord(t)
	snapshot := record.ConfigSnapshots[0]
	db, state := openScriptDB(t, dbStep{
		kind: "query", contains: "config_snapshot_ref=?", columns: sqlSnapshotTestColumns(), rows: [][]driver.Value{sqlTestSnapshotRow(record, snapshot)},
	})
	store, _ := NewSQLStore(db)
	got, err := store.GetConfigSnapshot(context.Background(), snapshot.Ref)
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if got.ConfigHash != snapshot.ConfigHash || got.ExecutionMode != snapshot.ExecutionMode {
		t.Fatalf("snapshot mismatch: got=%#v want=%#v", got, snapshot)
	}
	state.assertDone(t)
}

func TestSQLStoreReadsFrozenConfigSnapshotByMode(t *testing.T) {
	record := sqlTestRecord(t)
	snapshot := record.ConfigSnapshots[0]
	db, state := openScriptDB(t, dbStep{
		kind: "query", contains: "s.agent_id=? AND s.version=? AND s.execution_mode=?", columns: sqlSnapshotTestColumns(), rows: [][]driver.Value{sqlTestSnapshotRow(record, snapshot)},
	})
	store, _ := NewSQLStore(db)
	got, err := store.GetConfigSnapshotByMode(context.Background(), AgentRef{AgentID: snapshot.AgentID, Version: snapshot.Version}, snapshot.ExecutionMode)
	if err != nil {
		t.Fatalf("get snapshot by mode: %v", err)
	}
	if got.Ref != snapshot.Ref || got.ConfigHash != snapshot.ConfigHash {
		t.Fatalf("snapshot mismatch: got=%#v want=%#v", got, snapshot)
	}
	state.assertDone(t)
}

func TestSQLStoreRejectsProjectionDrift(t *testing.T) {
	record := sqlTestRecord(t)
	row := sqlTestRow(record)
	row[0] = "tampered_agent"
	db, state := openScriptDB(t, dbStep{
		kind: "query", contains: "version=?", columns: strings.Split(sqlRecordColumns, ", "), rows: [][]driver.Value{row},
	})
	store, _ := NewSQLStore(db)
	_, err := store.Get(context.Background(), StoreLookup{AgentID: record.Config.AgentID, Version: record.Config.Version})
	if !errors.Is(err, ErrStoreCorrupt) {
		t.Fatalf("get error = %v, want ErrStoreCorrupt", err)
	}
	state.assertDone(t)
}

func TestSQLStoreRejectsUnknownStatusWhenAllProjectionsAgree(t *testing.T) {
	record := sqlTestRecord(t)
	record.Config.Status = AgentStatus("bogus")
	record.Card.Status = AgentStatus("bogus")
	record.Card.CardHash, _ = computeCardHash(record.Card)
	row := sqlTestRow(record)
	db, state := openScriptDB(t, dbStep{
		kind: "query", contains: "version=?", columns: strings.Split(sqlRecordColumns, ", "), rows: [][]driver.Value{row},
	})
	store, _ := NewSQLStore(db)
	_, err := store.Get(context.Background(), StoreLookup{AgentID: record.Config.AgentID, Version: record.Config.Version})
	if !errors.Is(err, ErrStoreCorrupt) {
		t.Fatalf("get error = %v, want ErrStoreCorrupt", err)
	}
	state.assertDone(t)
}

func TestSQLStoreRejectsInvalidMutationValuesBeforeTransaction(t *testing.T) {
	record := sqlTestRecord(t)
	db, state := openScriptDB(t)
	store, _ := NewSQLStore(db)
	ref := AgentRef{AgentID: record.Config.AgentID, Version: record.Config.Version}
	if err := store.UpdateStatus(context.Background(), ref, AgentStatus("bogus"), 1, sqlTestEvent(EventAgentEnabled, record)); !errors.Is(err, ErrStoreCorrupt) {
		t.Fatalf("invalid status error = %v", err)
	}
	if err := store.UpdateGrayPercent(context.Background(), ref, 101, sqlTestEvent(EventGrayPercentChanged, record)); !errors.Is(err, ErrStoreCorrupt) {
		t.Fatalf("invalid gray error = %v", err)
	}
	state.assertDone(t)
	if state.begins != 0 {
		t.Fatalf("invalid mutation opened %d transactions", state.begins)
	}
}

func TestSQLStoreRejectsCanonicalHashDrift(t *testing.T) {
	record := sqlTestRecord(t)
	tests := map[string]func([]driver.Value){
		"effective config": func(row []driver.Value) {
			effective := cloneEffectiveConfig(record.Effective)
			effective.Definition.PromptRef = "prompt://tampered"
			data, _ := json.Marshal(effective)
			row[8] = string(data)
		},
		"capability card": func(row []driver.Value) {
			card := cloneCapabilityCard(record.Card)
			card.Description = "tampered"
			data, _ := json.Marshal(card)
			row[7] = string(data)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			row := sqlTestRow(record)
			mutate(row)
			db, state := openScriptDB(t, dbStep{
				kind: "query", contains: "version=?", columns: strings.Split(sqlRecordColumns, ", "), rows: [][]driver.Value{row},
			})
			store, _ := NewSQLStore(db)
			_, err := store.Get(context.Background(), StoreLookup{AgentID: record.Config.AgentID, Version: record.Config.Version})
			if !errors.Is(err, ErrStoreCorrupt) {
				t.Fatalf("get error = %v, want ErrStoreCorrupt", err)
			}
			state.assertDone(t)
		})
	}
}

func TestSQLStoreRejectsNonCanonicalSnapshotRef(t *testing.T) {
	record := sqlTestRecord(t)
	snapshot := record.ConfigSnapshots[0]
	snapshot.Ref = "agent-config://tampered/v1/" + snapshot.ConfigHash
	snapshot.Effective.ConfigSnapshotRef = snapshot.Ref
	db, state := openScriptDB(t, dbStep{
		kind: "query", contains: "config_snapshot_ref=?", columns: sqlSnapshotTestColumns(), rows: [][]driver.Value{sqlTestSnapshotRow(record, snapshot)},
	})
	store, _ := NewSQLStore(db)
	_, err := store.GetConfigSnapshot(context.Background(), snapshot.Ref)
	if !errors.Is(err, ErrStoreCorrupt) {
		t.Fatalf("get snapshot error = %v, want ErrStoreCorrupt", err)
	}
	state.assertDone(t)
}

func TestSQLStoreRejectsSnapshotPayloadHashDrift(t *testing.T) {
	record := sqlTestRecord(t)
	snapshot := record.ConfigSnapshots[0]
	snapshot.Effective.Definition.PromptRef = "prompt://tampered"
	db, state := openScriptDB(t, dbStep{
		kind: "query", contains: "config_snapshot_ref=?", columns: sqlSnapshotTestColumns(), rows: [][]driver.Value{sqlTestSnapshotRow(record, snapshot)},
	})
	store, _ := NewSQLStore(db)
	_, err := store.GetConfigSnapshot(context.Background(), snapshot.Ref)
	if !errors.Is(err, ErrStoreCorrupt) {
		t.Fatalf("get snapshot error = %v, want ErrStoreCorrupt", err)
	}
	state.assertDone(t)
}

func TestSQLStoreLookupUsesAllIdentityFieldsAndStableOrder(t *testing.T) {
	db, _ := openScriptDB(t)
	store, _ := NewSQLStore(db)
	query, args, err := store.getQuery(StoreLookup{AgentID: "agent", AgentType: "planner", Version: "v1"})
	if err != nil {
		t.Fatalf("get query: %v", err)
	}
	for _, fragment := range []string{"agent_id=?", "agent_type=?", "version=?", "ORDER BY activated_at_ms DESC, registered_at_ms DESC, agent_id DESC"} {
		if !strings.Contains(query, fragment) {
			t.Errorf("query missing %q: %s", fragment, query)
		}
	}
	if !reflect.DeepEqual(args, []any{"agent", "planner", "v1"}) {
		t.Fatalf("query args = %#v", args)
	}
}

func TestSQLStoreSupportsPostgresPlaceholdersAndSafeSchema(t *testing.T) {
	db, _ := openScriptDB(t)
	store, err := NewSQLStore(db, WithSQLDialect(SQLDialectPostgres), WithSQLTable("tenant_agent_registry"))
	if err != nil {
		t.Fatalf("new postgres store: %v", err)
	}
	query, _, err := store.getQuery(StoreLookup{AgentID: "agent", Version: "v1"})
	if err != nil {
		t.Fatalf("get query: %v", err)
	}
	if !strings.Contains(query, "agent_id=$1") || !strings.Contains(query, "version=$2") {
		t.Fatalf("postgres placeholders missing: %s", query)
	}
	if _, err := NewSQLStore(db, WithSQLTable("registry; DROP TABLE users")); err == nil {
		t.Fatal("unsafe table name must be rejected")
	}
	if _, err := NewSQLStore(db, WithSQLSnapshotTable("snapshots; DROP TABLE users")); err == nil {
		t.Fatal("unsafe snapshot table name must be rejected")
	}
	if _, err := NewSQLStore(db, WithSQLAuditTable("audit; DROP TABLE users")); err == nil {
		t.Fatal("unsafe audit table name must be rejected")
	}
	statements := sqlSchemaStatements("tenant_agent_registry")
	if len(statements) < 2 || !strings.Contains(statements[0], "revision BIGINT NOT NULL") {
		t.Fatalf("online schema incomplete: %#v", statements)
	}
}

func TestNewServiceWithSQLStoreInjectsExternalPool(t *testing.T) {
	if _, err := NewServiceWithSQLStore(nil); err == nil {
		t.Fatal("nil SQL pool must be rejected")
	}
	db, _ := openScriptDB(t)
	svc, err := NewServiceWithSQLStore(db, WithClock(func() time.Time {
		return time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)
	}))
	if err != nil {
		t.Fatalf("new SQL service: %v", err)
	}
	store, ok := svc.store.(*SQLStore)
	if !ok || store.db != db {
		t.Fatalf("external SQL pool was not injected: %#v", svc.store)
	}
	if got := svc.now(); got.Day() != 13 {
		t.Fatalf("service options were lost: %v", got)
	}
	if _, err := svc.RuntimeSystemPromptResolver(); err != nil {
		t.Fatalf("SQL service did not wire shared prompt resolver: %v", err)
	}
}

func TestNewSQLServiceWithToolCatalogCannotOmitRegistry(t *testing.T) {
	db, _ := openScriptDB(t)
	if _, err := NewServiceWithSQLStoreAndToolCatalog(db, nil); errorCode(err) != CodeDependencyMissing {
		t.Fatalf("missing Tool Registry error = %v", err)
	}
	registry := toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{catalogTool("search", "v1", toolgateway.RiskLow)})
	svc, err := NewServiceWithSQLStoreAndToolCatalog(db, registry)
	if err != nil {
		t.Fatalf("new SQL service with Tool Registry: %v", err)
	}
	composite, ok := svc.resolver.(CompositeDependencyResolver)
	if !ok || len(composite) != 2 {
		t.Fatalf("ToolCatalogResolver was not composed: %#v", svc.resolver)
	}
	if err := svc.ValidateProduction(); err != nil {
		t.Fatalf("complete SQL service did not pass production validation: %v", err)
	}
}

func TestProductionSQLServiceCannotBypassToolCatalog(t *testing.T) {
	db, _ := openScriptDB(t)
	legacy, err := NewServiceWithSQLStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.ValidateProduction(); !errors.Is(err, ErrProductionToolCatalog) {
		t.Fatalf("legacy SQL composition passed production validation: %v", err)
	}
	if _, err := NewProductionServiceWithSQLStore(db, nil); errorCode(err) != CodeDependencyMissing {
		t.Fatalf("production constructor accepted missing Tool Registry: %v", err)
	}

	registry := toolgateway.NewStaticRegistry([]toolgateway.ToolDefinition{
		catalogTool("search", "v1", toolgateway.RiskLow),
	})
	service, err := NewProductionServiceWithSQLStore(db, registry)
	if err != nil {
		t.Fatalf("new production SQL service: %v", err)
	}
	if err := service.ValidateProduction(); err != nil {
		t.Fatalf("production SQL service validation: %v", err)
	}
}

func TestLocalRegistryCompatibilityDoesNotImplicitlyEnableProduction(t *testing.T) {
	service := NewService(WithLegacyUnresolvedPrompts())
	if err := service.ValidateProduction(); !errors.Is(err, ErrProductionMemoryStore) {
		t.Fatalf("memory service unexpectedly passed production validation: %v", err)
	}
	if _, err := service.RegisterAgent(context.Background(), validConfig()); err != nil {
		t.Fatalf("local registry behavior changed: %v", err)
	}
}

func TestSQLStoreListsVersionsAndAppendsAudit(t *testing.T) {
	first := sqlTestRecord(t)
	secondService := NewService(WithClock(func() time.Time { return first.RegisteredAt.Add(time.Second) }))
	second, err := secondService.compile(extendedConfig(first.Config.AgentID, "v2"), ResolvedDependencies{}, nil)
	if err != nil {
		t.Fatalf("compile second version: %v", err)
	}
	db, state := openScriptDB(t,
		dbStep{
			kind: "query", contains: "WHERE agent_id=? ORDER BY version", columns: strings.Split(sqlRecordColumns, ", "),
			rows: [][]driver.Value{sqlTestRow(first), sqlTestRow(second)},
		},
		dbStep{kind: "exec", contains: "INSERT INTO agent_registry_audit_events", affected: 1},
	)
	store, _ := NewSQLStore(db)
	versions, err := store.ListVersions(context.Background(), first.Config.AgentID)
	if err != nil || len(versions) != 2 || versions[0].Version != "v1" || versions[1].Version != "v2" {
		t.Fatalf("list versions = %#v, %v", versions, err)
	}
	if err := store.AppendRegistryEvent(context.Background(), sqlTestEvent(EventEvalGatePassed, first)); err != nil {
		t.Fatalf("append audit: %v", err)
	}
	state.assertDone(t)
}

func TestSQLStoreEnsureSchemaExecutesCompleteLocalBootstrap(t *testing.T) {
	statements := sqlRegistrySchemaStatements("", "", "")
	steps := make([]dbStep, 0, len(statements))
	for _, statement := range statements {
		steps = append(steps, dbStep{kind: "exec", contains: statement, affected: 0})
	}
	db, state := openScriptDB(t, steps...)
	store, _ := NewSQLStore(db)
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	state.assertDone(t)
}

func sqlTestRecord(t *testing.T) *StoreRecord {
	t.Helper()
	svc := NewService(WithClock(func() time.Time { return time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC) }))
	record, err := svc.compile(extendedConfig("sql_agent", "v1"), ResolvedDependencies{}, nil)
	if err != nil {
		t.Fatalf("compile sql record: %v", err)
	}
	return record
}

func sqlTestEvent(eventType string, record *StoreRecord) RegistryEvent {
	return RegistryEvent{
		EventID: "evt_test", Type: eventType, AgentID: record.Config.AgentID, Version: record.Config.Version,
		ConfigHash: record.Effective.ConfigHash, Timestamp: time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC),
	}
}

func sqlTestRow(record *StoreRecord) []driver.Value {
	configJSON, _ := json.Marshal(record.Config)
	cardJSON, _ := json.Marshal(record.Card)
	effectiveJSON, _ := json.Marshal(record.Effective)
	return []driver.Value{
		record.Config.AgentID, record.Config.AgentType, record.Config.Version, string(record.Config.Status),
		record.Effective.ConfigHash, int64(record.GrayPercent), string(configJSON), string(cardJSON), string(effectiveJSON),
		record.RegisteredAt.UnixMilli(), record.ActivatedAtMS, record.Revision,
	}
}

func sqlSnapshotTestColumns() []string {
	return strings.Split(sqlSnapshotColumns+", config_json", ", ")
}

func sqlTestSnapshotRow(record *StoreRecord, snapshot ConfigSnapshotRecord) []driver.Value {
	effectiveJSON, _ := json.Marshal(snapshot.Effective)
	configJSON, _ := json.Marshal(record.Config)
	return []driver.Value{
		snapshot.Ref, snapshot.AgentID, snapshot.Version, string(snapshot.ExecutionMode), snapshot.ConfigHash,
		string(effectiveJSON), snapshot.CreatedAt.UnixMilli(), string(configJSON),
	}
}

type dbStep struct {
	kind        string
	contains    string
	notContains string
	columns     []string
	rows        [][]driver.Value
	affected    int64
	err         error
}

type scriptState struct {
	mu        sync.Mutex
	steps     []dbStep
	begins    int
	commits   int
	rollbacks int
}

func (s *scriptState) pop(kind, query string) (dbStep, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.steps) == 0 {
		return dbStep{}, fmt.Errorf("unexpected %s: %s", kind, query)
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	if step.kind != kind || !strings.Contains(query, step.contains) {
		return dbStep{}, fmt.Errorf("unexpected %s query %q, want %s containing %q", kind, query, step.kind, step.contains)
	}
	if step.notContains != "" && strings.Contains(query, step.notContains) {
		return dbStep{}, fmt.Errorf("unexpected %s query %q containing forbidden %q", kind, query, step.notContains)
	}
	return step, nil
}

func (s *scriptState) assertDone(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.steps) != 0 {
		t.Fatalf("unconsumed database steps: %#v", s.steps)
	}
}

var scriptDriverID atomic.Uint64

func openScriptDB(t *testing.T, steps ...dbStep) (*sql.DB, *scriptState) {
	t.Helper()
	state := &scriptState{steps: append([]dbStep(nil), steps...)}
	name := fmt.Sprintf("agentregistry-script-%d", scriptDriverID.Add(1))
	sql.Register(name, scriptDriver{state: state})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open script db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db, state
}

type scriptDriver struct{ state *scriptState }

func (d scriptDriver) Open(string) (driver.Conn, error) { return &scriptConn{state: d.state}, nil }

type scriptConn struct{ state *scriptState }

func (c *scriptConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (c *scriptConn) Close() error { return nil }
func (c *scriptConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *scriptConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.state.mu.Lock()
	c.state.begins++
	c.state.mu.Unlock()
	return &scriptTx{state: c.state}, nil
}

func (c *scriptConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	step, err := c.state.pop("exec", query)
	if err != nil {
		return nil, err
	}
	if step.err != nil {
		return nil, step.err
	}
	return driver.RowsAffected(step.affected), nil
}

func (c *scriptConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	step, err := c.state.pop("query", query)
	if err != nil {
		return nil, err
	}
	if step.err != nil {
		return nil, step.err
	}
	return &scriptRows{columns: step.columns, rows: step.rows}, nil
}

type scriptTx struct{ state *scriptState }

func (tx *scriptTx) Commit() error {
	tx.state.mu.Lock()
	tx.state.commits++
	tx.state.mu.Unlock()
	return nil
}

func (tx *scriptTx) Rollback() error {
	tx.state.mu.Lock()
	tx.state.rollbacks++
	tx.state.mu.Unlock()
	return nil
}

type scriptRows struct {
	columns []string
	rows    [][]driver.Value
	index   int
}

func (r *scriptRows) Columns() []string { return r.columns }
func (r *scriptRows) Close() error      { return nil }
func (r *scriptRows) Next(dest []driver.Value) error {
	if r.index >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.index])
	r.index++
	return nil
}
