CREATE TABLE IF NOT EXISTS skill_retirements (
  row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增物理主键，仅用于 InnoDB 聚簇索引',
  tenant_id VARCHAR(255) NOT NULL COMMENT 'Skill 所属租户标识；全局 Skill 为空字符串',
  skill_id VARCHAR(128) NOT NULL COMMENT 'Skill 业务标识',
  version VARCHAR(64) NOT NULL COMMENT '被退役的不可变 Skill 语义版本号',
  retired_at_ms BIGINT NOT NULL COMMENT '退役时间，Unix 毫秒',
  identity_digest BINARY(32) NOT NULL COMMENT '应用计算的 tenant_id、skill_id、version 唯一身份摘要',
  PRIMARY KEY (row_id),
  UNIQUE KEY uk_skill_retirement_identity (identity_digest),
  KEY idx_skill_retirement_tenant_id (tenant_id(64), skill_id(64), retired_at_ms DESC),
  CONSTRAINT chk_skill_retirement_retired_at CHECK (retired_at_ms >= 0)
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='Skill 版本退役标记表；隐藏发现但保留精确版本解析';
