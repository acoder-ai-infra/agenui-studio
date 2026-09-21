-- Local SQLite schema for user-authored rule Markdown parse jobs. Published
-- layout, element and atomic-rule documents live in immutable revisions.

CREATE TABLE IF NOT EXISTS agenui_rule_doc (
  id INTEGER PRIMARY KEY AUTOINCREMENT, file_name TEXT NOT NULL DEFAULT '', content TEXT NOT NULL,
  content_md5 TEXT NOT NULL, status INTEGER NOT NULL DEFAULT 2, is_current INTEGER NOT NULL DEFAULT 1,
  is_effective INTEGER NOT NULL DEFAULT 0, parse_status INTEGER NOT NULL DEFAULT 0,
  parser_version TEXT NOT NULL DEFAULT '', parse_error TEXT NOT NULL DEFAULT '',
  parse_report TEXT NOT NULL DEFAULT '', parse_worker TEXT NOT NULL DEFAULT '', retry_count INTEGER NOT NULL DEFAULT 0,
  parse_started_at TEXT, parse_finished_at TEXT, lease_expire_at TEXT,
  change_description TEXT NOT NULL DEFAULT '', create_user TEXT NOT NULL DEFAULT '',
  gmt_create TEXT NOT NULL DEFAULT (datetime('now')), gmt_modified TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_rule_doc_current ON agenui_rule_doc(is_current, id);
CREATE INDEX IF NOT EXISTS idx_rule_doc_parse_task ON agenui_rule_doc(parse_status, lease_expire_at, id);
