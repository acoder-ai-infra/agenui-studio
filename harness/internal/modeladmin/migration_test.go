package modeladmin

import (
	"strings"
	"testing"
)

func TestModelProviderMySQLSchemaFitsMySQLIndexContract(t *testing.T) {
	for _, fragment := range []string{
		"row_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT",
		"tenant_id VARCHAR(255) NOT NULL COMMENT",
		"provider_id VARCHAR(128) NOT NULL COMMENT",
		"definition_json JSON NOT NULL COMMENT",
		"revision BIGINT NOT NULL COMMENT",
		"identity_digest BINARY(32) NOT NULL COMMENT",
		"UNIQUE KEY uk_model_provider_identity (identity_digest)",
		"tenant_id(64)",
		"provider_id(64)",
		"COMMENT='租户级模型 Provider 定义与版本事实表'",
	} {
		if !strings.Contains(mysqlManagedSchema, fragment) {
			t.Errorf("MySQL model provider schema missing %q", fragment)
		}
	}
	if strings.Contains(strings.ToUpper(mysqlManagedSchema), " BOOLEAN") {
		t.Error("MySQL model provider schema must not use unsupported BOOLEAN type")
	}
}
