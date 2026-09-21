package agentregistry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const defaultSQLPromptTable = "agent_registry_prompt_versions"

const sqlPromptColumns = "prompt_ref, version, content, content_ref, content_hash, variables_json, created_at_ms, created_by"

type SQLPromptStoreOption func(*SQLPromptStore) error

// SQLPromptStore 复用应用注入的 database/sql 连接池，不在包内绑定 MySQL 或 SQLite driver。
type SQLPromptStore struct {
	db                 *sql.DB
	table              string
	dialect            SQLDialect
	conflictClassifier func(error) bool
}

func NewSQLPromptStore(db *sql.DB, opts ...SQLPromptStoreOption) (*SQLPromptStore, error) {
	if db == nil {
		return nil, errors.New("agentregistry: prompt sql db is required")
	}
	store := &SQLPromptStore{
		db:                 db,
		table:              defaultSQLPromptTable,
		dialect:            SQLDialectQuestionMark,
		conflictClassifier: defaultSQLConflictClassifier,
	}
	for _, opt := range opts {
		if err := opt(store); err != nil {
			return nil, err
		}
	}
	if !validSQLIdentifier(store.table) {
		return nil, fmt.Errorf("agentregistry: invalid prompt sql table %q", store.table)
	}
	return store, nil
}

func WithSQLPromptTable(table string) SQLPromptStoreOption {
	return func(store *SQLPromptStore) error {
		if !validSQLIdentifier(table) {
			return fmt.Errorf("agentregistry: invalid prompt sql table %q", table)
		}
		store.table = table
		return nil
	}
}

func WithSQLPromptDialect(dialect SQLDialect) SQLPromptStoreOption {
	return func(store *SQLPromptStore) error {
		switch dialect {
		case SQLDialectQuestionMark, SQLDialectPostgres:
			store.dialect = dialect
			return nil
		default:
			return fmt.Errorf("agentregistry: unsupported prompt sql dialect %q", dialect)
		}
	}
}

func WithSQLPromptConflictClassifier(classifier func(error) bool) SQLPromptStoreOption {
	return func(store *SQLPromptStore) error {
		if classifier == nil {
			return errors.New("agentregistry: prompt sql conflict classifier is required")
		}
		store.conflictClassifier = classifier
		return nil
	}
}

// EnsureSchema 只服务本地开发和单机 demo；线上环境必须执行编号 migration。
func (s *SQLPromptStore) EnsureSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, sqlPromptSchemaStatementWithDigestType(s.table, sqlIdentityDigestType(s.dialect))); err != nil {
		return fmt.Errorf("agentregistry: ensure prompt sql schema: %w", err)
	}
	return nil
}

func (s *SQLPromptStore) Create(ctx context.Context, prompt PromptVersion) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	prepared, err := preparePromptVersion(prompt, time.Now())
	if err != nil {
		return err
	}
	variables, err := json.Marshal(prepared.Variables)
	if err != nil {
		return fmt.Errorf("agentregistry: encode prompt variables: %w", err)
	}
	var content, contentRef any
	if prepared.Content != "" {
		content = prepared.Content
	} else {
		contentRef = prepared.ContentRef
	}
	_, err = s.db.ExecContext(ctx, s.insertQuery(),
		prepared.Ref,
		prepared.Version,
		content,
		contentRef,
		prepared.ContentHash,
		string(variables),
		prepared.CreatedAt.UnixMilli(),
		prepared.CreatedBy,
		identityTupleDigest(prepared.Ref, prepared.Version),
	)
	if err == nil {
		return nil
	}
	if !s.conflictClassifier(err) {
		return fmt.Errorf("agentregistry: create prompt version: %w", err)
	}

	// INSERT-only 保证版本不可变；重复发布只有内容完全一致时才视为幂等成功。
	key := PromptKey{Ref: prepared.Ref, Version: prepared.Version}
	existing, getErr := s.Get(ctx, key)
	if getErr == nil && samePromptVersion(existing, prepared) {
		return nil
	}
	if getErr != nil {
		return fmt.Errorf("%w: inspect existing %s@%s: %w", ErrPromptVersionExists, key.Ref, key.Version, getErr)
	}
	return promptVersionConflict(key)
}

