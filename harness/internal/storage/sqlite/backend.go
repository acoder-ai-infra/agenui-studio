// Package sqlite 是 storage 的 SQLite 后端(单机/CI/本地 P0,portability §8)。
//
// 它用 modernc.org/sqlite(纯 Go,无 cgo),因此能在普通 `go test` 中真实运行,
// 不需要外部 DB、不需要 build tag —— 这是相对 mysql 雏形(需 live server)的关键优势。
// 依赖标准库 database/sql;驱动通过 blank import 注册。
//
// 提供完整 store 聚合，包括 EventStore、RunStore 与带 fencing 的 ResumeStore。
package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

//go:embed schema.sql
var Schema string

// Backend 是 SQLite 存储后端。
type Backend struct {
	db *sql.DB
}

// Open 打开一个 SQLite 数据库(dsn 例如 ":memory:" 或 "harness.db")。
// SQLite 写事务需串行化,这里将连接数固定为 1(SQLite 的推荐用法),
// 从而无需 SELECT ... FOR UPDATE 即可保证 EventStore 的 sequence 分配安全。
//
// 对于文件型 DSN(非 :memory:),设置 busy_timeout 防止偶发锁冲突。
// 不使用 WAL 模式:WAL 下数据先写 -wal 文件再 checkpoint 到主文件,
// 外部 SQLite 查看器(DB Browser / TablePlus / IntelliJ 等)若不加载 WAL
// 会看到空表。DELETE 模式(默认)数据直接写主文件,兼容所有查看器。
func Open(dsn string) (*Backend, error) {
	db, err := OpenDatabase(dsn)
	if err != nil {
		return nil, err
	}
	return New(db), nil
}

// OpenDatabase opens a caller-owned SQLite pool using the same connection
// policy as Backend. Composition code uses it when several schema owners must
// migrate the same database through one connection.
func OpenDatabase(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if dsn != ":memory:" {
		if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
			db.Close()
			return nil, fmt.Errorf("sqlite: set busy_timeout: %w", err)
		}
	}
	return db, nil
}

// New 包装一个已打开的 *sql.DB(驱动由调用方注册)。
func New(db *sql.DB) *Backend {
	db.SetMaxOpenConns(1)
	return &Backend{db: db}
}

// Close 关闭底层连接池。
func (b *Backend) Close() error { return b.db.Close() }

func (b *Backend) Name() string { return "sqlite" }

// Capabilities 声明 SQLite 的功能能力(单机;非高并发生产)。
func (b *Backend) Capabilities() map[storage.Capability]bool {
	return map[storage.Capability]bool{
		storage.CapCAS:             true,
		storage.CapTransaction:     true,
		storage.CapOrderedAppend:   true,
		storage.CapIdempotency:     true,
		storage.CapTTL:             true,
		storage.CapPrefixScan:      true,
		storage.CapPagination:      true,
		storage.CapSecondaryIndex:  true,
		storage.CapTenantIsolation: true,
	}
}

func (b *Backend) Health(ctx context.Context) error { return b.db.PingContext(ctx) }

// CheckReady verifies that the required schema is already present. It performs
// metadata reads only and never creates or alters database objects.
func (b *Backend) CheckReady(ctx context.Context) error {
	for table, columns := range requiredColumns() {
		have, err := b.columns(ctx, table)
		if err != nil {
			return err
		}
		if len(have) == 0 {
			return fmt.Errorf("sqlite schema not ready: table %s is missing; run storage migration before starting Runtime", table)
		}
		for _, column := range columns {
			if !have[column] {
				return fmt.Errorf("sqlite schema not ready: column %s.%s is missing; run storage migration before starting Runtime", table, column)
			}
		}
	}
	return nil
}

// Migrate 执行 schema.sql:先剔除整行 "--" 注释,再按 ";" 拆分逐条执行
// (避免以注释起头的语句块被整体跳过)。
func (b *Backend) Migrate(ctx context.Context) error {
	var clean strings.Builder
	for _, line := range strings.Split(Schema, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		clean.WriteString(line)
		clean.WriteByte('\n')
	}
	for _, stmt := range strings.Split(clean.String(), ";") {
		s := strings.TrimSpace(stmt)
		if s == "" {
			continue
		}
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "runtime_binding", definition: "TEXT"},
		{name: "resume_attempt_id", definition: "TEXT"},
	} {
		if err := b.ensureColumn(ctx, "runs", column.name, column.definition); err != nil {
			return err
		}
	}
	// 旧库可能仍有 resume_lease_until；新代码不再读写，避免为清理无害列执行破坏性 DDL。
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "parent_span_id", definition: "TEXT"},
		{name: "agent_type", definition: "TEXT"},
		{name: "runtime", definition: "TEXT"},
	} {
		if err := b.ensureColumn(ctx, "agent_events", column.name, column.definition); err != nil {
			return err
		}
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "total_latency_ms", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "first_token_observed", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "first_token_ms", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "generation_duration_ms", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "output_tokens_per_second", definition: "REAL NOT NULL DEFAULT 0"},
	} {
		if err := b.ensureColumn(ctx, "model_usage_records", column.name, column.definition); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := b.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = b.db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+definition)
	return err
}

