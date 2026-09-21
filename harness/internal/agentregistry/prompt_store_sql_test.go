package agentregistry

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSQLPromptStoreCreatesAndReadsInlinePrompt(t *testing.T) {
	prompt := sqlTestPrompt()
	db, state := openScriptDB(t,
		dbStep{kind: "exec", contains: "prompt_identity_digest", affected: 1},
		dbStep{
			kind: "query", contains: "WHERE prompt_ref=? AND version=?",
			columns: strings.Split(sqlPromptColumns, ", "), rows: [][]driver.Value{sqlPromptRow(prompt)},
		},
	)
	store, err := NewSQLPromptStore(db)
	if err != nil {
		t.Fatalf("new sql prompt store: %v", err)
	}
	if err := store.Create(context.Background(), prompt); err != nil {
		t.Fatalf("create prompt: %v", err)
	}
	got, err := store.Get(context.Background(), PromptKey{Ref: prompt.Ref, Version: prompt.Version})
	if err != nil {
		t.Fatalf("get prompt: %v", err)
	}
	if got.Content != prompt.Content || got.ContentHash != prompt.ContentHash || got.CreatedBy != prompt.CreatedBy {
		t.Fatalf("stored prompt drift: got=%#v want=%#v", got, prompt)
	}
	state.assertDone(t)
}

func TestSQLPromptStoreDuplicateIsIdempotentOnlyForSameContent(t *testing.T) {
	prompt := sqlTestPrompt()
	tests := []struct {
		name     string
		existing PromptVersion
		wantErr  bool
	}{
		{name: "same", existing: prompt},
		{name: "changed", existing: func() PromptVersion {
			changed := prompt
			changed.Content = "changed"
			changed.ContentHash = PromptContentHash(changed.Content)
			return changed
		}(), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, state := openScriptDB(t,
				dbStep{kind: "exec", contains: "INSERT INTO agent_registry_prompt_versions", err: errors.New("duplicate key")},
				dbStep{
					kind: "query", contains: "WHERE prompt_ref=? AND version=?",
					columns: strings.Split(sqlPromptColumns, ", "), rows: [][]driver.Value{sqlPromptRow(test.existing)},
				},
			)
			store, _ := NewSQLPromptStore(db)
			err := store.Create(context.Background(), prompt)
			if test.wantErr {
				if !errors.Is(err, ErrPromptVersionExists) || !errors.Is(err, ErrStoreConflict) {
					t.Fatalf("immutable conflict error = %v", err)
				}
			} else if err != nil {
				t.Fatalf("idempotent duplicate: %v", err)
			}
			state.assertDone(t)
		})
	}
}

func TestSQLPromptStoreRejectsCorruptBodyAndVariables(t *testing.T) {
	prompt := sqlTestPrompt()
	tests := []struct {
		name string
		row  []driver.Value
	}{
		{name: "body hash", row: func() []driver.Value {
			row := sqlPromptRow(prompt)
			row[2] = "tampered"
			return row
		}()},
		{name: "variables json", row: func() []driver.Value {
			row := sqlPromptRow(prompt)
			row[5] = `{"not":"array"}`
			return row
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, state := openScriptDB(t, dbStep{
				kind: "query", contains: "WHERE prompt_ref=? AND version=?",
				columns: strings.Split(sqlPromptColumns, ", "), rows: [][]driver.Value{test.row},
			})
			store, _ := NewSQLPromptStore(db)
			_, err := store.Get(context.Background(), PromptKey{Ref: prompt.Ref, Version: prompt.Version})
			if !errors.Is(err, ErrStoreCorrupt) {
				t.Fatalf("corrupt row error = %v", err)
			}
			state.assertDone(t)
		})
	}
}

func TestSQLPromptStoreReturnsNotFound(t *testing.T) {
	db, state := openScriptDB(t, dbStep{
		kind: "query", contains: "WHERE prompt_ref=? AND version=?",
		columns: strings.Split(sqlPromptColumns, ", "),
	})
	store, _ := NewSQLPromptStore(db)
	_, err := store.Get(context.Background(), PromptKey{Ref: "prompt://missing", Version: "v1"})
	if !errors.Is(err, ErrPromptNotFound) {
		t.Fatalf("get missing error = %v", err)
	}
	state.assertDone(t)
}

