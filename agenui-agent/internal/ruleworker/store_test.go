package ruleworker

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// newTestDB opens a fresh sqlite file DB and applies the design-table DDL.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "design.db")
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ddl, err := os.ReadFile(filepath.Join("..", "..", "db", "design", "agenui_design_tables.sqlite.sql"))
	if err != nil {
		t.Fatalf("read ddl: %v", err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("apply ddl: %v", err)
	}
	return db
}

func seedDoc(t *testing.T, db *sql.DB, file, md5 string, isCurrent int) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO agenui_rule_doc
		(file_name, content, content_md5, is_current, parse_status)
		VALUES (?, 'body', ?, ?, 0)`, file, md5, isCurrent)
	if err != nil {
		t.Fatalf("seed doc: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestStoreListAndClaim(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Check(ctx); err != nil {
		t.Fatalf("check: %v", err)
	}

	docID := seedDoc(t, db, "rules.md", "docmd5", 1)
	docs, err := store.ListPendingDocs(ctx)
	if err != nil || len(docs) != 1 || docs[0].ID != docID || !docs[0].IsCurrent {
		t.Fatalf("pending docs = %+v, err %v", docs, err)
	}
	counts, err := store.CountRuleDocsByParseStatus(ctx)
	if err != nil || len(counts) != 1 || counts[0].ParseStatus != 0 || counts[0].Count != 1 {
		t.Fatalf("status counts = %+v, err %v", counts, err)
	}

	// First claim wins, second loses (parse_status already 1).
	won, err := store.ClaimDoc(ctx, docID, "w1", time.Minute)
	if err != nil || !won {
		t.Fatalf("first claim won=%v err=%v", won, err)
	}
	won2, err := store.ClaimDoc(ctx, docID, "w2", time.Minute)
	if err != nil || won2 {
		t.Fatalf("second claim should lose, won=%v err=%v", won2, err)
	}

	// No longer pending.
	docs, _ = store.ListPendingDocs(ctx)
	if len(docs) != 0 {
		t.Fatalf("expected 0 pending after claim, got %d", len(docs))
	}
	counts, err = store.CountRuleDocsByParseStatus(ctx)
	if err != nil || len(counts) != 1 || counts[0].ParseStatus != 1 || counts[0].Count != 1 {
		t.Fatalf("claimed status counts = %+v, err %v", counts, err)
	}

	// A failed document becomes pending again and can be claimed exactly once.
	if err := store.MarkParsed(ctx, docID, 3, "projection failed"); err != nil {
		t.Fatal(err)
	}
	docs, err = store.ListPendingDocs(ctx)
	if err != nil || len(docs) != 1 || docs[0].ID != docID || docs[0].ParseStatus != 3 {
		t.Fatalf("retryable failed docs = %+v, err %v", docs, err)
	}
	won, err = store.ClaimDoc(ctx, docID, "w3", time.Minute)
	if err != nil || !won {
		t.Fatalf("failed-doc claim won=%v err=%v", won, err)
	}
	won2, err = store.ClaimDoc(ctx, docID, "w4", time.Minute)
	if err != nil || won2 {
		t.Fatalf("second failed-doc claim should lose, won=%v err=%v", won2, err)
	}
	var retryCount int
	var parseError string
	var parseFinishedAt sql.NullString
	if err := db.QueryRow(`SELECT retry_count, parse_error, parse_finished_at FROM agenui_rule_doc WHERE id = ?`, docID).
		Scan(&retryCount, &parseError, &parseFinishedAt); err != nil {
		t.Fatal(err)
	}
	if retryCount != 1 || parseError != "" || parseFinishedAt.Valid {
		t.Fatalf("retry state count=%d error=%q finished=%v", retryCount, parseError, parseFinishedAt)
	}
}

func TestPublishDocsMarksOneEffectiveRevisionSource(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store, _ := NewStore(db)
	first := seedDoc(t, db, "first.md", "one", 0)
	second := seedDoc(t, db, "current.md", "two", 1)
	docs, err := store.ListPendingDocs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PublishDocs(ctx, docs); err != nil {
		t.Fatalf("PublishDocs: %v", err)
	}
	for id, wantEffective := range map[int64]int{first: 0, second: 1} {
		var status, effective int
		if err := db.QueryRow(`SELECT parse_status,is_effective FROM agenui_rule_doc WHERE id=?`, id).Scan(&status, &effective); err != nil {
			t.Fatal(err)
		}
		if status != 2 || effective != wantEffective {
			t.Fatalf("doc %d status/effective=%d/%d, want 2/%d", id, status, effective, wantEffective)
		}
	}
}

func TestRequeueClaimedDocRequiresMatchingWorker(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store, _ := NewStore(db)
	docID := seedDoc(t, db, "rules.md", "docmd5", 1)
	won, err := store.ClaimDoc(ctx, docID, "owner", time.Minute)
	if err != nil || !won {
		t.Fatalf("ClaimDoc won=%v err=%v", won, err)
	}
	if err := store.RequeueClaimedDoc(ctx, docID, "other"); err != nil {
		t.Fatal(err)
	}
	var status int
	if err := db.QueryRow(`SELECT parse_status FROM agenui_rule_doc WHERE id = ?`, docID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != 1 {
		t.Fatalf("non-owner changed status to %d", status)
	}
	if err := store.RequeueClaimedDoc(ctx, docID, "owner"); err != nil {
		t.Fatal(err)
	}
	var worker string
	var started, lease sql.NullString
	if err := db.QueryRow(`SELECT parse_status, parse_worker, parse_started_at, lease_expire_at FROM agenui_rule_doc WHERE id = ?`, docID).
		Scan(&status, &worker, &started, &lease); err != nil {
		t.Fatal(err)
	}
	if status != 0 || worker != "" || started.Valid || lease.Valid {
		t.Fatalf("requeued status=%d worker=%q started=%v lease=%v", status, worker, started, lease)
	}
}

func TestMarkParsedFailure(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	store, _ := NewStore(db)
	docID := seedDoc(t, db, "rules.md", "md5", 0)
	if err := store.MarkParsed(ctx, docID, 3, "boom"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	var status int
	var perr string
	db.QueryRow(`SELECT parse_status, parse_error FROM agenui_rule_doc WHERE id=?`, docID).Scan(&status, &perr)
	if status != 3 || perr != "boom" {
		t.Fatalf("status=%d err=%q, want 3/boom", status, perr)
	}
}
