-- Restore Ledger logical IDs from the rejected BIGINT physical-ID model.
--
-- IMPORTANT:
-- This DDL only restores column types. If the tables already contain rows
-- written through stableIntID/SHA-256 mapping, those BIGINT values are hashes
-- and cannot be losslessly converted back into the original logical IDs.
-- For non-empty hashed tables, rebuild or backfill from an upstream source of
-- truth before marking the schema ready.
--
-- Recommended preflight:
--   SELECT 'sessions', COUNT(*) FROM sessions
--   UNION ALL SELECT 'messages', COUNT(*) FROM messages
--   UNION ALL SELECT 'model_usage_records', COUNT(*) FROM model_usage_records
--   UNION ALL SELECT 'runs', COUNT(*) FROM runs
--   UNION ALL SELECT 'steps', COUNT(*) FROM steps
--   UNION ALL SELECT 'agent_events', COUNT(*) FROM agent_events
--   UNION ALL SELECT 'checkpoint_meta', COUNT(*) FROM checkpoint_meta
--   UNION ALL SELECT 'control_requests', COUNT(*) FROM control_requests
--   UNION ALL SELECT 'idempotency_keys', COUNT(*) FROM idempotency_keys
--   UNION ALL SELECT 'open_turn_idempotency', COUNT(*) FROM open_turn_idempotency;

ALTER TABLE sessions
    MODIFY COLUMN id VARCHAR(64) NOT NULL COMMENT '会话ID';

ALTER TABLE messages
    MODIFY COLUMN id VARCHAR(64) NOT NULL COMMENT '消息ID';

ALTER TABLE model_usage_records
    MODIFY COLUMN usage_id VARCHAR(64) NOT NULL COMMENT '用量记录ID';

ALTER TABLE runs
    MODIFY COLUMN run_id VARCHAR(64) NOT NULL COMMENT '运行ID';

ALTER TABLE steps
    MODIFY COLUMN step_id VARCHAR(64) NOT NULL COMMENT '步骤ID',
    MODIFY COLUMN run_id VARCHAR(64) NOT NULL COMMENT '运行ID',
    MODIFY COLUMN parent_step_id VARCHAR(64) NULL COMMENT '父步骤ID';

ALTER TABLE agent_events
    MODIFY COLUMN event_id VARCHAR(64) NOT NULL COMMENT '事件ID';

ALTER TABLE checkpoint_meta
    MODIFY COLUMN checkpoint_id VARCHAR(64) NOT NULL COMMENT '检查点ID';

ALTER TABLE control_requests
    MODIFY COLUMN request_id VARCHAR(64) NOT NULL COMMENT '控制请求ID';

ALTER TABLE idempotency_keys
    MODIFY COLUMN id VARCHAR(320) NOT NULL COMMENT '幂等键 tenant|namespace|key';

ALTER TABLE open_turn_idempotency
    MODIFY COLUMN tenant_id VARCHAR(64) NOT NULL COMMENT '租户ID',
    MODIFY COLUMN user_id VARCHAR(64) NOT NULL COMMENT '用户ID',
    MODIFY COLUMN session_scope VARCHAR(128) NOT NULL COMMENT '幂等会话范围',
    MODIFY COLUMN idem_key VARCHAR(128) NOT NULL COMMENT '幂等键';

-- Optional postcheck:
--   SHOW COLUMNS FROM runs LIKE 'run_id';
--   SHOW COLUMNS FROM agent_events LIKE 'event_id';
--   SHOW COLUMNS FROM checkpoint_meta LIKE 'checkpoint_id';
--   SHOW COLUMNS FROM control_requests LIKE 'request_id';
