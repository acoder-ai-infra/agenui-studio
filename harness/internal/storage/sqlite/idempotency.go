package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// idempotencyStore 是 SQLite IdempotencyStore。Consume 通过 INSERT 尝试占位;
// 唯一冲突时检查是否已过期(过期则重新占用)。语义对齐 memory 后端。
type idempotencyStore struct {
	db *sql.DB
}

// Consume 返回 ok=true 表示首次消费;ok=false 表示重放(已消费且未过期)。
// expires_at=0 表示永不过期(对齐 memory 的零值语义)。
func (s *idempotencyStore) Consume(ctx context.Context, k storage.IdemKey) (bool, error) {
	if k.Key == "" || k.Namespace == "" {
		return false, storage.NewError(storage.ErrInvalidArgument, "idempotency namespace and key required")
	}
	if err := storage.ValidateIdemKeyIdentifiers(k); err != nil {
		return false, err
	}
	var exp time.Duration
	if k.TTL > 0 {
		exp = k.TTL
	}
	var expiresAt int64
	if exp > 0 {
		expiresAt = tsVal(time.Now().Add(exp))
	}
	// 尝试 INSERT
	_, err := s.db.ExecContext(ctx, `INSERT INTO idempotency_keys (tenant_id, namespace, idem_key, expires_at) VALUES (?,?,?,?)`,
		k.TenantID, k.Namespace, k.Key, expiresAt)
	if err == nil {
		return true, nil // 首次消费成功
	}
	if !isUniqueViolation(err) {
		return false, err
	}
	// 已存在:检查是否过期
	var existingExp int64
	if err := s.db.QueryRowContext(ctx, "SELECT expires_at FROM idempotency_keys WHERE tenant_id=? AND namespace=? AND idem_key=?",
		k.TenantID, k.Namespace, k.Key).Scan(&existingExp); err != nil {
		if err == sql.ErrNoRows {
			return true, nil // 并发场景:被删除了,重试 INSERT
		}
		return false, err
	}
	// expires_at=0 表示永不过期;existingExp > now 表示未过期
	if existingExp == 0 || existingExp > time.Now().UnixNano() {
		return false, nil // 未过期,重放
	}
	// 已过期:重新占用
	if _, err := s.db.ExecContext(ctx, "UPDATE idempotency_keys SET expires_at=? WHERE tenant_id=? AND namespace=? AND idem_key=?",
		expiresAt, k.TenantID, k.Namespace, k.Key); err != nil {
		return false, err
	}
	return true, nil
}

var _ storage.IdempotencyStore = (*idempotencyStore)(nil)
