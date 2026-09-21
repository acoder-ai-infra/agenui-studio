package agentruntime

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/adk"
)

const DefaultEinoCheckpointTTL = 24 * time.Hour

// einoCheckpointStoreProxy implements Eino's native store interface while
// delegating persistence to the Harness RuntimeCheckpointStore port.
type einoCheckpointStoreProxy struct {
	store RuntimeCheckpointStore
	scope RuntimeCheckpointScope
	clock func() time.Time
	ttl   time.Duration
}

var (
	_ adk.CheckPointStore   = (*einoCheckpointStoreProxy)(nil)
	_ adk.CheckPointDeleter = (*einoCheckpointStoreProxy)(nil)
)

func newEinoCheckpointStoreProxy(store RuntimeCheckpointStore, scope RuntimeCheckpointScope) (adk.CheckPointStore, error) {
	if store == nil {
		return nil, ErrCheckpointStoreMissing
	}
	if err := validateRuntimeCheckpointKey(RuntimeCheckpointKey{Scope: scope, CheckpointID: "validation"}); err != nil {
		return nil, err
	}
	return &einoCheckpointStoreProxy{store: store, scope: scope, clock: time.Now, ttl: DefaultEinoCheckpointTTL}, nil
}

func (s *einoCheckpointStoreProxy) Get(ctx context.Context, checkpointID string) ([]byte, bool, error) {
	record, ok, err := s.store.Load(ctx, RuntimeCheckpointKey{Scope: s.scope, CheckpointID: checkpointID})
	if err != nil || !ok {
		return nil, ok, err
	}
	if record.SchemaVersion != RuntimeCheckpointSchemaVersion {
		return nil, false, fmt.Errorf("%w: schema_version=%s", ErrCheckpointInvalid, record.SchemaVersion)
	}
	if runtimeCheckpointHash(record.Payload) != record.PayloadHash {
		return nil, false, ErrCheckpointHashMismatch
	}
	return append([]byte(nil), record.Payload...), true, nil
}

func (s *einoCheckpointStoreProxy) Set(ctx context.Context, checkpointID string, payload []byte) error {
	now := s.clock()
	record := RuntimeCheckpointRecord{
		SchemaVersion: RuntimeCheckpointSchemaVersion,
		Scope:         s.scope,
		CheckpointID:  checkpointID,
		Payload:       append([]byte(nil), payload...),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if s.ttl > 0 {
		record.ExpiresAt = now.Add(s.ttl)
	}
	_, err := s.store.Save(ctx, record)
	return err
}

func (s *einoCheckpointStoreProxy) Delete(ctx context.Context, checkpointID string) error {
	return s.store.Delete(ctx, RuntimeCheckpointKey{Scope: s.scope, CheckpointID: checkpointID})
}
