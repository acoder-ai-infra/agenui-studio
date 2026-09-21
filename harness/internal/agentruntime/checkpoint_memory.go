package agentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

const DefaultMaxRuntimeCheckpointBytes = 32 << 20

type InMemoryRuntimeCheckpointStore struct {
	mu              sync.RWMutex
	records         map[string]RuntimeCheckpointRecord
	clock           func() time.Time
	maxPayloadBytes int
}

func NewInMemoryRuntimeCheckpointStore() *InMemoryRuntimeCheckpointStore {
	return &InMemoryRuntimeCheckpointStore{
		records:         make(map[string]RuntimeCheckpointRecord),
		clock:           time.Now,
		maxPayloadBytes: DefaultMaxRuntimeCheckpointBytes,
	}
}

func (s *InMemoryRuntimeCheckpointStore) Load(_ context.Context, key RuntimeCheckpointKey) (RuntimeCheckpointRecord, bool, error) {
	if err := validateRuntimeCheckpointKey(key); err != nil {
		return RuntimeCheckpointRecord{}, false, err
	}
	s.mu.RLock()
	record, ok := s.records[runtimeCheckpointMapKey(key)]
	s.mu.RUnlock()
	if !ok {
		return RuntimeCheckpointRecord{}, false, nil
	}
	now := s.now()
	if !record.ExpiresAt.IsZero() && !record.ExpiresAt.After(now) {
		s.mu.Lock()
		delete(s.records, runtimeCheckpointMapKey(key))
		s.mu.Unlock()
		return RuntimeCheckpointRecord{}, false, nil
	}
	if runtimeCheckpointHash(record.Payload) != record.PayloadHash {
		return RuntimeCheckpointRecord{}, false, ErrCheckpointHashMismatch
	}
	return cloneRuntimeCheckpoint(record), true, nil
}

func (s *InMemoryRuntimeCheckpointStore) Save(_ context.Context, record RuntimeCheckpointRecord) (RuntimeCheckpointRecord, error) {
	if err := validateRuntimeCheckpointRecord(record); err != nil {
		return RuntimeCheckpointRecord{}, err
	}
	maxBytes := s.maxPayloadBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxRuntimeCheckpointBytes
	}
	if len(record.Payload) > maxBytes {
		return RuntimeCheckpointRecord{}, fmt.Errorf("%w: size=%d max=%d", ErrCheckpointPayloadTooLarge, len(record.Payload), maxBytes)
	}
	now := s.now()
	record.SchemaVersion = RuntimeCheckpointSchemaVersion
	record.Payload = append([]byte(nil), record.Payload...)
	record.PayloadHash = runtimeCheckpointHash(record.Payload)
	record.SizeBytes = int64(len(record.Payload))
	record.UpdatedAt = now
	key := runtimeCheckpointMapKey(RuntimeCheckpointKey{Scope: record.Scope, CheckpointID: record.CheckpointID})

	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.records[key]; ok {
		record.Revision = previous.Revision + 1
		record.CreatedAt = previous.CreatedAt
	} else {
		record.Revision = 1
		if record.CreatedAt.IsZero() {
			record.CreatedAt = now
		}
	}
	s.records[key] = record
	return cloneRuntimeCheckpoint(record), nil
}

func (s *InMemoryRuntimeCheckpointStore) Delete(_ context.Context, key RuntimeCheckpointKey) error {
	if err := validateRuntimeCheckpointKey(key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, runtimeCheckpointMapKey(key))
	return nil
}

func (s *InMemoryRuntimeCheckpointStore) now() time.Time {
	if s.clock == nil {
		return time.Now()
	}
	return s.clock()
}

func validateRuntimeCheckpointRecord(record RuntimeCheckpointRecord) error {
	if err := validateRuntimeCheckpointKey(RuntimeCheckpointKey{Scope: record.Scope, CheckpointID: record.CheckpointID}); err != nil {
		return err
	}
	if len(record.Payload) == 0 {
		return fmt.Errorf("%w: payload required", ErrCheckpointInvalid)
	}
	return nil
}

func validateRuntimeCheckpointKey(key RuntimeCheckpointKey) error {
	if key.CheckpointID == "" || key.Scope.SessionID == "" || key.Scope.RunID == "" || key.Scope.Runtime == "" {
		return fmt.Errorf("%w: checkpoint_id, session_id, run_id and runtime required", ErrCheckpointInvalid)
	}
	return nil
}

func runtimeCheckpointMapKey(key RuntimeCheckpointKey) string {
	return key.Scope.TenantID + "\x00" + key.Scope.SessionID + "\x00" + key.Scope.RunID + "\x00" + string(key.Scope.Runtime) + "\x00" + key.Scope.RuntimeVersion + "\x00" + key.Scope.AdapterVersion + "\x00" + key.CheckpointID
}

func runtimeCheckpointHash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func cloneRuntimeCheckpoint(record RuntimeCheckpointRecord) RuntimeCheckpointRecord {
	record.Payload = append([]byte(nil), record.Payload...)
	return record
}
