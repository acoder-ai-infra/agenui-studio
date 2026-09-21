-- Session/Run 存储账本 —— SQLite schema(单机/CI/本地 P0 后端)。
--
-- 与 mysql 后端同构:tenant_id 列 + 复合索引;EventStore 用唯一约束守核心不变量:
--   UNIQUE(run_id, sequence)          -> run 内单调、gap-tolerant 的 sequence
--   event_id PRIMARY KEY              -> event-id 幂等
--   UNIQUE(tenant_id, idempotency_key)-> idempotency-key 幂等(NULL 视为不同,故无 key 不去重)
--
-- 时间统一存 INTEGER(UnixNano;0 表示零值),规避驱动的时间格式差异。

CREATE TABLE IF NOT EXISTS runs (
    run_id               TEXT    NOT NULL PRIMARY KEY,
    session_id           TEXT    NOT NULL,
    turn_id              TEXT,
    parent_run_id        TEXT,
    tenant_id            TEXT    NOT NULL,
    agent_id             TEXT,
    runtime              TEXT,
    runtime_binding      TEXT,
    status               TEXT    NOT NULL,
    trace_id             TEXT,
    config_snapshot_ref  TEXT,
    context_snapshot_ref TEXT,
    agent_binding_id     TEXT,
    resume_attempt_id    TEXT,
    started_at           INTEGER NOT NULL DEFAULT 0,
    ended_at             INTEGER NOT NULL DEFAULT 0,
    error_code           TEXT,
    error_message        TEXT,
    version              INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_runs_tenant_session ON runs (tenant_id, session_id);
CREATE INDEX IF NOT EXISTS idx_runs_tenant_status ON runs (tenant_id, status);

CREATE TABLE IF NOT EXISTS agent_events (
    event_id        TEXT    NOT NULL PRIMARY KEY,
    run_id          TEXT    NOT NULL,
    sequence        INTEGER NOT NULL,
    tenant_id       TEXT,
    session_id      TEXT,
    step_id         TEXT,
    agent_id        TEXT,
    trace_id        TEXT,
    span_id         TEXT,
    parent_span_id  TEXT,
    agent_type      TEXT,
    runtime         TEXT,
    event_type      TEXT    NOT NULL,
    visibility      TEXT    NOT NULL,
    schema_version  TEXT    NOT NULL,
    idempotency_key TEXT,
    payload         TEXT,
    payload_preview TEXT,
    payload_ref     TEXT,
    usage           TEXT,
    debug_ref       TEXT,
    error           TEXT,
    created_at      INTEGER NOT NULL,
    UNIQUE (run_id, sequence),
    UNIQUE (tenant_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_events_run_seq ON agent_events (run_id, sequence, visibility);

-- sessions:长生命周期会话容器。List 走 keyset 分页(updated_at desc, session_id desc)。
CREATE TABLE IF NOT EXISTS sessions (
    session_id TEXT    NOT NULL PRIMARY KEY,
    tenant_id  TEXT    NOT NULL,
    user_id    TEXT,
    channel    TEXT,
    agent_id   TEXT,
    title      TEXT,
    status     TEXT    NOT NULL,
    created_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL DEFAULT 0,
    version    INTEGER NOT NULL DEFAULT 1,
    metadata   TEXT
);
CREATE INDEX IF NOT EXISTS idx_sessions_tenant_user_updated ON sessions (tenant_id, user_id, updated_at DESC, session_id DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_tenant_status ON sessions (tenant_id, status);

-- messages:面向用户的会话消息。List 走 before/after_message_id(created_at asc, message_id asc)。
CREATE TABLE IF NOT EXISTS messages (
    message_id      TEXT    NOT NULL PRIMARY KEY,
    session_id      TEXT    NOT NULL,
    turn_id         TEXT,
    run_id          TEXT,
    tenant_id       TEXT    NOT NULL,
    role            TEXT    NOT NULL,
    visibility      TEXT    NOT NULL,
    content_ref     TEXT,
    content_preview TEXT,
    created_at      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_messages_session_created ON messages (session_id, created_at, message_id);
CREATE INDEX IF NOT EXISTS idx_messages_tenant ON messages (tenant_id);

-- model_usage_records:模型网关写入的账单/用量事实。EventStore 保留生命周期流水；
-- 这里保留可查询、可聚合的 normalized usage/cost。
CREATE TABLE IF NOT EXISTS model_usage_records (
    usage_id          TEXT    NOT NULL PRIMARY KEY,
    request_id        TEXT,
    trace_id          TEXT,
    tenant_id         TEXT    NOT NULL,
    session_id        TEXT,
    run_id            TEXT,
    agent_id          TEXT,
    provider          TEXT,
    model             TEXT,
    attempt           INTEGER NOT NULL DEFAULT 0,
    fallback_applied  INTEGER NOT NULL DEFAULT 0,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    usage_source      TEXT,
    currency          TEXT,
    estimated_cost    REAL    NOT NULL DEFAULT 0,
    total_latency_ms  INTEGER NOT NULL DEFAULT 0,
    first_token_observed INTEGER NOT NULL DEFAULT 0,
    first_token_ms     INTEGER NOT NULL DEFAULT 0,
    generation_duration_ms INTEGER NOT NULL DEFAULT 0,
    output_tokens_per_second REAL NOT NULL DEFAULT 0,
    cache_hit         INTEGER NOT NULL DEFAULT 0,
    output_ref        TEXT,
    created_at        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_usage_tenant_created ON model_usage_records (tenant_id, created_at);
CREATE INDEX IF NOT EXISTS idx_usage_session_created ON model_usage_records (tenant_id, session_id, created_at);
CREATE INDEX IF NOT EXISTS idx_usage_run ON model_usage_records (run_id);
CREATE INDEX IF NOT EXISTS idx_usage_agent_created ON model_usage_records (tenant_id, agent_id, created_at);

-- steps:run 内部步骤记录。主键 (run_id, step_id) 实现 upsert。
CREATE TABLE IF NOT EXISTS steps (
    step_id        TEXT    NOT NULL,
    run_id         TEXT    NOT NULL,
    parent_step_id TEXT,
    tenant_id      TEXT,
    step_type      TEXT    NOT NULL,
    name           TEXT,
    status         TEXT    NOT NULL,
    started_at     INTEGER NOT NULL DEFAULT 0,
    ended_at       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (run_id, step_id)
);
CREATE INDEX IF NOT EXISTS idx_steps_run_started ON steps (run_id, started_at);

-- checkpoint_meta:断点续跑元数据(内容存 artifact,经 state_ref 引用)。
CREATE TABLE IF NOT EXISTS checkpoint_meta (
    checkpoint_id  TEXT    NOT NULL PRIMARY KEY,
    run_id         TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    runtime        TEXT,
    type           TEXT    NOT NULL,
    state_ref      TEXT    NOT NULL,
    event_sequence INTEGER NOT NULL DEFAULT 0,
    created_reason TEXT,
    created_at     INTEGER NOT NULL DEFAULT 0,
    expires_at     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_checkpoints_run_created ON checkpoint_meta (run_id, created_at);
CREATE INDEX IF NOT EXISTS idx_checkpoints_tenant ON checkpoint_meta (tenant_id);

-- control_requests:AskUser/HITL/权限/elicitation 交互。状态用 CAS + version 自增。
CREATE TABLE IF NOT EXISTS control_requests (
    request_id        TEXT    NOT NULL PRIMARY KEY,
    run_id            TEXT    NOT NULL,
    tenant_id         TEXT    NOT NULL,
    checkpoint_id     TEXT,
    type              TEXT    NOT NULL,
    status            TEXT    NOT NULL,
    resume_token_hash TEXT,
    tool_use_id       TEXT,
    prompt_preview    TEXT,
    response_ref      TEXT,
    schema_version    TEXT    NOT NULL,
    created_at        INTEGER NOT NULL DEFAULT 0,
    expires_at        INTEGER NOT NULL DEFAULT 0,
    version           INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_control_run ON control_requests (run_id);
CREATE INDEX IF NOT EXISTS idx_control_tenant_status ON control_requests (tenant_id, status);

-- idempotency_keys:命名空间化的通用一次性键；Resume claim 由 runs 的 attempt owner 管理。
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id  TEXT    NOT NULL,
    namespace  TEXT    NOT NULL,
    idem_key   TEXT    NOT NULL,
    expires_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, namespace, idem_key)
);

CREATE TABLE IF NOT EXISTS open_turn_idempotency (
    tenant_id        TEXT NOT NULL,
    user_id          TEXT NOT NULL,
    session_scope    TEXT NOT NULL,
    idem_key         TEXT NOT NULL,
    request_hash     TEXT NOT NULL,
    session_id       TEXT NOT NULL,
    run_id           TEXT NOT NULL,
    message_id       TEXT NOT NULL,
    created_at       INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, user_id, session_scope, idem_key)
);
