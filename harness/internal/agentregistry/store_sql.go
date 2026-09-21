package agentregistry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultSQLTable         = "agent_registry_entries"
	defaultSQLSnapshotTable = "agent_registry_config_snapshots"
	defaultSQLAuditTable    = "agent_registry_audit_events"
)

type SQLDialect string

const (
	SQLDialectQuestionMark SQLDialect = "question_mark"
	SQLDialectPostgres     SQLDialect = "postgres"
)

type SQLStoreOption func(*SQLStore) error

// SQLStore 使用共享关系型数据库作为事实源；调用方只需注入已配置连接池的 *sql.DB。
// 包内不导入任何具体 driver，部署方可按环境选择 MySQL、PostgreSQL 或兼容实现。
type SQLStore struct {
	db                 *sql.DB
	table              string
	snapshotTable      string
	auditTable         string
	dialect            SQLDialect
	conflictClassifier func(error) bool
}

func NewSQLStore(db *sql.DB, opts ...SQLStoreOption) (*SQLStore, error) {
	if db == nil {
		return nil, errors.New("agentregistry: sql db is required")
	}
	store := &SQLStore{
		db:                 db,
		table:              defaultSQLTable,
		snapshotTable:      defaultSQLSnapshotTable,
		auditTable:         defaultSQLAuditTable,
		dialect:            SQLDialectQuestionMark,
		conflictClassifier: defaultSQLConflictClassifier,
	}
	for _, opt := range opts {
		if err := opt(store); err != nil {
			return nil, err
		}
	}
	for _, table := range []string{store.table, store.snapshotTable, store.auditTable} {
		if !validSQLIdentifier(table) {
			return nil, fmt.Errorf("agentregistry: invalid sql table %q", table)
		}
	}
	return store, nil
}

func WithSQLDialect(dialect SQLDialect) SQLStoreOption {
	return func(store *SQLStore) error {
		switch dialect {
		case SQLDialectQuestionMark, SQLDialectPostgres:
			store.dialect = dialect
			return nil
		default:
			return fmt.Errorf("agentregistry: unsupported sql dialect %q", dialect)
		}
	}
}

func WithSQLTable(table string) SQLStoreOption {
	return func(store *SQLStore) error {
		if !validSQLIdentifier(table) {
			return fmt.Errorf("agentregistry: invalid sql table %q", table)
		}
		store.table = table
		return nil
	}
}

func WithSQLSnapshotTable(table string) SQLStoreOption {
	return func(store *SQLStore) error {
		if !validSQLIdentifier(table) {
			return fmt.Errorf("agentregistry: invalid sql snapshot table %q", table)
		}
		store.snapshotTable = table
		return nil
	}
}

func WithSQLAuditTable(table string) SQLStoreOption {
	return func(store *SQLStore) error {
		if !validSQLIdentifier(table) {
			return fmt.Errorf("agentregistry: invalid sql audit table %q", table)
		}
		store.auditTable = table
		return nil
	}
}

func WithSQLConflictClassifier(classifier func(error) bool) SQLStoreOption {
	return func(store *SQLStore) error {
		if classifier == nil {
			return errors.New("agentregistry: sql conflict classifier is required")
		}
		store.conflictClassifier = classifier
		return nil
	}
}

func (s *SQLStore) EnsureSchema(ctx context.Context) error {
	for _, statement := range sqlRegistrySchemaStatementsWithDigestType(s.table, s.snapshotTable, s.auditTable, sqlIdentityDigestType(s.dialect)) {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("agentregistry: ensure sql schema: %w", err)
		}
	}
	return nil
}

func sqlSchemaStatements(table string) []string {
	return sqlSchemaStatementsWithDigestType(table, "BLOB")
}

