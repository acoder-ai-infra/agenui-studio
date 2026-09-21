-- MCP OAuth 授权的 MySQL 8 建表脚本。
-- Token 只保存应用侧 AEAD 加密后的密文；明文 Token、OAuth code、client secret 不落库。
-- 长业务身份使用完整字段保存，应用侧 SHA-256 摘要保证唯一，查询索引采用有界前缀，
-- 兼容公司 767-byte 二级索引预检且不牺牲业务身份的精确唯一性。
-- identity_digest 必须由应用使用长度前缀 tuple 编码后写入；禁止依赖 trigger 或生成列，
-- 使用摘要索引避免可注入分隔符导致不同业务身份串位。

CREATE TABLE IF NOT EXISTS mcp_oauth_grants (
  row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增行主键，仅用于 InnoDB 聚簇索引',
  tenant_id VARCHAR(255) NOT NULL COMMENT 'OAuth 授权所属租户标识',
  user_id VARCHAR(255) NOT NULL COMMENT 'OAuth 授权所属用户标识；MCP OAuth grant 必须绑定到具体用户',
  server_id VARCHAR(128) NOT NULL COMMENT 'MCP server 业务标识，对应租户 MCP 配置中的 server_id',
  provider VARCHAR(128) NOT NULL COMMENT 'OAuth provider 标识，对应 MCP server auth.provider',
  scope_hash VARCHAR(96) NOT NULL COMMENT '应用按 scopes 与 resource 规范化后计算的授权范围哈希',
  scopes_json JSON NOT NULL COMMENT '授权 scope JSON 数组快照，用于展示、审计与恢复授权上下文',
  resource VARCHAR(1024) NOT NULL DEFAULT '' COMMENT 'OAuth resource/audience；未配置时为空字符串',
  access_token_ciphertext TEXT NOT NULL COMMENT 'Access Token 密文，使用应用 AEAD codec 加密，禁止保存明文',
  refresh_token_ciphertext TEXT NOT NULL COMMENT 'Refresh Token 密文；无 refresh token 时为空字符串',
  access_expires_at_ms BIGINT NOT NULL DEFAULT 0 COMMENT 'Access Token 过期时间，Unix 毫秒；0 表示未知或不校验',
  refresh_expires_at_ms BIGINT NOT NULL DEFAULT 0 COMMENT 'Refresh Token 过期时间，Unix 毫秒；0 表示未知或不校验',
  status VARCHAR(32) NOT NULL COMMENT '授权状态：active 或 revoked',
  revision BIGINT NOT NULL COMMENT '乐观锁版本号，创建为 1，更新或撤销时递增',
  updated_at_ms BIGINT NOT NULL COMMENT '最近更新时间，Unix 毫秒',
  identity_digest BINARY(32) NOT NULL COMMENT '应用计算的 tenant/user/server/provider/scope_hash 唯一身份摘要',
  PRIMARY KEY (row_id),
  UNIQUE KEY uk_mcp_oauth_grant_identity (identity_digest),
  KEY idx_mcp_oauth_grant_user_updated (tenant_id(64), user_id(64), updated_at_ms DESC),
  CONSTRAINT chk_mcp_oauth_status CHECK (status IN ('active', 'revoked')),
  CONSTRAINT chk_mcp_oauth_access_expiry CHECK (access_expires_at_ms >= 0),
  CONSTRAINT chk_mcp_oauth_refresh_expiry CHECK (refresh_expires_at_ms >= 0),
  CONSTRAINT chk_mcp_oauth_revision CHECK (revision >= 1),
  CONSTRAINT chk_mcp_oauth_updated CHECK (updated_at_ms >= 0)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='MCP server 用户级 OAuth 授权事实表';
