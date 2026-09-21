package mysql

import (
	"strconv"
	"strings"
	"testing"
)

func TestReadinessRejectsNumericBusinessIDsFromPhysicalDDL(t *testing.T) {
	cases := []struct {
		table  string
		column string
		have   map[string]columnInfo
	}{
		{
			table:  "sessions",
			column: "id",
			have:   map[string]columnInfo{"id": {dataType: "bigint", columnType: "bigint"}},
		},
		{
			table:  "runs",
			column: "run_id",
			have:   map[string]columnInfo{"run_id": {dataType: "bigint", columnType: "bigint"}},
		},
		{
			table:  "steps",
			column: "run_id",
			have:   map[string]columnInfo{"run_id": {dataType: "bigint", columnType: "bigint"}},
		},
		{
			table:  "agent_events",
			column: "event_id",
			have:   map[string]columnInfo{"event_id": {dataType: "bigint", columnType: "bigint unsigned"}},
		},
		{
			table:  "checkpoint_meta",
			column: "checkpoint_id",
			have:   map[string]columnInfo{"checkpoint_id": {dataType: "bigint", columnType: "bigint"}},
		},
		{
			table:  "control_requests",
			column: "request_id",
			have:   map[string]columnInfo{"request_id": {dataType: "bigint", columnType: "bigint"}},
		},
		{
			table:  "open_turn_idempotency",
			column: "tenant_id",
			have:   map[string]columnInfo{"tenant_id": {dataType: "bigint", columnType: "bigint unsigned"}},
		},
		{
			table:  "idempotency_keys",
			column: "id",
			have:   map[string]columnInfo{"id": {dataType: "bigint", columnType: "bigint unsigned"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.table+"."+tc.column, func(t *testing.T) {
			schema := defaultPhysicalSchema()
			err := schema.observeColumnTypes(tc.table, tc.have)
			if err == nil {
				t.Fatal("numeric business ID column was accepted")
			}
			if !strings.Contains(err.Error(), "expected string type") {
				t.Fatalf("error = %v, want expected string type", err)
			}
		})
	}
}

func TestReadinessAdaptsAgentEventsUsageRename(t *testing.T) {
	schema := defaultPhysicalSchema()
	have := map[string]columnInfo{
		"usage_data": {dataType: "json", columnType: "json"},
	}
	if _, ok := have["usage"]; ok {
		t.Fatal("test setup unexpectedly has canonical usage column")
	}
	schema.eventUsageColumn = "usage_data"
	if err := schema.observeColumnTypes("agent_events", have); err != nil {
		t.Fatalf("usage_data alias was rejected: %v", err)
	}
	store := eventStore{schema: schema}
	if !strings.Contains(store.selectEventCols(), "usage_data") {
		t.Fatalf("select did not use usage_data alias: %s", store.selectEventCols())
	}
}

func TestReadinessAcceptsCanonicalStringAndJSONColumns(t *testing.T) {
	cases := []struct {
		table string
		have  map[string]columnInfo
	}{
		{
			table: "sessions",
			have: map[string]columnInfo{
				"id":        {dataType: "varchar", columnType: "varchar(64)"},
				"tenant_id": {dataType: "varchar", columnType: "varchar(64)"},
				"user_id":   {dataType: "varchar", columnType: "varchar(64)"},
				"metadata":  {dataType: "json", columnType: "json"},
			},
		},
		{
			table: "agent_events",
			have: map[string]columnInfo{
				"event_id":        {dataType: "varchar", columnType: "varchar(64)"},
				"run_id":          {dataType: "varchar", columnType: "varchar(64)"},
				"idempotency_key": {dataType: "varchar", columnType: "varchar(128)"},
				"payload":         {dataType: "json", columnType: "json"},
				"payload_preview": {dataType: "json", columnType: "json"},
				"usage":           {dataType: "json", columnType: "json"},
				"error":           {dataType: "json", columnType: "json"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.table, func(t *testing.T) {
			schema := defaultPhysicalSchema()
			if err := schema.observeColumnTypes(tc.table, tc.have); err != nil {
				t.Fatalf("canonical columns rejected: %v", err)
			}
		})
	}
}

func TestReadinessRejectsIdentifierColumnsNarrowerThanContract(t *testing.T) {
	for table, columns := range requiredIdentifierColumnWidths() {
		for column, minimum := range columns {
			t.Run(table+"."+column, func(t *testing.T) {
				schema := defaultPhysicalSchema()
				err := schema.observeColumnTypes(table, map[string]columnInfo{
					column: {dataType: "varchar", columnType: "varchar(" + strconv.Itoa(minimum-1) + ")"},
				})
				if err == nil || !strings.Contains(err.Error(), "capacity of at least") {
					t.Fatalf("error = %v, want minimum-capacity failure", err)
				}
			})
		}
	}
}

func TestReadinessRejectsTinyTextForBoundedIdentifier(t *testing.T) {
	schema := defaultPhysicalSchema()
	err := schema.observeColumnTypes("idempotency_keys", map[string]columnInfo{
		"id": {dataType: "tinytext", columnType: "tinytext"},
	})
	if err == nil || !strings.Contains(err.Error(), "capacity of at least") {
		t.Fatalf("error = %v, want tinytext capacity failure", err)
	}
}

func TestPhysicalSchemaLeavesBusinessIDsLogical(t *testing.T) {
	schema := defaultPhysicalSchema()
	if got := schema.id("runs", "run_id", "run_abc"); got != "run_abc" {
		t.Fatalf("business ID was mapped: %v", got)
	}
}
