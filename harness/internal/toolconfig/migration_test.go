package toolconfig

import (
	"strings"
	"testing"
)

func TestToolConfigMySQLSchemaFitsMySQLIndexContract(t *testing.T) {
	for _, fragment := range []string{
		"row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT",
		"tenant_id VARCHAR(255) NOT NULL COMMENT",
		"tool_name VARCHAR(128) NOT NULL COMMENT",
		"definition_json JSON NOT NULL COMMENT",
		"revision BIGINT NOT NULL COMMENT",
		"identity_digest BINARY(32) NOT NULL COMMENT",
		"UNIQUE KEY uk_tool_config_identity (identity_digest)",
		"tenant_id(64)",
		"tool_name(64)",
		"COMMENT='租户级工具运行配置与版本事实表'",
	} {
		if !strings.Contains(mysqlManagedSchema, fragment) {
			t.Errorf("MySQL tool config schema missing %q", fragment)
		}
	}
	if strings.Contains(strings.ToUpper(mysqlManagedSchema), " BOOLEAN") {
		t.Error("MySQL tool config schema must not use unsupported BOOLEAN type")
	}
}
