-- Agent Registry 的 MySQL 8 全新建库基线。
-- 连接池、迁移顺序和关闭由应用 Composition Root 统一管理；Registry 只拥有以下四张表。
-- Binding、Run、AgentEvent、Scheduler 等事实属于各自 Storage owner，不在这里建表。
-- 长业务身份使用完整字段保存、应用侧 SHA-256 摘要保证唯一，查询索引采用有界前缀，
-- 兼容公司 767-byte 二级索引预检且不牺牲业务身份的精确唯一性。
-- 摘要列必须由 Registry 使用长度前缀 tuple 编码后写入；禁止依赖 trigger 或生成列，
-- 使用摘要索引避免可注入分隔符导致不同业务身份串位。

CREATE TABLE IF NOT EXISTS agent_registry_entries (
  row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增行主键，仅用于 InnoDB 聚簇索引',
  agent_id VARCHAR(255) NOT NULL COMMENT 'Agent 全局业务标识',
  agent_type VARCHAR(255) NOT NULL COMMENT 'Agent 类型标识',
  version VARCHAR(255) NOT NULL COMMENT 'Agent 不可变版本号',
  status VARCHAR(32) NOT NULL COMMENT 'Agent 版本状态：enabled 或 disabled',
  config_hash VARCHAR(80) NOT NULL COMMENT 'EffectiveConfig 内容哈希',
  gray_percent INT NOT NULL DEFAULT 0 COMMENT '版本灰度百分比，范围 0 到 100',
  config_json JSON NOT NULL COMMENT '注册时原始 Agent 配置快照',
  card_json JSON NOT NULL COMMENT '编译后的能力卡快照',
  effective_json JSON NOT NULL COMMENT '编译后的 EffectiveConfig 快照',
  registered_at_ms BIGINT NOT NULL COMMENT '注册时间，Unix 毫秒',
  activated_at_ms BIGINT NOT NULL DEFAULT 0 COMMENT '最近启用时间，Unix 毫秒；未启用为 0',
  revision BIGINT NOT NULL DEFAULT 1 COMMENT '乐观锁版本号',
  agent_version_digest BINARY(32) NOT NULL COMMENT '应用计算的 agent_id 与 version 唯一身份摘要',
  type_version_digest BINARY(32) NOT NULL COMMENT '应用计算的 agent_type 与 version 唯一身份摘要',
  PRIMARY KEY (row_id),
  UNIQUE KEY uk_registry_entry_agent_version (agent_version_digest),
  UNIQUE KEY uk_registry_entry_type_version (type_version_digest),
  KEY idx_registry_entry_agent_lookup (agent_id(64), status, activated_at_ms DESC, registered_at_ms DESC, version(48)),
  KEY idx_registry_entry_type_lookup (agent_type(64), status, activated_at_ms DESC, registered_at_ms DESC, version(48)),
  CONSTRAINT chk_registry_entry_status CHECK (status IN ('enabled', 'disabled')),
  CONSTRAINT chk_registry_entry_gray CHECK (gray_percent BETWEEN 0 AND 100),
  CONSTRAINT chk_registry_entry_registered_at CHECK (registered_at_ms >= 0),
  CONSTRAINT chk_registry_entry_activated_at CHECK (activated_at_ms >= 0),
  CONSTRAINT chk_registry_entry_revision CHECK (revision >= 1)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='Agent 版本目录与编译结果事实表';

CREATE TABLE IF NOT EXISTS agent_registry_config_snapshots (
  row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增行主键，仅用于 InnoDB 聚簇索引',
  config_snapshot_ref VARCHAR(640) NOT NULL COMMENT '不可变配置快照引用',
  agent_id VARCHAR(255) NOT NULL COMMENT '快照所属 Agent 标识',
  version VARCHAR(255) NOT NULL COMMENT '快照所属 Agent 版本',
  execution_mode VARCHAR(32) NOT NULL COMMENT '快照绑定的执行范式',
  config_hash VARCHAR(80) NOT NULL COMMENT 'EffectiveConfig 内容哈希',
  effective_json JSON NOT NULL COMMENT '执行范式对应的 EffectiveConfig 快照',
  created_at_ms BIGINT NOT NULL COMMENT '快照创建时间，Unix 毫秒',
  snapshot_ref_digest BINARY(32) NOT NULL COMMENT '应用计算的 config_snapshot_ref 唯一身份摘要',
  agent_mode_digest BINARY(32) NOT NULL COMMENT '应用计算的 Agent 版本与执行范式唯一身份摘要',
  agent_hash_digest BINARY(32) NOT NULL COMMENT '应用计算的 Agent 版本与配置哈希唯一身份摘要',
  PRIMARY KEY (row_id),
  UNIQUE KEY uk_registry_snapshot_ref (snapshot_ref_digest),
  UNIQUE KEY uk_registry_snapshot_agent_mode (agent_mode_digest),
  UNIQUE KEY uk_registry_snapshot_agent_hash (agent_hash_digest),
  KEY idx_registry_snapshot_ref_lookup (config_snapshot_ref(128)),
  KEY idx_registry_snapshot_agent_mode_time (agent_id(64), version(48), execution_mode, created_at_ms DESC),
  CONSTRAINT chk_registry_snapshot_mode CHECK (execution_mode IN ('direct_action', 'single_agent', 'deep_agent', 'workflow', 'graph')),
  CONSTRAINT chk_registry_snapshot_created_at CHECK (created_at_ms >= 0)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='Agent EffectiveConfig 不可变快照表';

CREATE TABLE IF NOT EXISTS agent_registry_audit_events (
  row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增行主键，仅用于 InnoDB 聚簇索引',
  event_id VARCHAR(255) NOT NULL COMMENT 'Registry 审计事件唯一标识',
  event_type VARCHAR(80) NOT NULL COMMENT 'Registry 审计事件类型',
  tenant_id VARCHAR(255) NOT NULL DEFAULT '' COMMENT '事件所属租户标识',
  agent_id VARCHAR(255) NOT NULL DEFAULT '' COMMENT '事件关联的 Agent 标识',
  agent_version VARCHAR(255) NOT NULL DEFAULT '' COMMENT '事件关联的 Agent 版本',
  trace_id VARCHAR(255) NOT NULL DEFAULT '' COMMENT '全链路 Trace 标识',
  span_id VARCHAR(255) NOT NULL DEFAULT '' COMMENT '当前 Span 标识',
  parent_span_id VARCHAR(255) NOT NULL DEFAULT '' COMMENT '父 Span 标识',
  request_id VARCHAR(255) NOT NULL DEFAULT '' COMMENT '请求唯一标识',
  run_id VARCHAR(255) NOT NULL DEFAULT '' COMMENT '关联 Run 标识',
  selection_hash VARCHAR(255) NOT NULL DEFAULT '' COMMENT 'Agent 选择事实哈希',
  binding_hash VARCHAR(255) NOT NULL DEFAULT '' COMMENT 'Agent Binding 事实哈希',
  config_hash VARCHAR(80) NOT NULL DEFAULT '' COMMENT '关联 EffectiveConfig 内容哈希',
  actor VARCHAR(255) NOT NULL DEFAULT '' COMMENT '执行注册操作的主体',
  reason VARCHAR(255) NOT NULL DEFAULT '' COMMENT '注册操作原因',
  diff_summary TEXT NOT NULL COMMENT '配置变更摘要，不保存敏感正文',
  occurred_at_ms BIGINT NOT NULL COMMENT '事件发生时间，Unix 毫秒',
  event_id_digest BINARY(32) NOT NULL COMMENT '应用计算的 event_id 唯一身份摘要',
  PRIMARY KEY (row_id),
  UNIQUE KEY uk_registry_audit_event_id (event_id_digest),
  KEY idx_registry_audit_agent_time (agent_id(64), agent_version(48), occurred_at_ms DESC, event_id(48)),
  KEY idx_registry_audit_tenant_time (tenant_id(64), occurred_at_ms DESC, event_id(48)),
  KEY idx_registry_audit_trace_time (trace_id(64), occurred_at_ms, event_id(48)),
  KEY idx_registry_audit_run_time (run_id(64), occurred_at_ms, event_id(48)),
  KEY idx_registry_audit_type_time (event_type, occurred_at_ms DESC, event_id(48)),
  CONSTRAINT chk_registry_audit_occurred_at CHECK (occurred_at_ms >= 0)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='Agent Registry 配置变更审计事件表';

CREATE TABLE IF NOT EXISTS agent_registry_prompt_versions (
  row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增行主键，仅用于 InnoDB 聚簇索引',
  prompt_ref VARCHAR(512) NOT NULL COMMENT 'System Prompt 业务引用',
  version VARCHAR(128) NOT NULL COMMENT 'System Prompt 不可变版本号',
  content LONGTEXT NULL COMMENT 'System Prompt 原文；与 content_ref 二选一',
  content_ref VARCHAR(1024) NULL COMMENT 'System Prompt 外部内容引用；与 content 二选一',
  content_hash VARCHAR(80) NOT NULL COMMENT 'System Prompt 正文内容哈希',
  variables_json JSON NOT NULL COMMENT '允许插值的变量名 JSON 数组',
  created_at_ms BIGINT NOT NULL COMMENT 'Prompt 版本创建时间，Unix 毫秒',
  created_by VARCHAR(255) NOT NULL DEFAULT '' COMMENT 'Prompt 版本创建主体',
  prompt_identity_digest BINARY(32) NOT NULL COMMENT '应用计算的 prompt_ref 与 version 唯一身份摘要',
  PRIMARY KEY (row_id),
  UNIQUE KEY uk_registry_prompt_ref_version (prompt_identity_digest),
  KEY idx_registry_prompt_ref_version_lookup (prompt_ref(96), version(48)),
  CONSTRAINT chk_registry_prompt_content_source CHECK (
    (content IS NOT NULL AND CHAR_LENGTH(content) > 0 AND content_ref IS NULL)
    OR
    (content IS NULL AND content_ref IS NOT NULL AND CHAR_LENGTH(TRIM(content_ref)) > 0)
  ),
  CONSTRAINT chk_registry_prompt_hash CHECK (CHAR_LENGTH(TRIM(content_hash)) > 0),
  CONSTRAINT chk_registry_prompt_variables CHECK (JSON_TYPE(variables_json) = 'ARRAY'),
  CONSTRAINT chk_registry_prompt_created_at CHECK (created_at_ms >= 0)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='System Prompt 不可变版本与正文事实表';