func (s *SQLPromptStore) Get(ctx context.Context, input PromptKey) (PromptVersion, error) {
	if err := ctx.Err(); err != nil {
		return PromptVersion{}, err
	}
	key, err := normalizePromptKey(input)
	if err != nil {
		return PromptVersion{}, err
	}
	var (
		prompt                         PromptVersion
		content, contentRef, createdBy sql.NullString
		variablesJSON                  string
		createdAtMS                    int64
	)
	err = s.db.QueryRowContext(ctx, s.selectQuery(), key.Ref, key.Version).Scan(
		&prompt.Ref,
		&prompt.Version,
		&content,
		&contentRef,
		&prompt.ContentHash,
		&variablesJSON,
		&createdAtMS,
		&createdBy,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return PromptVersion{}, ErrPromptNotFound
	}
	if err != nil {
		return PromptVersion{}, fmt.Errorf("agentregistry: get prompt version: %w", err)
	}
	if createdAtMS < 0 {
		return PromptVersion{}, fmt.Errorf("%w: negative prompt created_at_ms", ErrStoreCorrupt)
	}
	prompt.Content = content.String
	prompt.ContentRef = contentRef.String
	prompt.CreatedAt = time.UnixMilli(createdAtMS).UTC()
	prompt.CreatedBy = createdBy.String
	if err := json.Unmarshal([]byte(variablesJSON), &prompt.Variables); err != nil {
		return PromptVersion{}, fmt.Errorf("%w: decode prompt variables: %v", ErrStoreCorrupt, err)
	}
	if prompt.Variables == nil {
		prompt.Variables = []string{}
	}
	if prompt.Ref != key.Ref || prompt.Version != key.Version {
		return PromptVersion{}, fmt.Errorf("%w: prompt identity projection mismatch", ErrStoreCorrupt)
	}
	if err := validateStoredPromptVersion(prompt); err != nil {
		return PromptVersion{}, err
	}
	return clonePromptVersion(prompt), nil
}

func (s *SQLPromptStore) ListVersions(ctx context.Context, inputRef string) ([]PromptVersion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ref, err := normalizePromptRef(inputRef)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, s.listQuery(), ref)
	if err != nil {
		return nil, fmt.Errorf("agentregistry: list prompt versions: %w", err)
	}
	defer rows.Close()
	versions := make([]PromptVersion, 0)
	for rows.Next() {
		var (
			prompt                         PromptVersion
			content, contentRef, createdBy sql.NullString
			variablesJSON                  string
			createdAtMS                    int64
		)
		if err := rows.Scan(&prompt.Ref, &prompt.Version, &content, &contentRef, &prompt.ContentHash, &variablesJSON, &createdAtMS, &createdBy); err != nil {
			return nil, fmt.Errorf("agentregistry: scan prompt version: %w", err)
		}
		if createdAtMS < 0 {
			return nil, fmt.Errorf("%w: negative prompt created_at_ms", ErrStoreCorrupt)
		}
		prompt.Content = content.String
		prompt.ContentRef = contentRef.String
		prompt.CreatedAt = time.UnixMilli(createdAtMS).UTC()
		prompt.CreatedBy = createdBy.String
		if err := json.Unmarshal([]byte(variablesJSON), &prompt.Variables); err != nil {
			return nil, fmt.Errorf("%w: decode prompt variables: %v", ErrStoreCorrupt, err)
		}
		if prompt.Variables == nil {
			prompt.Variables = []string{}
		}
		if prompt.Ref != ref {
			return nil, fmt.Errorf("%w: prompt identity projection mismatch", ErrStoreCorrupt)
		}
		if err := validateStoredPromptVersion(prompt); err != nil {
			return nil, err
		}
		versions = append(versions, clonePromptVersion(prompt))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentregistry: iterate prompt versions: %w", err)
	}
	return versions, nil
}

func (s *SQLPromptStore) insertQuery() string {
	return "INSERT INTO " + s.table + " (" + sqlPromptColumns + ", prompt_identity_digest) VALUES (" + s.placeholders(9) + ")"
}

func (s *SQLPromptStore) selectQuery() string {
	return "SELECT " + sqlPromptColumns + " FROM " + s.table + " WHERE prompt_ref=" + s.placeholder(1) + " AND version=" + s.placeholder(2)
}

func (s *SQLPromptStore) listQuery() string {
	return "SELECT " + sqlPromptColumns + " FROM " + s.table + " WHERE prompt_ref=" + s.placeholder(1) + " ORDER BY created_at_ms DESC, version DESC"
}

func (s *SQLPromptStore) placeholder(index int) string {
	if s.dialect == SQLDialectPostgres {
		return "$" + strconv.Itoa(index)
	}
	// MySQL 和 SQLite 的 database/sql driver 都使用问号占位符。
	return "?"
}

func (s *SQLPromptStore) placeholders(count int) string {
	values := make([]string, count)
	for i := range values {
		values[i] = s.placeholder(i + 1)
	}
	return strings.Join(values, ",")
}

var _ PromptStore = (*SQLPromptStore)(nil)
