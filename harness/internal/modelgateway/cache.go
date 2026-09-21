package modelgateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
)

type ModelCache interface {
	Get(ctx context.Context, key string) (ModelResponse, bool)
	Put(ctx context.Context, key string, resp ModelResponse)
}

type MemoryModelCache struct {
	mu    sync.Mutex
	items map[string]ModelResponse
}

func NewMemoryModelCache() *MemoryModelCache {
	return &MemoryModelCache{items: map[string]ModelResponse{}}
}

func (c *MemoryModelCache) Get(_ context.Context, key string) (ModelResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	resp, ok := c.items[key]
	return resp, ok
}

func (c *MemoryModelCache) Put(_ context.Context, key string, resp ModelResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = resp
}

func cacheKey(req ModelRequest, target ModelTarget) string {
	tenant := req.Trace.TenantID
	if tenant == "" {
		tenant = "default"
	}
	toolHash := hashRaw(req.ToolsSchema)
	msgHash := hashAny(req.Messages)
	optionsHash := hashAny(req.Options)
	return fmt.Sprintf("%s|%s|%s|prompt=%s|ctx=%s|tools=%s|schema=%s|options=%s|messages=%s",
		tenant, target.Provider, target.Model, req.PromptVersion, req.ContextHash, toolHash, req.ResponseSchemaRef, optionsHash, msgHash)
}

func hashRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func hashAny(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
