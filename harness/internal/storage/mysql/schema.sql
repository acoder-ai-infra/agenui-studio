-- Session/Run storage ledger — MySQL OLTP schema (Phase 1 prototype, D3=MySQL).
--
-- Every business table carries tenant_id and composite indexes for tenant-scoped
-- queries (session-run-storage-design.md §13.1). The EventStore enforces the
-- core invariants via unique constraints:
--   (run_id, sequence)  -> run-monotonic, gap-tolerant sequence
--   event_id            -> event-id idempotency
--   (tenant_id, idempotency_key) -> idempotency-key idempotency (tenant scoped)
--
-- schema_version columns support portability migration (portability §7).

CREATE TABLE IF NOT EXISTS sessions (
    id           VARCHAR(64)  NOT NULL COMMENT '会话ID',
    tenant_id    VARCHAR(64)  NOT NULL COMMENT '租户ID',
    user_id      VARCHAR(64)  NOT NULL COMMENT '用户ID',
    channel      VARCHAR(32)  NULL COMMENT '会话渠道',
    agent_id     VARCHAR(128) NULL COMMENT '默认 Agent ID',
    title        VARCHAR(512) NULL COMMENT '会话标题',
    status       VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT '会话状态',
    created_at   DATETIME(3)  NOT NULL COMMENT '创建时间',
    updated_at   DATETIME(3)  NOT NULL COMMENT '更新时间',
    version      BIGINT       NOT NULL DEFAULT 1 COMMENT '乐观锁版本号',
    metadata     JSON         NULL COMMENT '会话元数据',
    PRIMARY KEY (id),
    KEY idx_sessions_tenant_user (tenant_id, user_id, updated_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='会话管理表';

CREATE TABLE IF NOT EXISTS messages (
    id              VARCHAR(64)  NOT NULL COMMENT '消息ID',
    session_id      VARCHAR(64)  NOT NULL COMMENT '会话ID',
    turn_id         VARCHAR(64)  NULL COMMENT '轮次ID',
    run_id          VARCHAR(64)  NULL COMMENT '运行ID',
    tenant_id       VARCHAR(64)  NOT NULL COMMENT '租户ID',
    role            VARCHAR(16)  NOT NULL COMMENT '消息角色',
    visibility      VARCHAR(16)  NOT NULL COMMENT '可见性',
    content_ref     VARCHAR(512) NULL COMMENT '消息内容制品引用',
    content_preview TEXT         NULL COMMENT '消息内容预览',
    created_at      DATETIME(3)  NOT NULL COMMENT '创建时间',
    PRIMARY KEY (id),
    KEY idx_messages_session (tenant_id, session_id, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='消息记录表';

CREATE TABLE IF NOT EXISTS model_usage_records (
    usage_id          VARCHAR(64)  NOT NULL COMMENT '用量记录ID',
    request_id        VARCHAR(64)  NULL COMMENT '模型网关请求ID',
    trace_id          VARCHAR(64)  NULL COMMENT '链路追踪ID',
    tenant_id         VARCHAR(64)  NOT NULL COMMENT '租户ID',
    session_id        VARCHAR(64)  NULL COMMENT '会话ID',
    run_id            VARCHAR(64)  NULL COMMENT '运行ID',
    agent_id          VARCHAR(128) NULL COMMENT 'Agent ID',
    provider          VARCHAR(64)  NULL COMMENT '模型供应商',
    model             VARCHAR(128) NULL COMMENT '模型名称',
    attempt           INT          NOT NULL DEFAULT 0 COMMENT '模型调用尝试序号',
    fallback_applied  TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '是否发生模型降级，0 否 1 是',
    prompt_tokens     BIGINT       NOT NULL DEFAULT 0 COMMENT '输入 token 数',
    completion_tokens BIGINT       NOT NULL DEFAULT 0 COMMENT '输出 token 数',
    reasoning_tokens  BIGINT       NOT NULL DEFAULT 0 COMMENT '推理 token 数',
    cache_read_tokens BIGINT       NOT NULL DEFAULT 0 COMMENT '缓存读取 token 数',
    cache_write_tokens BIGINT      NOT NULL DEFAULT 0 COMMENT '缓存写入 token 数',
    usage_source      VARCHAR(32)  NULL COMMENT '用量来源',
    currency          VARCHAR(16)  NULL COMMENT '计费币种',
    estimated_cost    DECIMAL(18,8) NOT NULL DEFAULT 0 COMMENT '预估费用',
    total_latency_ms  BIGINT       NOT NULL DEFAULT 0 COMMENT '总耗时，单位毫秒',
    first_token_observed TINYINT(1) NOT NULL DEFAULT 0 COMMENT '是否观测到首 token，0 否 1 是',
    first_token_ms     BIGINT       NOT NULL DEFAULT 0 COMMENT '首 token 耗时，单位毫秒',
    generation_duration_ms BIGINT   NOT NULL DEFAULT 0 COMMENT '生成阶段耗时，单位毫秒',
    output_tokens_per_second DECIMAL(18,6) NOT NULL DEFAULT 0 COMMENT '每秒输出 token 数',
    cache_hit         TINYINT(1)   NOT NULL DEFAULT 0 COMMENT '是否命中缓存，0 否 1 是',
    output_ref        VARCHAR(512) NULL COMMENT '模型输出制品引用',
    created_at        DATETIME(3)  NOT NULL COMMENT '记录创建时间',
    PRIMARY KEY (usage_id),
    KEY idx_usage_tenant_created (tenant_id, created_at),
    KEY idx_usage_session_created (tenant_id, session_id, created_at),
    KEY idx_usage_run (run_id),
    KEY idx_usage_agent_created (tenant_id, agent_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='模型调用用量记录表';

CREATE TABLE IF NOT EXISTS runs (
    run_id               VARCHAR(64)  NOT NULL COMMENT '运行ID',
    session_id           VARCHAR(64)  NOT NULL COMMENT '会话ID',
    turn_id              VARCHAR(64)  NULL COMMENT '轮次ID',
    parent_run_id        VARCHAR(64)  NULL COMMENT '父运行ID',
    tenant_id            VARCHAR(64)  NOT NULL COMMENT '租户ID',
    agent_id             VARCHAR(128) NULL COMMENT 'Agent ID',
    runtime              VARCHAR(32)  NULL COMMENT '运行时类型',
    runtime_binding      JSON         NULL COMMENT '运行时绑定事实',
    status               VARCHAR(24)  NOT NULL COMMENT '运行状态',
    trace_id             VARCHAR(64)  NULL COMMENT '链路追踪ID',
    config_snapshot_ref  VARCHAR(512) NULL COMMENT '配置快照引用',
    context_snapshot_ref VARCHAR(512) NULL COMMENT '上下文快照引用',
    agent_binding_id     VARCHAR(128) NULL COMMENT 'Agent 绑定ID',
    resume_attempt_id    VARCHAR(128) NULL COMMENT '恢复尝试ID',
    started_at           DATETIME(3)  NULL COMMENT '开始时间',
    ended_at             DATETIME(3)  NULL COMMENT '结束时间',
    error_code           VARCHAR(64)  NULL COMMENT '错误码',
    error_message        TEXT         NULL COMMENT '错误信息',
    version              BIGINT       NOT NULL DEFAULT 1 COMMENT '乐观锁版本号',
    PRIMARY KEY (run_id),
    KEY idx_runs_tenant_session (tenant_id, session_id),
    KEY idx_runs_tenant_status (tenant_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='运行记录表';

CREATE TABLE IF NOT EXISTS steps (
    step_id        VARCHAR(64)  NOT NULL COMMENT '步骤ID',
    run_id         VARCHAR(64)  NOT NULL COMMENT '运行ID',
    parent_step_id VARCHAR(64)  NULL COMMENT '父步骤ID',
    step_type      VARCHAR(32)  NOT NULL COMMENT '步骤类型',
    name           VARCHAR(256) NULL COMMENT '步骤名称',
    status         VARCHAR(24)  NOT NULL COMMENT '步骤状态',
    started_at     DATETIME(3)  NOT NULL COMMENT '开始时间',
    ended_at       DATETIME(3)  NULL COMMENT '结束时间',
    PRIMARY KEY (run_id, step_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='运行步骤表';

CREATE TABLE IF NOT EXISTS agent_events (
    event_id        VARCHAR(64)  NOT NULL COMMENT '事件ID',
    run_id          VARCHAR(64)  NOT NULL COMMENT '运行ID',
    sequence        BIGINT       NOT NULL COMMENT '事件序列号',
    tenant_id       VARCHAR(64)  NULL COMMENT '租户ID',
    session_id      VARCHAR(64)  NULL COMMENT '会话ID',
    step_id         VARCHAR(64)  NULL COMMENT '步骤ID',
    agent_id        VARCHAR(128) NULL COMMENT 'Agent ID',
    trace_id        VARCHAR(64)  NULL COMMENT '链路追踪ID',
    span_id         VARCHAR(64)  NULL COMMENT 'Span ID',
    parent_span_id  VARCHAR(64)  NULL COMMENT '父 Span ID',
    agent_type      VARCHAR(64)  NULL COMMENT 'Agent 类型',
    runtime         VARCHAR(32)  NULL COMMENT '运行时类型',
    event_type      VARCHAR(64)  NOT NULL COMMENT '事件类型',
    visibility      VARCHAR(16)  NOT NULL COMMENT '可见性',
    schema_version  VARCHAR(64)  NOT NULL COMMENT 'Schema 版本',
    idempotency_key VARCHAR(128) NULL COMMENT '幂等键',
    payload         JSON         NULL COMMENT '事件负载',
    payload_preview JSON         NULL COMMENT '事件负载预览',
    payload_ref     VARCHAR(512) NULL COMMENT '事件负载制品引用',
    usage_data      JSON         NULL COMMENT '模型或资源用量数据',
    debug_ref       VARCHAR(512) NULL COMMENT '调试制品引用',
    error           JSON         NULL COMMENT '错误信息',
    created_at      DATETIME(3)  NOT NULL COMMENT '创建时间',
    PRIMARY KEY (event_id),
    UNIQUE KEY uk_events_run_seq (run_id, sequence),
    UNIQUE KEY uk_events_idem (tenant_id, idempotency_key),
    KEY idx_events_run_seq (run_id, sequence, visibility)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Agent 运行事件表';

CREATE TABLE IF NOT EXISTS checkpoint_meta (
    checkpoint_id  VARCHAR(64)  NOT NULL COMMENT '检查点ID',
    run_id         VARCHAR(64)  NOT NULL COMMENT '运行ID',
    tenant_id      VARCHAR(64)  NOT NULL COMMENT '租户ID',
    runtime        VARCHAR(32)  NULL COMMENT '运行时类型',
    type           VARCHAR(32)  NOT NULL COMMENT '检查点类型',
    state_ref      VARCHAR(512) NOT NULL COMMENT '状态制品引用',
    event_sequence BIGINT       NOT NULL DEFAULT 0 COMMENT '对应事件序列号',
    created_reason VARCHAR(32)  NULL COMMENT '创建原因',
    created_at     DATETIME(3)  NOT NULL COMMENT '创建时间',
    expires_at     DATETIME(3)  NULL COMMENT '过期时间',
    PRIMARY KEY (checkpoint_id),
    KEY idx_ckpt_run (tenant_id, run_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='运行检查点元数据表';

CREATE TABLE IF NOT EXISTS control_requests (
    request_id        VARCHAR(64)  NOT NULL COMMENT '控制请求ID',
    run_id            VARCHAR(64)  NOT NULL COMMENT '运行ID',
    tenant_id         VARCHAR(64)  NOT NULL COMMENT '租户ID',
    checkpoint_id     VARCHAR(64)  NULL COMMENT '检查点ID',
    type              VARCHAR(32)  NOT NULL COMMENT '控制请求类型',
    status            VARCHAR(16)  NOT NULL COMMENT '控制请求状态',
    resume_token_hash VARCHAR(128) NULL COMMENT '恢复令牌哈希',
    tool_use_id       VARCHAR(64)  NULL COMMENT '工具调用ID',
    prompt_preview    TEXT         NULL COMMENT '提示预览',
    response_ref      VARCHAR(512) NULL COMMENT '响应制品引用',
    schema_version    VARCHAR(64)  NOT NULL COMMENT 'Schema 版本',
    created_at        DATETIME(3)  NOT NULL COMMENT '创建时间',
    expires_at        DATETIME(3)  NULL COMMENT '过期时间',
    version           BIGINT       NOT NULL DEFAULT 1 COMMENT '乐观锁版本号',
    PRIMARY KEY (request_id),
    KEY idx_ctrl_tenant_status (tenant_id, status, expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='控制请求表';

CREATE TABLE IF NOT EXISTS idempotency_keys (
    id         VARCHAR(320) NOT NULL COMMENT '幂等键 tenant|namespace|key',
    expires_at DATETIME(3)  NULL COMMENT '过期时间',
    created_at DATETIME(3)  NOT NULL COMMENT '创建时间',
    PRIMARY KEY (id),
    KEY idx_idem_expiry (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='幂等键表';

CREATE TABLE IF NOT EXISTS open_turn_idempotency (
    tenant_id VARCHAR(64) NOT NULL COMMENT '租户ID',
    user_id VARCHAR(64) NOT NULL COMMENT '用户ID',
    session_scope VARCHAR(128) NOT NULL COMMENT '幂等会话范围',
    idem_key VARCHAR(128) NOT NULL COMMENT '幂等键',
    request_hash VARCHAR(64) NOT NULL COMMENT '请求内容哈希',
    session_id VARCHAR(64) NOT NULL COMMENT '会话ID',
    run_id VARCHAR(64) NOT NULL COMMENT '运行ID',
    message_id VARCHAR(64) NOT NULL COMMENT '消息ID',
    created_at DATETIME(3) NOT NULL COMMENT '创建时间',
    PRIMARY KEY (tenant_id,user_id,session_scope,idem_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='OpenTurn 幂等控制表';
