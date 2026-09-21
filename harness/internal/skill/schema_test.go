package skill

import (
	"strings"
	"testing"
)

func TestSkillSQLiteAndMySQLSchemasStaySynchronized(t *testing.T) {
	for _, column := range []string{"tenant_id", "skill_id", "version", "definition_json", "content_hash", "content_size", "content", "published_at_ms", "identity_digest"} {
		if !strings.Contains(sqliteSchema, "\n  "+column+" ") {
			t.Errorf("SQLite skill schema missing %s", column)
		}
		if !strings.Contains(mysqlSchema, "\n  "+column+" ") {
			t.Errorf("MySQL skill schema missing %s", column)
		}
	}
	for _, fragment := range []string{"identity_digest BINARY(32) NOT NULL", "UNIQUE KEY uk_skill_version_identity", "tenant_id(64)", "skill_id(64)"} {
		if !strings.Contains(mysqlSchema, fragment) {
			t.Errorf("MySQL skill schema missing %q", fragment)
		}
	}
}

func TestSkillLifecycleSchemasStaySynchronized(t *testing.T) {
	for _, schema := range []string{sqliteLifecycleSchema, mysqlLifecycleSchema} {
		for _, column := range []string{"tenant_id", "skill_id", "version", "retired_at_ms", "identity_digest"} {
			if !strings.Contains(schema, "\n  "+column+" ") {
				t.Errorf("skill lifecycle schema missing %s", column)
			}
		}
	}
	for _, fragment := range []string{
		"row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT",
		"tenant_id VARCHAR(255) NOT NULL COMMENT",
		"retired_at_ms BIGINT NOT NULL COMMENT",
		"identity_digest BINARY(32) NOT NULL COMMENT",
		"COMMENT='Skill 版本退役标记表",
	} {
		if !strings.Contains(mysqlLifecycleSchema, fragment) {
			t.Errorf("MySQL Skill retirement schema missing comment %q", fragment)
		}
	}
}

func TestSkillPackageFileSchemasStaySynchronized(t *testing.T) {
	for _, schema := range []string{sqlitePackageFileSchema, mysqlPackageFileSchema} {
		for _, column := range []string{"tenant_id", "skill_id", "version", "file_path", "mime_type", "content_hash", "content_size", "previewable", "artifact_ref", "identity_digest"} {
			if !strings.Contains(schema, "\n  "+column+" ") {
				t.Errorf("skill package file schema missing %s", column)
			}
		}
	}
	for _, schema := range []string{sqlitePackageFileSchema, mysqlPackageFileSchema} {
		if strings.Contains(schema, "\n  content ") {
			t.Error("skill package file index must not persist file content in the database")
		}
	}
	for _, fragment := range []string{"skill_package_files", "identity_digest", "file_path"} {
		if !strings.Contains(sqlitePackageFileSchema, fragment) {
			t.Errorf("SQLite package file schema missing %q", fragment)
		}
		if !strings.Contains(mysqlPackageFileSchema, fragment) {
			t.Errorf("MySQL package file schema missing %q", fragment)
		}
	}
	for _, fragment := range []string{
		"row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT",
		"file_path VARCHAR(1024) NOT NULL COMMENT",
		"content_hash VARCHAR(80) NOT NULL COMMENT",
		"previewable TINYINT NOT NULL COMMENT",
		"CHECK (previewable IN (0, 1))",
		"artifact_ref VARCHAR(1024) NOT NULL COMMENT",
		"identity_digest BINARY(32) NOT NULL COMMENT",
		"COMMENT='基于 Artifact Store 的不可变 Skill ZIP 文件索引表'",
	} {
		if !strings.Contains(mysqlPackageFileSchema, fragment) {
			t.Errorf("MySQL Skill package file schema missing comment %q", fragment)
		}
	}
	if strings.Contains(mysqlPackageFileSchema, "previewable BOOLEAN") {
		t.Error("MySQL Skill package schema must not use unsupported BOOLEAN type")
	}
}
