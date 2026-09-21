package ruleworker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

var ErrManualUnlockUnsupported = errors.New("manual unlock is unavailable for an in-process lock")

type LockLease interface {
	Context() context.Context
	Release()
}

// Locker prevents two local Worker passes from publishing the same revision.
// Distributed scheduling is intentionally outside Studio's public core.
type Locker interface {
	TryLock(ctx context.Context) (lease LockLease, acquired bool, err error)
}

type LockStatus struct {
	Backend   string `json:"backend"`
	Key       string `json:"key,omitempty"`
	Held      bool   `json:"held"`
	TTLMillis int64  `json:"ttlMillis"`
}

type LockAdministrator interface {
	LockStatus(ctx context.Context) (LockStatus, error)
	ForceRelease(ctx context.Context) (bool, error)
}

type InProcessLocker struct {
	mu   sync.Mutex
	held atomic.Bool
}

func (l *InProcessLocker) TryLock(ctx context.Context) (LockLease, bool, error) {
	if !l.mu.TryLock() {
		return nil, false, nil
	}
	l.held.Store(true)
	return &inProcessLease{ctx: ctx, locker: l}, true, nil
}

type inProcessLease struct {
	ctx    context.Context
	locker *InProcessLocker
	once   sync.Once
}

func (l *inProcessLease) Context() context.Context { return l.ctx }
func (l *inProcessLease) Release() {
	l.once.Do(func() {
		l.locker.held.Store(false)
		l.locker.mu.Unlock()
	})
}

func (l *InProcessLocker) LockStatus(context.Context) (LockStatus, error) {
	return LockStatus{Backend: "in-process", Held: l.held.Load()}, nil
}

func (l *InProcessLocker) ForceRelease(context.Context) (bool, error) {
	return false, ErrManualUnlockUnsupported
}
