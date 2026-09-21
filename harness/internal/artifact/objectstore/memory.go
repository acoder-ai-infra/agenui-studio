package objectstore

import (
	"bytes"
	"context"
	"io"
	"sync"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/downloadtoken"
)

type MemoryObjectStore struct {
	mu      sync.RWMutex
	objects map[string][]byte
	codec   *downloadtoken.Codec
}

// SetDownloadCodec installs the optional token codec used to mint redeemable
// download tokens. Nil (the default) preserves opaque random tokens.
func (s *MemoryObjectStore) SetDownloadCodec(codec *downloadtoken.Codec) {
	s.codec = codec
}

func (s *MemoryObjectStore) DownloadCodec() *downloadtoken.Codec { return s.codec }

func NewMemoryObjectStore() *MemoryObjectStore {
	return &MemoryObjectStore{objects: make(map[string][]byte)}
}

func NewMemory() *MemoryObjectStore {
	return NewMemoryObjectStore()
}

func (s *MemoryObjectStore) Backend() string {
	return "memory"
}

func (*MemoryObjectStore) ProductionReady() bool { return false }

func (s *MemoryObjectStore) Put(_ context.Context, key string, r io.Reader) (*artifact.ObjectInfo, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = append([]byte(nil), data...)
	return &artifact.ObjectInfo{Backend: s.Backend(), Key: key, Size: int64(len(data))}, nil
}

func (s *MemoryObjectStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: "object not found: " + key}
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), data...))), nil
}

func (s *MemoryObjectStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

func (s *MemoryObjectStore) CreateDownloadURL(_ context.Context, key string, ttl time.Duration) (*artifact.DownloadURL, error) {
	s.mu.RLock()
	_, exists := s.objects[key]
	s.mu.RUnlock()
	if !exists {
		return nil, &artifact.Error{Code: artifact.ErrNotFound, Message: "object not found: " + key}
	}
	effectiveTTL, err := downloadTTL(ttl)
	if err != nil {
		return nil, err
	}
	expiresAt := time.Now().Add(effectiveTTL)
	token, err := IssueDownloadToken(s.codec, key, expiresAt)
	if err != nil {
		return nil, err
	}
	return &artifact.DownloadURL{
		URL:       "memory://download/" + token,
		ExpiresAt: expiresAt,
	}, nil
}