func TestSQLPromptStoreListsVersionsNewestFirst(t *testing.T) {
	newest := sqlTestPrompt()
	older := newest
	older.Version = "v3"
	older.CreatedAt = newest.CreatedAt.Add(-time.Hour)
	db, state := openScriptDB(t, dbStep{
		kind: "query", contains: "WHERE prompt_ref=? ORDER BY created_at_ms DESC",
		columns: strings.Split(sqlPromptColumns, ", "), rows: [][]driver.Value{sqlPromptRow(newest), sqlPromptRow(older)},
	})
	store, _ := NewSQLPromptStore(db)
	versions, err := store.ListVersions(context.Background(), newest.Ref)
	if err != nil {
		t.Fatalf("list prompt versions: %v", err)
	}
	if len(versions) != 2 || versions[0].Version != newest.Version || versions[1].Version != older.Version {
		t.Fatalf("versions are not newest first: %#v", versions)
	}
	state.assertDone(t)
}

func TestSQLPromptStoreHonorsCanceledContextBeforeDatabaseAccess(t *testing.T) {
	db, state := openScriptDB(t)
	store, _ := NewSQLPromptStore(db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Create(ctx, sqlTestPrompt()); !errors.Is(err, context.Canceled) {
		t.Fatalf("create canceled error = %v", err)
	}
	if _, err := store.Get(ctx, PromptKey{Ref: "prompt://x", Version: "v1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("get canceled error = %v", err)
	}
	state.assertDone(t)
}

func TestSQLPromptStoreSupportsSharedQuestionMarkDialect(t *testing.T) {
	db, _ := openScriptDB(t)
	store, err := NewSQLPromptStore(db, WithSQLPromptTable("tenant_prompt_versions"))
	if err != nil {
		t.Fatalf("new shared-dialect store: %v", err)
	}
	if query := store.insertQuery(); !strings.Contains(query, "VALUES (?,?,?,?,?,?,?,?,?)") {
		t.Fatalf("MySQL/SQLite placeholders missing: %s", query)
	}
	if query := store.selectQuery(); !strings.Contains(query, "prompt_ref=? AND version=?") {
		t.Fatalf("question-mark lookup placeholders missing: %s", query)
	}

	postgres, err := NewSQLPromptStore(db, WithSQLPromptDialect(SQLDialectPostgres))
	if err != nil {
		t.Fatalf("new postgres prompt store: %v", err)
	}
	if query := postgres.insertQuery(); !strings.Contains(query, "VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)") {
		t.Fatalf("postgres placeholders missing: %s", query)
	}
	if _, err := NewSQLPromptStore(db, WithSQLPromptTable("prompts; DROP TABLE users")); err == nil {
		t.Fatal("unsafe prompt table name must be rejected")
	}
}

func TestSQLPromptStoreEnsureSchemaUsesConfiguredTable(t *testing.T) {
	db, state := openScriptDB(t, dbStep{
		kind: "exec", contains: "CREATE TABLE IF NOT EXISTS tenant_prompt_versions", affected: 0,
	})
	store, err := NewSQLPromptStore(db, WithSQLPromptTable("tenant_prompt_versions"))
	if err != nil {
		t.Fatalf("new prompt store: %v", err)
	}
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	state.assertDone(t)
}

func sqlTestPrompt() PromptVersion {
	content := "你是旅行规划助手。"
	return PromptVersion{
		Ref: "prompt://travel/system", Version: "v4", Content: content,
		ContentHash: PromptContentHash(content), Variables: []string{"current_date", "user_profile"},
		CreatedAt: time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC), CreatedBy: "publisher",
	}
}

func sqlPromptRow(prompt PromptVersion) []driver.Value {
	var content, contentRef driver.Value
	if prompt.Content != "" {
		content = prompt.Content
	} else {
		contentRef = prompt.ContentRef
	}
	return []driver.Value{
		prompt.Ref,
		prompt.Version,
		content,
		contentRef,
		prompt.ContentHash,
		`["current_date","user_profile"]`,
		prompt.CreatedAt.UnixMilli(),
		prompt.CreatedBy,
	}
}
