CREATE TABLE IF NOT EXISTS tool_configs (
  row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增物理主键，仅用于 InnoDB 聚簇索引',
  tenant_id VARCHAR(255) NOT NULL COMMENT '工具配置所属租户标识',
  tool_name VARCHAR(128) NOT NULL COMMENT '租户内唯一的工具名称',
  definition_json JSON NOT NULL COMMENT '工具的租户级运行配置 JSON，实际字段受工具 config schema 约束',
  revision BIGINT NOT NULL COMMENT '乐观锁版本号，创建为 1，每次更新递增',
  created_at_ms BIGINT NOT NULL COMMENT '创建时间，Unix 毫秒',
  updated_at_ms BIGINT NOT NULL COMMENT '最近更新时间，Unix 毫秒',
  updated_by VARCHAR(255) NOT NULL COMMENT '最近一次更新的操作人标识',
  identity_digest BINARY(32) NOT NULL COMMENT '应用计算的 tenant_id、tool_name 唯一身份摘要',
  PRIMARY KEY (row_id),
  UNIQUE KEY uk_tool_config_identity (identity_digest),
  KEY idx_tool_config_tenant_updated (tenant_id(64), updated_at_ms DESC, tool_name(64)),
  CONSTRAINT chk_tool_config_revision CHECK (revision >= 1),
  CONSTRAINT chk_tool_config_created_at CHECK (created_at_ms >= 0),
  CONSTRAINT chk_tool_config_updated_at CHECK (updated_at_ms >= created_at_ms)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='租户级工具运行配置与版本事实表';