func sqlSchemaStatementsWithDigestType(table, digestType string) []string {
	if table == "" {
		table = defaultSQLTable
	}
	return []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  agent_id VARCHAR(255) NOT NULL,
  agent_type VARCHAR(255) NOT NULL,
  version VARCHAR(255) NOT NULL,
  status VARCHAR(32) NOT NULL,
  config_hash VARCHAR(80) NOT NULL,
  gray_percent INTEGER NOT NULL DEFAULT 0,
  config_json TEXT NOT NULL,
  card_json TEXT NOT NULL,
  effective_json TEXT NOT NULL,
  registered_at_ms BIGINT NOT NULL,
  activated_at_ms BIGINT NOT NULL,
  revision BIGINT NOT NULL,
	  agent_version_digest %s NOT NULL,
	  type_version_digest %s NOT NULL,
  PRIMARY KEY (agent_id, version),
	  UNIQUE (agent_type, version),
	  UNIQUE (agent_version_digest),
	  UNIQUE (type_version_digest)
)`, table, digestType, digestType),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s_agent_lookup_idx ON %s (agent_id, status, activated_at_ms DESC, registered_at_ms DESC, version DESC)", table, table),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s_type_lookup_idx ON %s (agent_type, status, activated_at_ms DESC, registered_at_ms DESC, version DESC)", table, table),
	}
}

// sqlRegistrySchemaStatements 仅用于本地 bootstrap；线上应使用对应数据库的编号 migration。
// 表名只来自已校验的 Store 配置，不对外暴露动态 DDL 拼接入口。
func sqlRegistrySchemaStatements(table, snapshotTable, auditTable string) []string {
	return sqlRegistrySchemaStatementsWithDigestType(table, snapshotTable, auditTable, "BLOB")
}

func sqlRegistrySchemaStatementsWithDigestType(table, snapshotTable, auditTable, digestType string) []string {
	if table == "" {
		table = defaultSQLTable
	}
	if snapshotTable == "" {
		snapshotTable = defaultSQLSnapshotTable
	}
	if auditTable == "" {
		auditTable = defaultSQLAuditTable
	}
	statements := sqlSchemaStatementsWithDigestType(table, digestType)
	return append(statements,
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  config_snapshot_ref VARCHAR(768) NOT NULL,
  agent_id VARCHAR(255) NOT NULL,
  version VARCHAR(255) NOT NULL,
  execution_mode VARCHAR(32) NOT NULL,
  config_hash VARCHAR(80) NOT NULL,
  effective_json TEXT NOT NULL,
  created_at_ms BIGINT NOT NULL,
	  snapshot_ref_digest %s NOT NULL,
	  agent_mode_digest %s NOT NULL,
	  agent_hash_digest %s NOT NULL,
  PRIMARY KEY (config_snapshot_ref),
  UNIQUE (agent_id, version, execution_mode),
	  UNIQUE (agent_id, version, config_hash),
	  UNIQUE (snapshot_ref_digest),
	  UNIQUE (agent_mode_digest),
	  UNIQUE (agent_hash_digest)
)`, snapshotTable, digestType, digestType, digestType),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s_agent_mode_idx ON %s (agent_id, version, execution_mode, created_at_ms DESC)", snapshotTable, snapshotTable),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  event_id VARCHAR(255) NOT NULL,
  event_type VARCHAR(80) NOT NULL,
  tenant_id VARCHAR(255) NOT NULL DEFAULT '',
  agent_id VARCHAR(255) NOT NULL DEFAULT '',
  agent_version VARCHAR(255) NOT NULL DEFAULT '',
  trace_id VARCHAR(255) NOT NULL DEFAULT '',
  span_id VARCHAR(255) NOT NULL DEFAULT '',
  parent_span_id VARCHAR(255) NOT NULL DEFAULT '',
  request_id VARCHAR(255) NOT NULL DEFAULT '',
  run_id VARCHAR(255) NOT NULL DEFAULT '',
  selection_hash VARCHAR(255) NOT NULL DEFAULT '',
  binding_hash VARCHAR(255) NOT NULL DEFAULT '',
  config_hash VARCHAR(80) NOT NULL DEFAULT '',
  actor VARCHAR(255) NOT NULL DEFAULT '',
  reason VARCHAR(255) NOT NULL DEFAULT '',
  diff_summary TEXT NOT NULL,
  occurred_at_ms BIGINT NOT NULL,
	  event_id_digest %s NOT NULL,
	  PRIMARY KEY (event_id),
	  UNIQUE (event_id_digest)
)`, auditTable, digestType),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s_agent_time_idx ON %s (agent_id, agent_version, occurred_at_ms DESC, event_id DESC)", auditTable, auditTable),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s_tenant_time_idx ON %s (tenant_id, occurred_at_ms DESC, event_id DESC)", auditTable, auditTable),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s_trace_time_idx ON %s (trace_id, occurred_at_ms, event_id)", auditTable, auditTable),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s_run_time_idx ON %s (run_id, occurred_at_ms, event_id)", auditTable, auditTable),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s_type_time_idx ON %s (event_type, occurred_at_ms DESC, event_id DESC)", auditTable, auditTable),
		sqlPromptSchemaStatementWithDigestType(defaultSQLPromptTable, digestType),
	)
}

func sqlPromptSchemaStatement(table string) string {
	return sqlPromptSchemaStatementWithDigestType(table, "BLOB")
}

func sqlPromptSchemaStatementWithDigestType(table, digestType string) string {
	if table == "" {
		table = defaultSQLPromptTable
	}
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  prompt_ref VARCHAR(512) NOT NULL,
  version VARCHAR(128) NOT NULL,
  content TEXT,
  content_ref VARCHAR(1024),
  content_hash VARCHAR(80) NOT NULL,
  variables_json TEXT NOT NULL,
  created_at_ms BIGINT NOT NULL,
  created_by VARCHAR(255) NOT NULL DEFAULT '',
	  prompt_identity_digest %s NOT NULL,
  PRIMARY KEY (prompt_ref, version),
	  UNIQUE (prompt_identity_digest),
  CHECK ((content IS NOT NULL AND length(content) > 0 AND content_ref IS NULL)
	  OR (content IS NULL AND content_ref IS NOT NULL AND length(content_ref) > 0)),
  CHECK (created_at_ms >= 0)
)`, table, digestType)
}

