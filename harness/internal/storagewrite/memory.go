package storagewrite

import (
	"context"
	"sync"
)

type MemorySink struct {
	mu       sync.Mutex
	records  []Write
	failures map[Operation]error
}

func NewMemorySink() *MemorySink {
	return &MemorySink{failures: make(map[Operation]error)}
}

func (s *MemorySink) Write(_ context.Context, write Write) (WriteReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failures[write.Operation]; err != nil {
		return WriteReceipt{}, err
	}
	s.records = append(s.records, write)
	return WriteReceipt{Store: write.Store, Ref: write.Ref}, nil
}

func (s *MemorySink) Fail(operation Operation, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[operation] = err
}

func (s *MemorySink) Records() []Write {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Write, len(s.records))
	copy(out, s.records)
	return out
}

type MemoryPort struct {
	store StoreName
	sink  *MemorySink
}

func NewMemoryPort(store StoreName) *MemoryPort {
	return &MemoryPort{store: store, sink: NewMemorySink()}
}

func (p *MemoryPort) Write(ctx context.Context, write Write) (WriteReceipt, error) {
	if write.Store != p.store {
		return WriteReceipt{}, ErrStoreMismatch
	}
	return p.sink.Write(ctx, write)
}

func (p *MemoryPort) Fail(operation Operation, err error) {
	p.sink.Fail(operation, err)
}

func (p *MemoryPort) Records() []Write {
	return p.sink.Records()
}

type MemoryStores struct {
	Run        *MemoryPort
	Step       *MemoryPort
	Fallback   *MemoryPort
	Message    *MemoryPort
	Event      *MemoryPort
	Artifact   *MemoryPort
	HotSession *MemoryPort
	HotContext *MemoryPort
	HotStream  *MemoryPort
}

func NewMemoryStores() *MemoryStores {
	return &MemoryStores{
		Run:        NewMemoryPort(StoreRun),
		Step:       NewMemoryPort(StoreStep),
		Fallback:   NewMemoryPort(StoreFallback),
		Message:    NewMemoryPort(StoreMessage),
		Event:      NewMemoryPort(StoreEvent),
		Artifact:   NewMemoryPort(StoreArtifact),
		HotSession: NewMemoryPort(StoreHotSession),
		HotContext: NewMemoryPort(StoreHotContext),
		HotStream:  NewMemoryPort(StoreHotStream),
	}
}

func (s *MemoryStores) Ports() map[StoreName]StorePort {
	return map[StoreName]StorePort{
		StoreRun:        s.Run,
		StoreStep:       s.Step,
		StoreFallback:   s.Fallback,
		StoreMessage:    s.Message,
		StoreEvent:      s.Event,
		StoreArtifact:   s.Artifact,
		StoreHotSession: s.HotSession,
		StoreHotContext: s.HotContext,
		StoreHotStream:  s.HotStream,
	}
}

func (s *MemoryStores) Records(store StoreName) []Write {
	port := s.port(store)
	if port == nil {
		return nil
	}
	return port.Records()
}

func (s *MemoryStores) port(store StoreName) *MemoryPort {
	switch store {
	case StoreRun:
		return s.Run
	case StoreStep:
		return s.Step
	case StoreFallback:
		return s.Fallback
	case StoreMessage:
		return s.Message
	case StoreEvent:
		return s.Event
	case StoreArtifact:
		return s.Artifact
	case StoreHotSession:
		return s.HotSession
	case StoreHotContext:
		return s.HotContext
	case StoreHotStream:
		return s.HotStream
	default:
		return nil
	}
}
