package ruleworker

import (
	"context"
	"errors"
	"testing"
)

func TestInProcessLockerAllowsOneWorkerPass(t *testing.T) {
	locker := &InProcessLocker{}
	lease, acquired, err := locker.TryLock(context.Background())
	if err != nil || !acquired {
		t.Fatalf("first lock acquired=%v err=%v", acquired, err)
	}
	if _, acquired, err := locker.TryLock(context.Background()); err != nil || acquired {
		t.Fatalf("second lock acquired=%v err=%v", acquired, err)
	}
	status, err := locker.LockStatus(context.Background())
	if err != nil || !status.Held || status.Backend != "in-process" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	lease.Release()
	if _, acquired, err := locker.TryLock(context.Background()); err != nil || !acquired {
		t.Fatalf("lock after release acquired=%v err=%v", acquired, err)
	}
}

func TestInProcessLockerCannotBeForceReleased(t *testing.T) {
	locker := &InProcessLocker{}
	if released, err := locker.ForceRelease(context.Background()); released || !errors.Is(err, ErrManualUnlockUnsupported) {
		t.Fatalf("released=%v err=%v", released, err)
	}
}