func sqlIdentityDigestType(dialect SQLDialect) string {
	if dialect == SQLDialectPostgres {
		return "BYTEA"
	}
	return "BLOB"
}

func (s *SQLStore) Create(ctx context.Context, record *StoreRecord, audit RegistryEvent) error {
	if record == nil {
		return ErrAgentNotFound
	}
	if err := validateConfigSnapshots(record); err != nil {
		return err
	}
	if err := validateRegistryEvent(audit); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := s.insert(ctx, tx, record); err != nil {
		if s.conflictClassifier(err) {
			return fmt.Errorf("%w: %v", ErrDuplicateAgent, err)
		}
		return err
	}
	for i := range record.ConfigSnapshots {
		if err := s.insertConfigSnapshot(ctx, tx, &record.ConfigSnapshots[i]); err != nil {
			if s.conflictClassifier(err) {
				return fmt.Errorf("%w: config snapshot already exists", ErrStoreConflict)
			}
			return err
		}
	}
	if err := s.insertRegistryEvent(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLStore) Get(ctx context.Context, lookup StoreLookup) (*StoreRecord, error) {
	query, args, err := s.getQuery(lookup)
	if err != nil {
		return nil, err
	}
	record, err := scanSQLRecord(s.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAgentNotFound
	}
	return record, err
}

const sqlSnapshotColumns = "config_snapshot_ref, agent_id, version, execution_mode, config_hash, effective_json, created_at_ms"
const sqlSnapshotInsertColumns = sqlSnapshotColumns + ", snapshot_ref_digest, agent_mode_digest, agent_hash_digest"
const sqlSnapshotReadColumns = "s.config_snapshot_ref, s.agent_id, s.version, s.execution_mode, s.config_hash, s.effective_json, s.created_at_ms, e.config_json"

func (s *SQLStore) GetConfigSnapshot(ctx context.Context, ref string) (*ConfigSnapshotRecord, error) {
	query := "SELECT " + sqlSnapshotReadColumns + " FROM " + s.snapshotTable + " AS s JOIN " + s.table +
		" AS e ON e.agent_id=s.agent_id AND e.version=s.version WHERE s.config_snapshot_ref=" + s.placeholder(1)
	snapshot, err := scanSQLConfigSnapshot(s.db.QueryRowContext(ctx, query, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConfigSnapshotMissing
	}
	return snapshot, err
}

func (s *SQLStore) GetConfigSnapshotByMode(ctx context.Context, ref AgentRef, mode ExecutionMode) (*ConfigSnapshotRecord, error) {
	query := "SELECT " + sqlSnapshotReadColumns + " FROM " + s.snapshotTable + " AS s JOIN " + s.table +
		" AS e ON e.agent_id=s.agent_id AND e.version=s.version WHERE s.agent_id=" + s.placeholder(1) +
		" AND s.version=" + s.placeholder(2) + " AND s.execution_mode=" + s.placeholder(3) + " LIMIT 1"
	snapshot, err := scanSQLConfigSnapshot(s.db.QueryRowContext(ctx, query, ref.AgentID, ref.Version, string(mode)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConfigSnapshotMissing
	}
	return snapshot, err
}

func (s *SQLStore) List(ctx context.Context) ([]*StoreRecord, error) {
	query := "SELECT " + sqlRecordColumns + " FROM " + s.table + " ORDER BY agent_id, version"
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []*StoreRecord
	for rows.Next() {
		record, err := scanSQLRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *SQLStore) UpdateStatus(ctx context.Context, ref AgentRef, status AgentStatus, activatedAtMS int64, audit RegistryEvent) error {
	if !validAgentStatus(status) {
		return fmt.Errorf("%w: invalid agent status %q", ErrStoreCorrupt, status)
	}
	if err := validateRegistryEvent(audit); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	revision, config, card, err := s.readMutable(ctx, tx, ref)
	if err != nil {
		return err
	}
	config.Status = status
	card.Status = status
	card.CardHash, err = computeCardHash(card)
	if err != nil {
		return err
	}
	configJSON, cardJSON, err := marshalMutable(config, card)
	if err != nil {
		return err
	}
	// revision 同时出现在读结果和 UPDATE 条件中，多实例竞争时只有一个提交者能成功。
	query := "UPDATE " + s.table + " SET status=" + s.placeholder(1) + ", config_json=" + s.placeholder(2) +
		", card_json=" + s.placeholder(3)
	args := []any{string(status), configJSON, cardJSON}
	if status == AgentStatusEnabled {
		query += ", activated_at_ms=" + s.placeholder(4)
		args = append(args, activatedAtMS)
	}
	next := len(args) + 1
	query += ", revision=revision+1 WHERE agent_id=" + s.placeholder(next) +
		" AND version=" + s.placeholder(next+1) + " AND revision=" + s.placeholder(next+2)
	args = append(args, ref.AgentID, ref.Version, revision)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if err := requireSingleUpdate(result); err != nil {
		return err
	}
	if err := s.insertRegistryEvent(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLStore) UpdateGrayPercent(ctx context.Context, ref AgentRef, percent int, audit RegistryEvent) error {
	if percent < 0 || percent > 100 {
		return fmt.Errorf("%w: gray percent must be between 0 and 100", ErrStoreCorrupt)
	}
	if err := validateRegistryEvent(audit); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	revision, config, _, err := s.readMutable(ctx, tx, ref)
	if err != nil {
		return err
	}
	config.Release.GrayPercent = percent
	configJSON, err := json.Marshal(config)
	if err != nil {
		return err
	}
	query := "UPDATE " + s.table + " SET gray_percent=" + s.placeholder(1) + ", config_json=" + s.placeholder(2) +
		", revision=revision+1 WHERE agent_id=" + s.placeholder(3) + " AND version=" + s.placeholder(4) +
		" AND revision=" + s.placeholder(5)
	result, err := tx.ExecContext(ctx, query, percent, configJSON, ref.AgentID, ref.Version, revision)
	if err != nil {
		return err
	}
	if err := requireSingleUpdate(result); err != nil {
		return err
	}
	if err := s.insertRegistryEvent(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLStore) AppendRegistryEvent(ctx context.Context, event RegistryEvent) error {
	return s.insertRegistryEvent(ctx, s.db, event)
}

func (s *SQLStore) ListVersions(ctx context.Context, agentID string) ([]AgentVersion, error) {
	query := "SELECT " + sqlRecordColumns + " FROM " + s.table +
		" WHERE agent_id=" + s.placeholder(1) + " ORDER BY version"
	rows, err := s.db.QueryContext(ctx, query, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var versions []AgentVersion
	for rows.Next() {
		record, err := scanSQLRecord(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, AgentVersion{
			AgentID: record.Config.AgentID, AgentType: record.Config.AgentType, Version: record.Config.Version,
			Status: record.Config.Status, ConfigHash: record.Effective.ConfigHash, RegisteredAt: record.RegisteredAt,
			GrayPercent: record.GrayPercent, Revision: record.Revision,
		})
	}
	return versions, rows.Err()
}

const sqlRecordColumns = "agent_id, agent_type, version, status, config_hash, gray_percent, config_json, card_json, effective_json, registered_at_ms, activated_at_ms, revision"
const sqlRecordInsertColumns = sqlRecordColumns + ", agent_version_digest, type_version_digest"

type sqlRecordScanner interface{ Scan(...any) error }

func scanSQLRecord(scanner sqlRecordScanner) (*StoreRecord, error) {
	var (
		record                                          StoreRecord
		agentID, agentType, version, status, configHash string
		configJSON, cardJSON, effJSON                   string
		grayPercent                                     int
		registeredAtMS, activatedAtMS, revision         int64
	)
	if err := scanner.Scan(
		&agentID, &agentType, &version, &status, &configHash, &grayPercent,
		&configJSON, &cardJSON, &effJSON, &registeredAtMS, &activatedAtMS, &revision,
	); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(configJSON), &record.Config); err != nil {
		return nil, fmt.Errorf("%w: decode stored config: %v", ErrStoreCorrupt, err)
	}
	if err := json.Unmarshal([]byte(cardJSON), &record.Card); err != nil {
		return nil, fmt.Errorf("%w: decode stored capability card: %v", ErrStoreCorrupt, err)
	}
	if err := json.Unmarshal([]byte(effJSON), &record.Effective); err != nil {
		return nil, fmt.Errorf("%w: decode stored effective config: %v", ErrStoreCorrupt, err)
	}
	if !validAgentStatus(AgentStatus(status)) ||
		record.Config.AgentID != agentID || record.Config.AgentType != agentType || record.Config.Version != version ||
		string(record.Config.Status) != status || record.Config.Release.GrayPercent != grayPercent ||
		record.Card.AgentID != agentID || record.Card.AgentType != agentType || record.Card.Version != version ||
		string(record.Card.Status) != status || record.Card.ConfigHash != configHash ||
		record.Effective.Definition.AgentID != agentID || record.Effective.Definition.AgentType != agentType ||
		record.Effective.Definition.Version != version || record.Effective.ConfigHash != configHash ||
		record.Effective.ConfigSnapshotRef != configSnapshotRef(agentID, version, configHash) ||
		grayPercent < 0 || grayPercent > 100 || registeredAtMS < 0 || activatedAtMS < 0 || revision < 1 {
		return nil, fmt.Errorf("%w: registry entry projection mismatch", ErrStoreCorrupt)
	}
	effectiveHash, err := computeEffectiveConfigHash(record.Effective, record.Config)
	if err != nil || effectiveHash != configHash {
		return nil, fmt.Errorf("%w: registry effective config hash mismatch", ErrStoreCorrupt)
	}
	cardHash, err := computeCardHash(record.Card)
	if err != nil || cardHash != record.Card.CardHash {
		return nil, fmt.Errorf("%w: registry capability card hash mismatch", ErrStoreCorrupt)
	}
	record.GrayPercent = grayPercent
	record.RegisteredAt = time.UnixMilli(registeredAtMS).UTC()
	record.ActivatedAtMS = activatedAtMS
	record.Revision = revision
	return &record, nil
}

func (s *SQLStore) insert(ctx context.Context, tx *sql.Tx, input *StoreRecord) error {
	record := cloneCompiledAgent(input)
	configJSON, err := json.Marshal(record.Config)
	if err != nil {
		return err
	}
	cardJSON, err := json.Marshal(record.Card)
	if err != nil {
		return err
	}
	effectiveJSON, err := json.Marshal(record.Effective)
	if err != nil {
		return err
	}
	if record.Revision == 0 {
		record.Revision = 1
	}
	query := "INSERT INTO " + s.table + " (" + sqlRecordInsertColumns + ") VALUES (" + s.placeholders(14) + ")"
	_, err = tx.ExecContext(ctx, query,
		record.Config.AgentID, record.Config.AgentType, record.Config.Version, string(record.Config.Status),
		record.Effective.ConfigHash, record.GrayPercent, configJSON, cardJSON, effectiveJSON,
		record.RegisteredAt.UnixMilli(), record.ActivatedAtMS, record.Revision,
		identityTupleDigest(record.Config.AgentID, record.Config.Version),
		identityTupleDigest(record.Config.AgentType, record.Config.Version),
	)
	return err
}

type sqlContextExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *SQLStore) insertConfigSnapshot(ctx context.Context, executor sqlContextExecer, snapshot *ConfigSnapshotRecord) error {
	if err := validateConfigSnapshotRecord(snapshot); err != nil {
		return err
	}
	effectiveJSON, err := json.Marshal(snapshot.Effective)
	if err != nil {
		return err
	}
	query := "INSERT INTO " + s.snapshotTable + " (" + sqlSnapshotInsertColumns + ") VALUES (" + s.placeholders(10) + ")"
	_, err = executor.ExecContext(ctx, query,
		snapshot.Ref, snapshot.AgentID, snapshot.Version, string(snapshot.ExecutionMode), snapshot.ConfigHash,
		effectiveJSON, snapshot.CreatedAt.UnixMilli(),
		identityTupleDigest(snapshot.Ref),
		identityTupleDigest(snapshot.AgentID, snapshot.Version, string(snapshot.ExecutionMode)),
		identityTupleDigest(snapshot.AgentID, snapshot.Version, snapshot.ConfigHash),
	)
	return err
}

func (s *SQLStore) insertRegistryEvent(ctx context.Context, executor sqlContextExecer, event RegistryEvent) error {
	if err := validateRegistryEvent(event); err != nil {
		return err
	}
	const columns = "event_id, event_type, tenant_id, agent_id, agent_version, trace_id, span_id, parent_span_id, request_id, run_id, selection_hash, binding_hash, config_hash, actor, reason, diff_summary, occurred_at_ms, event_id_digest"
	query := "INSERT INTO " + s.auditTable + " (" + columns + ") VALUES (" + s.placeholders(18) + ")"
	_, err := executor.ExecContext(ctx, query,
		event.EventID, event.Type, event.Tenant, event.AgentID, event.Version,
		event.TraceID, event.SpanID, event.ParentSpanID, event.RequestID, event.RunID,
		event.SelectionHash, event.BindingHash, event.ConfigHash, event.Actor, event.Reason,
		event.DiffSummary, event.Timestamp.UnixMilli(), identityTupleDigest(event.EventID),
	)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAuditWrite, err)
	}
	return nil
}

func scanSQLConfigSnapshot(scanner sqlRecordScanner) (*ConfigSnapshotRecord, error) {
	var snapshot ConfigSnapshotRecord
	var mode, effectiveJSON, configJSON string
	var createdAtMS int64
	if err := scanner.Scan(
		&snapshot.Ref, &snapshot.AgentID, &snapshot.Version, &mode, &snapshot.ConfigHash,
		&effectiveJSON, &createdAtMS, &configJSON,
	); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(effectiveJSON), &snapshot.Effective); err != nil {
		return nil, fmt.Errorf("%w: decode config snapshot: %v", ErrStoreCorrupt, err)
	}
	snapshot.ExecutionMode = ExecutionMode(mode)
	snapshot.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	if err := validateConfigSnapshotRecord(&snapshot); err != nil {
		return nil, err
	}
	var cfg AgentConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return nil, fmt.Errorf("%w: decode snapshot source config: %v", ErrStoreCorrupt, err)
	}
	if err := validateConfigSnapshotForAgent(&snapshot, cfg); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (s *SQLStore) readMutable(ctx context.Context, tx *sql.Tx, ref AgentRef) (int64, AgentConfig, CapabilityCard, error) {
	query := "SELECT " + sqlRecordColumns + " FROM " + s.table + " WHERE agent_id=" + s.placeholder(1) +
		" AND version=" + s.placeholder(2)
	record, err := scanSQLRecord(tx.QueryRowContext(ctx, query, ref.AgentID, ref.Version))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, AgentConfig{}, CapabilityCard{}, ErrAgentNotFound
		}
		return 0, AgentConfig{}, CapabilityCard{}, err
	}
	return record.Revision, record.Config, record.Card, nil
}

