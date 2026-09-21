-- Agent Registry 的 SQLite 开发/单机运行迁移。
-- 当前 Agent 目录是全局事实源；tenant_id 只记录审计上下文，不参与配置主键。
-- 如果以后支持租户私有 Agent，需要先升级 Store 查询契约，再单独迁移主键和索引。
--
-- Binding 与 Orchestrator 不在这里建表：EffectiveBinding、Run 槽位和 canonical
-- AgentEvent 统一通过 StorageWritePlan 交给 Session/Run Storage owner 持久化。

CREATE TABLE IF NOT EXISTS agent_registry_entries (
  agent_id TEXT NOT NULL CHECK (length(trim(agent_id)) > 0),
  agent_type TEXT NOT NULL CHECK (length(trim(agent_type)) > 0),
  version TEXT NOT NULL CHECK (length(trim(version)) > 0),
  status TEXT NOT NULL CHECK (status IN ('enabled', 'disabled')),
  config_hash TEXT NOT NULL CHECK (length(config_hash) > 0),
  gray_percent INTEGER NOT NULL DEFAULT 0 CHECK (gray_percent BETWEEN 0 AND 100),
  config_json TEXT NOT NULL CHECK (length(config_json) > 0),
  card_json TEXT NOT NULL CHECK (length(card_json) > 0),
  effective_json TEXT NOT NULL CHECK (length(effective_json) > 0),
  registered_at_ms INTEGER NOT NULL CHECK (registered_at_ms >= 0),
  activated_at_ms INTEGER NOT NULL DEFAULT 0 CHECK (activated_at_ms >= 0),
  revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
  agent_version_digest BLOB NOT NULL CHECK (length(agent_version_digest) = 32),
  type_version_digest BLOB NOT NULL CHECK (length(type_version_digest) = 32),
  PRIMARY KEY (agent_id, version),
  UNIQUE (agent_type, version),
  UNIQUE (agent_version_digest),
  UNIQUE (type_version_digest)
);

-- 支持按 agent_id 或 agent_type 解析最新启用版本；精确版本查询由主键覆盖。
CREATE INDEX IF NOT EXISTS agent_registry_entries_agent_lookup_idx
  ON agent_registry_entries (
    agent_id,
    status,
    activated_at_ms DESC,
    registered_at_ms DESC,
    version DESC
  );

CREATE INDEX IF NOT EXISTS agent_registry_entries_type_lookup_idx
  ON agent_registry_entries (
    agent_type,
    status,
    activated_at_ms DESC,
    registered_at_ms DESC,
    version DESC
  );

-- 按引用保存最终 EffectiveConfig，供恢复、回放和分布式 worker 重建 Runtime 请求。
-- entries.effective_json 保留默认 mode 的注册态投影；本表冻结每个允许的 execution mode，
-- 注册时与 entries 在同一事务写入，在线解析只读取快照，不再临时重算。
CREATE TABLE IF NOT EXISTS agent_registry_config_snapshots (
  config_snapshot_ref TEXT NOT NULL PRIMARY KEY CHECK (length(trim(config_snapshot_ref)) > 0),
  agent_id TEXT NOT NULL CHECK (length(trim(agent_id)) > 0),
  version TEXT NOT NULL CHECK (length(trim(version)) > 0),
  execution_mode TEXT NOT NULL CHECK (
    execution_mode IN ('direct_action', 'single_agent', 'deep_agent', 'workflow', 'graph')
  ),
  config_hash TEXT NOT NULL CHECK (length(config_hash) > 0),
  effective_json TEXT NOT NULL CHECK (length(effective_json) > 0),
  created_at_ms INTEGER NOT NULL CHECK (created_at_ms >= 0),
  snapshot_ref_digest BLOB NOT NULL CHECK (length(snapshot_ref_digest) = 32),
  agent_mode_digest BLOB NOT NULL CHECK (length(agent_mode_digest) = 32),
  agent_hash_digest BLOB NOT NULL CHECK (length(agent_hash_digest) = 32),
  UNIQUE (agent_id, version, execution_mode),
  UNIQUE (agent_id, version, config_hash),
  UNIQUE (snapshot_ref_digest),
  UNIQUE (agent_mode_digest),
  UNIQUE (agent_hash_digest)
);

