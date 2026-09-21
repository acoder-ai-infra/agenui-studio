package storagewrite

import (
	"container/list"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
)

var (
	ErrTraceIDRequired      = errors.New("trace_id required")
	ErrTenantIDRequired     = errors.New("tenant_id required")
	ErrRunIDRequired        = errors.New("run_id required")
	ErrIdempotencyRequired  = errors.New("idempotency_key required")
	ErrStoreMissing         = errors.New("store port missing")
	ErrStoreMismatch        = errors.New("store mismatch")
	ErrRequiredWriteFailed  = errors.New("required write failed")
	ErrUnsupportedOperation = errors.New("unsupported store operation")
	ErrMemoryPortForbidden  = errors.New("memory store port forbidden in production")
)

type Executor struct {
	mu         sync.Mutex
	ports      map[StoreName]StorePort
	ledger     map[string]*writeState
	completed  *list.List
	maxEntries int
}

type writeState struct {
	key       string
	done      chan struct{}
	result    WriteResult
	completed *list.Element
}

func NewExecutor(ports map[StoreName]StorePort) *Executor {
	return NewExecutorWithCacheLimit(ports, 65536)
}

// NewExecutorWithCacheLimit bounds process-local duplicate suppression. Store
// adapters remain the durable idempotency authority after an entry is evicted.
func NewExecutorWithCacheLimit(ports map[StoreName]StorePort, maxEntries int) *Executor {
	copied := make(map[StoreName]StorePort, len(ports))
	for name, port := range ports {
		copied[name] = port
	}
	if maxEntries <= 0 {
		maxEntries = 1
	}
	return &Executor{ports: copied, ledger: make(map[string]*writeState), completed: list.New(), maxEntries: maxEntries}
}

// ValidateProduction rejects development-only in-memory persistence ports.
func (e *Executor) ValidateProduction() error {
	if e == nil {
		return ErrStoreMissing
	}
	for name, port := range e.ports {
		switch port.(type) {
		case *MemoryPort, *MemorySink:
			return fmt.Errorf("%w: %s", ErrMemoryPortForbidden, name)
		}
	}
	return nil
}

func (e *Executor) Execute(ctx context.Context, plan Plan) (Result, error) {
	if err := validatePlan(plan); err != nil {
		return Result{}, err
	}
	var result Result

	for _, write := range plan.RequiredWrites {
		write.Blocking = true
		writeResult := e.executeWrite(ctx, plan, write)
		result.Required = append(result.Required, writeResult)
		if writeResult.Err != nil {
			return result, fmt.Errorf("%w: %s/%s/%s: %w", ErrRequiredWriteFailed, write.Store, write.Operation, write.Ref, writeResult.Err)
		}
	}

	for _, write := range plan.OptionalWrites {
		result.Optional = append(result.Optional, e.executeWrite(ctx, plan, write))
	}

	for _, projection := range plan.Projections {
		write := Write{
			Store:      projection.Store,
			Operation:  projection.Operation,
			Ref:        projection.Ref,
			PayloadRef: projection.PayloadRef,
			Payload:    projection.Payload,
			Blocking:   false,
		}
		writeResult := e.executeWrite(ctx, plan, write)
		result.Projections = append(result.Projections, ProjectionResult{
			Projection: projection,
			Skipped:    writeResult.Skipped,
			Err:        writeResult.Err,
		})
	}

	return result, nil
}

func (e *Executor) executeWrite(ctx context.Context, plan Plan, write Write) WriteResult {
	write = enrichWrite(plan, write)
	if write.Store == "" || write.Operation == "" {
		return WriteResult{Write: write, Err: ErrUnsupportedOperation}
	}
	key := writeKey(plan, write)
	state, owner := e.begin(key)
	if !owner {
		select {
		case <-ctx.Done():
			return WriteResult{Write: write, Err: ctx.Err()}
		case <-state.done:
			result := state.result
			result.Write = write
			result.Skipped = result.Err == nil
			return result
		}
	}
	result := WriteResult{Write: write}
	defer func() {
		e.finish(key, result)
	}()

	port := e.ports[write.Store]
	if port == nil {
		result.Err = fmt.Errorf("%w: %s", ErrStoreMissing, write.Store)
		return result
	}
	receipt, err := port.Write(ctx, write)
	if err != nil {
		result.Err = err
		return result
	}
	result.Receipt = receipt
	return result
}

func (e *Executor) begin(key string) (*writeState, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if state, ok := e.ledger[key]; ok {
		if state.completed != nil {
			e.completed.MoveToBack(state.completed)
		}
		return state, false
	}
	e.pruneCompletedLocked(e.maxEntries - 1)
	state := &writeState{key: key, done: make(chan struct{})}
	e.ledger[key] = state
	return state, true
}

func (e *Executor) finish(key string, result WriteResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.ledger[key]
	if state == nil {
		return
	}
	state.result = result
	if result.Err != nil {
		delete(e.ledger, key)
	} else {
		state.completed = e.completed.PushBack(state)
		e.pruneCompletedLocked(e.maxEntries)
	}
	close(state.done)
}

func (e *Executor) pruneCompletedLocked(target int) {
	for len(e.ledger) > target {
		front := e.completed.Front()
		if front == nil {
			return
		}
		state := front.Value.(*writeState)
		e.completed.Remove(front)
		state.completed = nil
		delete(e.ledger, state.key)
	}
}

func validatePlan(plan Plan) error {
	if plan.TraceID == "" {
		return ErrTraceIDRequired
	}
	if plan.TenantID == "" {
		return ErrTenantIDRequired
	}
	if plan.RunID == "" {
		return ErrRunIDRequired
	}
	if plan.IdempotencyKey == "" {
		return ErrIdempotencyRequired
	}
	return nil
}

func writeKey(plan Plan, write Write) string {
	parts := [...]string{
		plan.TenantID,
		plan.RunID,
		write.IdempotencyKey,
		string(write.Store),
		string(write.Operation),
		write.Ref,
	}
	key := make([]byte, 0, 128)
	for _, part := range parts {
		key = binary.AppendUvarint(key, uint64(len(part)))
		key = append(key, part...)
	}
	return string(key)
}

func enrichWrite(plan Plan, write Write) Write {
	if write.TraceID == "" {
		write.TraceID = plan.TraceID
	}
	if write.TenantID == "" {
		write.TenantID = plan.TenantID
	}
	if write.SessionID == "" {
		write.SessionID = plan.SessionID
	}
	if write.RunID == "" {
		write.RunID = plan.RunID
	}
	if write.IdempotencyKey == "" {
		write.IdempotencyKey = plan.IdempotencyKey
	}
	if write.Reason == "" {
		write.Reason = plan.Reason
	}
	return write
}
