package ruleworker

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const timeLayout = "2006-01-02 15:04:05"

// RuleDoc is one user-authored Markdown input. The immutable typed revision is
// stored in the design-knowledge repository rather than projected back into a
// second set of SQL layout/rule tables.
type RuleDoc struct {
	ID          int64
	FileName    string
	Content     string
	ContentMd5  string
	IsCurrent   bool
	ParseStatus int
}

type RuleDocStatusCount struct {
	ParseStatus int
	Count       int
}

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("ruleworker store: db is required")
	}
	return &Store{db: db}, nil
}

func (s *Store) Check(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT 1 FROM agenui_rule_doc WHERE 1 = 0")
	if err != nil {
		return fmt.Errorf("ruleworker store: check agenui_rule_doc: %w", err)
	}
	return rows.Close()
}

func (s *Store) ListPendingDocs(ctx context.Context) ([]RuleDoc, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, file_name, content, content_md5, is_current, parse_status
FROM agenui_rule_doc
WHERE parse_status IN (0, 3)
ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("ruleworker store: list pending docs: %w", err)
	}
	defer rows.Close()
	var out []RuleDoc
	for rows.Next() {
		var item RuleDoc
		var current int
		if err := rows.Scan(&item.ID, &item.FileName, &item.Content, &item.ContentMd5, &current, &item.ParseStatus); err != nil {
			return nil, fmt.Errorf("ruleworker store: scan doc: %w", err)
		}
		item.IsCurrent = current != 0
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) CountRuleDocsByParseStatus(ctx context.Context) ([]RuleDocStatusCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT parse_status, COUNT(*) FROM agenui_rule_doc GROUP BY parse_status ORDER BY parse_status`)
	if err != nil {
		return nil, fmt.Errorf("ruleworker store: count docs by parse status: %w", err)
	}
	defer rows.Close()
	var out []RuleDocStatusCount
	for rows.Next() {
		var item RuleDocStatusCount
		if err := rows.Scan(&item.ParseStatus, &item.Count); err != nil {
			return nil, fmt.Errorf("ruleworker store: scan doc status count: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ClaimDoc(ctx context.Context, id int64, worker string, lease time.Duration) (bool, error) {
	now := time.Now()
	result, err := s.db.ExecContext(ctx, `
UPDATE agenui_rule_doc
SET retry_count = retry_count + CASE WHEN parse_status = 3 THEN 1 ELSE 0 END,
    parse_status = 1, parse_error = '', parse_worker = ?, parse_started_at = ?,
    parse_finished_at = NULL, lease_expire_at = ?, gmt_modified = ?
WHERE id = ? AND parse_status IN (0, 3)`,
		worker, now.Format(timeLayout), now.Add(lease).Format(timeLayout), now.Format(timeLayout), id)
	if err != nil {
		return false, fmt.Errorf("ruleworker store: claim doc %d: %w", id, err)
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

func (s *Store) MarkParsed(ctx context.Context, id int64, status int, parseError string) error {
	now := time.Now().Format(timeLayout)
	_, err := s.db.ExecContext(ctx, `
UPDATE agenui_rule_doc
SET parse_status=?, parse_error=?, parse_finished_at=?, gmt_modified=?
WHERE id=?`, status, truncate(parseError, 1024), now, now, id)
	if err != nil {
		return fmt.Errorf("ruleworker store: mark doc %d parsed: %w", id, err)
	}
	return nil
}

// PublishDocs atomically records the documents included in the newly published
// immutable revision. This is bookkeeping only; the active pointer remains the
// source of truth for runtime rules.
func (s *Store) PublishDocs(ctx context.Context, docs []RuleDoc) error {
	if len(docs) == 0 {
		return fmt.Errorf("ruleworker store: publish requires at least one document")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ruleworker store: begin publish: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE agenui_rule_doc SET is_effective=0 WHERE is_effective=1`); err != nil {
		return fmt.Errorf("ruleworker store: clear effective document: %w", err)
	}
	now := time.Now().Format(timeLayout)
	for _, doc := range docs {
		if _, err := tx.ExecContext(ctx, `
UPDATE agenui_rule_doc
SET parse_status=2, parse_error='', parse_finished_at=?, gmt_modified=?
WHERE id=?`, now, now, doc.ID); err != nil {
			return fmt.Errorf("ruleworker store: publish doc %d: %w", doc.ID, err)
		}
	}
	effectiveID := docs[0].ID
	effectiveCurrent := docs[0].IsCurrent
	for _, doc := range docs[1:] {
		if (doc.IsCurrent && !effectiveCurrent) || doc.IsCurrent == effectiveCurrent && doc.ID > effectiveID {
			effectiveID = doc.ID
			effectiveCurrent = doc.IsCurrent
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agenui_rule_doc SET is_effective=1 WHERE id=?`, effectiveID); err != nil {
		return fmt.Errorf("ruleworker store: mark effective document: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ruleworker store: commit publish: %w", err)
	}
	return nil
}

func (s *Store) RequeueClaimedDoc(ctx context.Context, id int64, worker string) error {
	now := time.Now().Format(timeLayout)
	_, err := s.db.ExecContext(ctx, `
UPDATE agenui_rule_doc
SET parse_status=0, parse_error='', parse_worker='', parse_started_at=NULL,
    parse_finished_at=NULL, lease_expire_at=NULL, gmt_modified=?
WHERE id=? AND parse_status=1 AND parse_worker=?`, now, id, worker)
	if err != nil {
		return fmt.Errorf("ruleworker store: requeue claimed doc %d: %w", id, err)
	}
	return nil
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}
