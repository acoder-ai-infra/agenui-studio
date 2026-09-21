-- 租户自定义 HTTP Tool 的 MySQL 8 建表脚本。
-- definition_json 保存完整工具定义和模型可见 schema；认证只保存 header 到环境变量名的映射，
-- 不保存任何明文 secret、token 或密码。
-- 长租户身份使用完整字段保存，应用侧 SHA-256 摘要保证唯一，查询索引采用有界前缀，
-- 兼容公司 767-byte 二级索引预检且不牺牲业务身份的精确唯一性。
-- identity_digest 必须由应用使用长度前缀 tuple 编码后写入；禁止依赖 trigger 或生成列，
-- 使用摘要索引避免可注入分隔符导致不同业务身份串位。

CREATE TABLE IF NOT EXISTS http_tools (
  row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增行主键，仅用于 InnoDB 聚簇索引',
  tenant_id VARCHAR(255) NOT NULL COMMENT 'HTTP Tool 定义所属租户标识',
  tool_name VARCHAR(128) NOT NULL COMMENT '租户内唯一的 HTTP Tool 名称，需满足应用侧安全命名规则',
  definition_json JSON NOT NULL COMMENT 'HTTP Tool 完整定义 JSON，包含方法、URL、输入输出 schema、响应模式、风险等级和 header_env 映射',
  revision BIGINT NOT NULL COMMENT '乐观锁版本号，创建为 1，每次更新递增',
  created_at_ms BIGINT NOT NULL COMMENT '定义创建时间，Unix 毫秒',
  updated_at_ms BIGINT NOT NULL COMMENT '最近更新时间，Unix 毫秒',
  updated_by VARCHAR(255) NOT NULL COMMENT '最近更新主体标识，用于审计展示',
  identity_digest BINARY(32) NOT NULL COMMENT '应用计算的 tenant_id 与 tool_name 唯一身份摘要',
  PRIMARY KEY (row_id),
  UNIQUE KEY uk_http_tool_identity (identity_digest),
  KEY idx_http_tool_tenant_updated (tenant_id(64), updated_at_ms DESC, tool_name(64)),
  CONSTRAINT chk_http_tool_revision CHECK (revision >= 1),
  CONSTRAINT chk_http_tool_created_at CHECK (created_at_ms >= 0),
  CONSTRAINT chk_http_tool_updated_at CHECK (updated_at_ms >= created_at_ms)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='租户自定义 HTTP Tool 定义事实表';