CREATE INDEX IF NOT EXISTS agent_registry_config_snapshots_agent_time_idx
  ON agent_registry_config_snapshots (agent_id, version, execution_mode, created_at_ms DESC);

-- Registry 配置发布、启停、灰度和回滚等耐久审计事实。
CREATE TABLE IF NOT EXISTS agent_registry_audit_events (
  event_id TEXT NOT NULL PRIMARY KEY CHECK (length(trim(event_id)) > 0),
  event_type TEXT NOT NULL CHECK (length(trim(event_type)) > 0),
  tenant_id TEXT NOT NULL DEFAULT '',
  agent_id TEXT NOT NULL DEFAULT '',
  agent_version TEXT NOT NULL DEFAULT '',
  trace_id TEXT NOT NULL DEFAULT '',
  span_id TEXT NOT NULL DEFAULT '',
  parent_span_id TEXT NOT NULL DEFAULT '',
  request_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  selection_hash TEXT NOT NULL DEFAULT '',
  binding_hash TEXT NOT NULL DEFAULT '',
  config_hash TEXT NOT NULL DEFAULT '',
  actor TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  diff_summary TEXT NOT NULL DEFAULT '',
  occurred_at_ms INTEGER NOT NULL CHECK (occurred_at_ms >= 0),
  event_id_digest BLOB NOT NULL CHECK (length(event_id_digest) = 32),
  UNIQUE (event_id_digest)
);

CREATE INDEX IF NOT EXISTS agent_registry_audit_events_agent_time_idx
  ON agent_registry_audit_events (
    agent_id,
    agent_version,
    occurred_at_ms DESC,
    event_id DESC
  );

CREATE INDEX IF NOT EXISTS agent_registry_audit_events_tenant_time_idx
  ON agent_registry_audit_events (tenant_id, occurred_at_ms DESC, event_id DESC);

CREATE INDEX IF NOT EXISTS agent_registry_audit_events_trace_time_idx
  ON agent_registry_audit_events (trace_id, occurred_at_ms, event_id);

CREATE INDEX IF NOT EXISTS agent_registry_audit_events_run_time_idx
  ON agent_registry_audit_events (run_id, occurred_at_ms, event_id);

CREATE INDEX IF NOT EXISTS agent_registry_audit_events_type_time_idx
  ON agent_registry_audit_events (event_type, occurred_at_ms DESC, event_id DESC);

-- 固定 system prompt 的不可变版本；联合主键只支持精确版本解析，不提供 latest。
CREATE TABLE IF NOT EXISTS agent_registry_prompt_versions (
  prompt_ref TEXT NOT NULL CHECK (length(trim(prompt_ref)) BETWEEN 1 AND 512),
  version TEXT NOT NULL CHECK (length(trim(version)) BETWEEN 1 AND 128),
  content TEXT,
  content_ref TEXT,
  content_hash TEXT NOT NULL CHECK (length(trim(content_hash)) > 0),
  variables_json TEXT NOT NULL CHECK (
    json_valid(variables_json)
    AND json_type(variables_json) = 'array'
  ),
  created_at_ms INTEGER NOT NULL CHECK (created_at_ms >= 0),
  created_by TEXT NOT NULL DEFAULT '',
  prompt_identity_digest BLOB NOT NULL CHECK (length(prompt_identity_digest) = 32),
  PRIMARY KEY (prompt_ref, version),
  UNIQUE (prompt_identity_digest),
  CHECK (
    (
      content IS NOT NULL
      AND length(content) > 0
      AND content_ref IS NULL
    )
    OR
    (
      content IS NULL
      AND content_ref IS NOT NULL
      AND length(trim(content_ref)) BETWEEN 1 AND 1024
    )
  )
);
