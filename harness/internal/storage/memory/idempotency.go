package memory

import (
	"context"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

type idempotencyStore struct {
	mu   sync.Mutex
	seen map[string]time.Time // key -> expiry (zero = no expiry)
}

func newIdempotencyStore() *idempotencyStore {
	return &idempotencyStore{seen: make(map[string]time.Time)}
}

func idemKey(k storage.IdemKey) string {
	return k.TenantID + "|" + k.Namespace + "|" + k.Key
}

// Consume returns ok=true the first time a key is used; subsequent uses return
// ok=false (replay). Expired keys are treated as unused.
func (s *idempotencyStore) Consume(_ context.Context, k storage.IdemKey) (bool, error) {
	if k.Key == "" || k.Namespace == "" {
		return false, storageInvalid("idempotency namespace and key required")
	}
	if err := storage.ValidateIdemKeyIdentifiers(k); err != nil {
		return false, err
	}
	key := idemKey(k)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if exp, ok := s.seen[key]; ok {
		if exp.IsZero() || exp.After(now) {
			return false, nil // already consumed and not expired
		}
	}
	var exp time.Time
	if k.TTL > 0 {
		exp = now.Add(k.TTL)
	}
	s.seen[key] = exp
	return true, nil
}
