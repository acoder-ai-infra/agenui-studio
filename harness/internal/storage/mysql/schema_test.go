package mysql

import (
	"strings"
	"testing"
)

func TestSchemaCarriesDurableResumeClaim(t *testing.T) {
	for _, fragment := range []string{
		"resume_attempt_id",
		"parent_span_id", "agent_type", "runtime",
	} {
		if !strings.Contains(Schema, fragment) {
			t.Fatalf("schema missing %s", fragment)
		}
	}
	for _, obsolete := range []string{"resume_lease_until", "idx_runs_resume_lease"} {
		if strings.Contains(Schema, obsolete) {
			t.Fatalf("schema still contains obsolete %s", obsolete)
		}
	}
}

func TestModelUsageSchemaFollowsDBAConventions(t *testing.T) {
	block := schemaBlock(t, "CREATE TABLE IF NOT EXISTS model_usage_records", ") ENGINE=InnoDB")
	for _, forbidden := range []string{"BOOLEAN", "DOUBLE"} {
		if strings.Contains(strings.ToUpper(block), forbidden) {
			t.Fatalf("model_usage_records schema contains forbidden type %s:\n%s", forbidden, block)
		}
	}
	for _, required := range []string{
		"usage_id          VARCHAR(64)  NOT NULL COMMENT",
		"fallback_applied  TINYINT(1)   NOT NULL DEFAULT 0 COMMENT",
		"estimated_cost    DECIMAL(18,8) NOT NULL DEFAULT 0 COMMENT",
		"first_token_observed TINYINT(1) NOT NULL DEFAULT 0 COMMENT",
		"output_tokens_per_second DECIMAL(18,6) NOT NULL DEFAULT 0 COMMENT",
		"cache_hit         TINYINT(1)   NOT NULL DEFAULT 0 COMMENT",
		"PRIMARY KEY (usage_id)",
	} {
		if !strings.Contains(block, required) {
			t.Fatalf("model_usage_records schema missing %q:\n%s", required, block)
		}
	}
	if !strings.Contains(Schema, "COMMENT='模型调用用量记录表'") {
		t.Fatal("model_usage_records table comment is missing")
	}
}

func TestAllMySQLTablesCarryCommentsAndLogicalStringPrimaryKeys(t *testing.T) {
	for _, table := range []struct {
		name        string
		primaryDecl string
	}{
		{name: "sessions", primaryDecl: "id           VARCHAR(64)  NOT NULL COMMENT"},
		{name: "messages", primaryDecl: "id              VARCHAR(64)  NOT NULL COMMENT"},
		{name: "model_usage_records", primaryDecl: "usage_id          VARCHAR(64)  NOT NULL COMMENT"},
		{name: "runs", primaryDecl: "run_id               VARCHAR(64)  NOT NULL COMMENT"},
		{name: "steps", primaryDecl: "step_id        VARCHAR(64)  NOT NULL COMMENT"},
		{name: "agent_events", primaryDecl: "event_id        VARCHAR(64)  NOT NULL COMMENT"},
		{name: "checkpoint_meta", primaryDecl: "checkpoint_id  VARCHAR(64)  NOT NULL COMMENT"},
		{name: "control_requests", primaryDecl: "request_id        VARCHAR(64)  NOT NULL COMMENT"},
		{name: "idempotency_keys", primaryDecl: "id         VARCHAR(320) NOT NULL COMMENT"},
		{name: "open_turn_idempotency", primaryDecl: "tenant_id VARCHAR(64) NOT NULL COMMENT"},
	} {
		table := table
		t.Run(table.name, func(t *testing.T) {
			block := schemaBlock(t, "CREATE TABLE IF NOT EXISTS "+table.name, ") ENGINE=InnoDB")
			if !strings.Contains(block, table.primaryDecl) {
				t.Fatalf("%s primary key declaration is not canonical logical ID storage:\n%s", table.name, block)
			}
			if !strings.Contains(block, "COMMENT") {
				t.Fatalf("%s block has no column comments:\n%s", table.name, block)
			}
			suffixStart := strings.Index(Schema, block)
			suffix := Schema[suffixStart:]
			next := strings.Index(suffix, ";")
			if next < 0 || !strings.Contains(suffix[:next], "COMMENT='") {
				t.Fatalf("%s table comment is missing", table.name)
			}
		})
	}
}

func TestAgentEventsSchemaUsesUsageDataPhysicalColumn(t *testing.T) {
	block := schemaBlock(t, "CREATE TABLE IF NOT EXISTS agent_events", ") ENGINE=InnoDB")
	if strings.Contains(block, "usage           JSON") {
		t.Fatal("agent_events still uses reserved physical column usage")
	}
	if !strings.Contains(block, "usage_data      JSON") {
		t.Fatal("agent_events missing usage_data physical column")
	}
}

func schemaBlock(t *testing.T, start, end string) string {
	t.Helper()
	from := strings.Index(Schema, start)
	if from < 0 {
		t.Fatalf("schema block start %q not found", start)
	}
	to := strings.Index(Schema[from:], end)
	if to < 0 {
		t.Fatalf("schema block end %q not found", end)
	}
	return Schema[from : from+to]
}
