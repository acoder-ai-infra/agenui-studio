CREATE TABLE IF NOT EXISTS model_providers (
  row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增物理主键，仅用于 InnoDB 聚簇索引',
  tenant_id VARCHAR(255) NOT NULL COMMENT '模型 Provider 所属租户标识',
  provider_id VARCHAR(128) NOT NULL COMMENT '租户内唯一的模型 Provider 业务标识',
  definition_json JSON NOT NULL COMMENT '模型 Provider 完整定义，包含版本、协议、Endpoint、模型列表、能力、配额和凭证配置',
  revision BIGINT NOT NULL COMMENT '乐观锁版本号，创建为 1，每次更新递增',
  created_at_ms BIGINT NOT NULL COMMENT '创建时间，Unix 毫秒',
  updated_at_ms BIGINT NOT NULL COMMENT '最近更新时间，Unix 毫秒',
  updated_by VARCHAR(255) NOT NULL COMMENT '最近一次更新的操作人标识',
  identity_digest BINARY(32) NOT NULL COMMENT '应用计算的 tenant_id、provider_id 唯一身份摘要',
  PRIMARY KEY (row_id),
  UNIQUE KEY uk_model_provider_identity (identity_digest),
  KEY idx_model_provider_tenant_updated (tenant_id(64), updated_at_ms DESC, provider_id(64)),
  CONSTRAINT chk_model_provider_revision CHECK (revision >= 1),
  CONSTRAINT chk_model_provider_created_at CHECK (created_at_ms >= 0),
  CONSTRAINT chk_model_provider_updated_at CHECK (updated_at_ms >= created_at_ms)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='租户级模型 Provider 定义与版本事实表';