func (b *Backend) columns(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := b.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return columns, nil
}

func requiredColumns() map[string][]string {
	return map[string][]string{
		"runs":                  {"run_id", "session_id", "turn_id", "parent_run_id", "tenant_id", "agent_id", "runtime", "runtime_binding", "status", "trace_id", "config_snapshot_ref", "context_snapshot_ref", "agent_binding_id", "resume_attempt_id", "started_at", "ended_at", "error_code", "error_message", "version"},
		"agent_events":          {"event_id", "run_id", "sequence", "tenant_id", "session_id", "step_id", "agent_id", "trace_id", "span_id", "parent_span_id", "agent_type", "runtime", "event_type", "visibility", "schema_version", "idempotency_key", "payload", "payload_preview", "payload_ref", "usage", "debug_ref", "error", "created_at"},
		"sessions":              {"session_id", "tenant_id", "user_id", "channel", "agent_id", "title", "status", "created_at", "updated_at", "version", "metadata"},
		"messages":              {"message_id", "session_id", "turn_id", "run_id", "tenant_id", "role", "visibility", "content_ref", "content_preview", "created_at"},
		"model_usage_records":   {"usage_id", "request_id", "trace_id", "tenant_id", "session_id", "run_id", "agent_id", "provider", "model", "attempt", "fallback_applied", "prompt_tokens", "completion_tokens", "reasoning_tokens", "cache_read_tokens", "cache_write_tokens", "usage_source", "currency", "estimated_cost", "total_latency_ms", "first_token_observed", "first_token_ms", "generation_duration_ms", "output_tokens_per_second", "cache_hit", "output_ref", "created_at"},
		"steps":                 {"step_id", "run_id", "parent_step_id", "tenant_id", "step_type", "name", "status", "started_at", "ended_at"},
		"checkpoint_meta":       {"checkpoint_id", "run_id", "tenant_id", "runtime", "type", "state_ref", "event_sequence", "created_reason", "created_at", "expires_at"},
		"control_requests":      {"request_id", "run_id", "tenant_id", "checkpoint_id", "type", "status", "resume_token_hash", "tool_use_id", "prompt_preview", "response_ref", "schema_version", "created_at", "expires_at", "version"},
		"idempotency_keys":      {"tenant_id", "namespace", "idem_key", "expires_at"},
		"open_turn_idempotency": {"tenant_id", "user_id", "session_scope", "idem_key", "request_hash", "session_id", "run_id", "message_id", "created_at"},
	}
}

// EventStore 返回 SQLite EventStore。
func (b *Backend) EventStore() storage.EventStore { return &eventStore{db: b.db} }

// RunStore 返回 SQLite RunStore。
func (b *Backend) RunStore() storage.RunStore { return &runStore{db: b.db} }

// SessionStore 返回 SQLite SessionStore。
func (b *Backend) SessionStore() storage.SessionStore { return &sessionStore{db: b.db} }

// MessageStore 返回 SQLite MessageStore。
func (b *Backend) MessageStore() storage.MessageStore { return &messageStore{db: b.db} }

// ModelUsageStore 返回 SQLite ModelUsageStore。
func (b *Backend) ModelUsageStore() storage.ModelUsageStore { return &usageStore{db: b.db} }

// StepStore 返回 SQLite StepStore。
func (b *Backend) StepStore() storage.StepStore { return &stepStore{db: b.db} }

// CheckpointStore 返回 SQLite CheckpointStore。
func (b *Backend) CheckpointStore() storage.CheckpointStore { return &checkpointStore{db: b.db} }

// ControlRequestStore 返回 SQLite ControlRequestStore。
func (b *Backend) ControlRequestStore() storage.ControlRequestStore { return &controlStore{db: b.db} }

func (b *Backend) ResumeStore() storage.ResumeStore {
	return &resumeStore{db: b.db, events: &eventStore{db: b.db}}
}

// IdempotencyStore 返回 SQLite IdempotencyStore。
func (b *Backend) IdempotencyStore() storage.IdempotencyStore { return &idempotencyStore{db: b.db} }
func (b *Backend) OpenTurnStore() storage.OpenTurnStore {
	return &openTurnStore{db: b.db, ids: observability.NewULIDGenerator("")}
}

// Stores 返回由 SQLite 后端支撑的全部 8 个 store 端口。
func (b *Backend) Stores() storage.Stores {
	return storage.Stores{
		Turns:       b.OpenTurnStore(),
		Sessions:    b.SessionStore(),
		Messages:    b.MessageStore(),
		Usage:       b.ModelUsageStore(),
		Runs:        b.RunStore(),
		Steps:       b.StepStore(),
		Events:      b.EventStore(),
		Checkpoints: b.CheckpointStore(),
		Controls:    b.ControlRequestStore(),
		Resumes:     b.ResumeStore(),
		Idem:        b.IdempotencyStore(),
	}
}

var _ storage.Backend = (*Backend)(nil)

// isUniqueViolation 判断 err 是否为 SQLite 的唯一/主键约束冲突
// (modernc 驱动的报文形如 "constraint failed: UNIQUE constraint failed: ...")。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "constraint failed")
}

// --- 时间与 NULL 辅助 ---

func tsVal(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func tsTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
