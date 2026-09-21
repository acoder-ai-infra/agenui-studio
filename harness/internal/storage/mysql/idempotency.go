package mysql

import (
	"context"
	"database/sql"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// idempotencyStore is the MySQL IdempotencyStore. The table uses a single
// composite primary key column `id = tenant|namespace|key`; expires_at is
// DATETIME(3) NULL where NULL means never-expires (mirrors the zero-value
// semantics of the other backends). Consume claims via INSERT; on a duplicate it
// checks expiry and re-claims if aged out.
type idempotencyStore struct {
	db     *sql.DB
	schema physicalSchema
}

func (s *idempotencyStore) Consume(ctx context.Context, k storage.IdemKey) (bool, error) {
	if k.Key == "" || k.Namespace == "" {
		return false, storage.NewError(storage.ErrInvalidArgument, "idempotency namespace and key required")
	}
	if err := storage.ValidateIdemKeyIdentifiers(k); err != nil {
		return false, err
	}
	id := k.TenantID + "|" + k.Namespace + "|" + k.Key
	now := time.Now()
	physicalID := s.schema.id("idempotency_keys", "id", id)
	var expiresAt time.Time
	if k.TTL > 0 {
		expiresAt = now.Add(k.TTL)
	}

	for attempt := 0; attempt < 2; attempt++ {
		_, err := s.db.ExecContext(ctx, `INSERT INTO idempotency_keys (id, expires_at, created_at) VALUES (?,?,?)`,
			physicalID, nullTime(expiresAt), now)
		if err == nil {
			return true, nil
		}
		if !isDuplicate(err) {
			return false, err
		}

		// The conditional update is the expiry CAS. At most one contender can
		// move the same expired row to a future expiry.
		result, err := s.db.ExecContext(ctx, `UPDATE idempotency_keys SET expires_at=?, created_at=? WHERE id=? AND expires_at IS NOT NULL AND expires_at<=?`,
			nullTime(expiresAt), now, physicalID, now)
		if err != nil {
			return false, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		if affected == 1 {
			return true, nil
		}

		// A concurrent cleanup may remove the row after the duplicate INSERT.
		// Retry the INSERT once only when the row is actually gone. If it still
		// exists, another caller owns the current claim.
		var exists int
		if err := s.db.QueryRowContext(ctx, "SELECT 1 FROM idempotency_keys WHERE id=?", physicalID).Scan(&exists); err == sql.ErrNoRows {
			continue
		} else if err != nil {
			return false, err
		}
		return false, nil
	}
	return false, nil
}

var _ storage.IdempotencyStore = (*idempotencyStore)(nil)