func marshalMutable(config AgentConfig, card CapabilityCard) ([]byte, []byte, error) {
	configJSON, err := json.Marshal(config)
	if err != nil {
		return nil, nil, err
	}
	cardJSON, err := json.Marshal(card)
	if err != nil {
		return nil, nil, err
	}
	return configJSON, cardJSON, nil
}

func (s *SQLStore) getQuery(lookup StoreLookup) (string, []any, error) {
	base := "SELECT " + sqlRecordColumns + " FROM " + s.table + " WHERE "
	if lookup.AgentID == "" && lookup.AgentType == "" {
		return "", nil, ErrAgentNotFound
	}
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 3)
	if lookup.AgentID != "" {
		args = append(args, lookup.AgentID)
		conditions = append(conditions, "agent_id="+s.placeholder(len(args)))
	}
	if lookup.AgentType != "" {
		args = append(args, lookup.AgentType)
		conditions = append(conditions, "agent_type="+s.placeholder(len(args)))
	}
	if lookup.Version != "" {
		args = append(args, lookup.Version)
		conditions = append(conditions, "version="+s.placeholder(len(args)))
	} else {
		args = append(args, string(AgentStatusEnabled))
		conditions = append(conditions, "status="+s.placeholder(len(args)))
	}
	query := base + strings.Join(conditions, " AND ") + " ORDER BY activated_at_ms DESC, registered_at_ms DESC"
	if lookup.Version == "" {
		query += ", version DESC"
	}
	return query + ", agent_id DESC LIMIT 1", args, nil
}

func (s *SQLStore) placeholder(index int) string {
	if s.dialect == SQLDialectPostgres {
		return "$" + strconv.Itoa(index)
	}
	return "?"
}

func (s *SQLStore) placeholders(count int) string {
	values := make([]string, count)
	for i := range values {
		values[i] = s.placeholder(i + 1)
	}
	return strings.Join(values, ",")
}

func requireSingleUpdate(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrStoreConflict
	}
	return nil
}

func rollback(tx *sql.Tx) { _ = tx.Rollback() }

func defaultSQLConflictClassifier(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate") || strings.Contains(message, "unique constraint") || strings.Contains(message, "unique violation")
}

var sqlIdentifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validSQLIdentifier(value string) bool { return sqlIdentifierPattern.MatchString(value) }

var _ Store = (*SQLStore)(nil)
