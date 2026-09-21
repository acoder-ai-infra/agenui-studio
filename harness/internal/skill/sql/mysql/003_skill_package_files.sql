CREATE TABLE IF NOT EXISTS skill_package_files (
  row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增物理主键，仅用于 InnoDB 聚簇索引',
  tenant_id VARCHAR(255) NOT NULL COMMENT 'Skill 所属租户标识；全局 Skill 为空字符串',
  skill_id VARCHAR(128) NOT NULL COMMENT 'Skill 业务标识',
  version VARCHAR(64) NOT NULL COMMENT '不可变 Skill 语义版本号',
  file_path VARCHAR(1024) NOT NULL COMMENT '文件在规范化 Skill ZIP 内的相对路径',
  mime_type VARCHAR(128) NOT NULL COMMENT '文件 MIME 类型，用于管理页展示与预览',
  content_hash VARCHAR(80) NOT NULL COMMENT '文件内容 SHA-256 摘要，用于完整性校验',
  content_size BIGINT NOT NULL COMMENT '文件解压后大小，单位字节',
  previewable TINYINT NOT NULL COMMENT '是否为可在管理页只读预览的 UTF-8 文本文件：0 否，1 是',
  artifact_ref VARCHAR(1024) NOT NULL COMMENT '完整 Skill ZIP 在 Artifact Store 中的不可变引用',
  identity_digest BINARY(32) NOT NULL COMMENT '应用计算的 tenant_id、skill_id、version 唯一身份摘要',
  PRIMARY KEY (row_id),
  UNIQUE KEY uk_skill_package_file (identity_digest, file_path(512)),
  KEY idx_skill_package_file_version (tenant_id(64), skill_id(64), version, file_path(128)),
  CONSTRAINT chk_skill_package_file_size CHECK (content_size >= 0 AND content_size <= 26214400),
  CONSTRAINT chk_skill_package_file_previewable CHECK (previewable IN (0, 1))
) ENGINE=InnoDB ROW_FORMAT=DYNAMIC DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='基于 Artifact Store 的不可变 Skill ZIP 文件索引表';
