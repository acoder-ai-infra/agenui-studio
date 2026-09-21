package agentregistry

import (
	"context"

	lru "github.com/hashicorp/golang-lru"
)

// DefaultConfigVersionCacheSize bounds the number of published Agent versions
// held in memory by cachingConfigControlStore.
const DefaultConfigVersionCacheSize = 1024

// cachingConfigControlStore decorates an AgentConfigControlStore with an
// in-memory LRU over GetVersion.
//
// Published Agent versions are immutable append-only snapshots keyed by
// (tenant, agent, version), so a hit never needs invalidation: a republish or
// promotion produces a new version id (a new key), and GetRelease — the live
// pointer that governs which version a run resolves to — is intentionally NOT
// cached, so publishing/promoting remains immediately effective on the next
// run. Only successful lookups are cached; misses (ErrAgentConfigControlNotFound
// and other errors) fall through so a not-yet-published version is not pinned.
//
// All other store methods (drafts, releases, publish, promote, list) pass
// straight through to the delegate.
type cachingConfigControlStore struct {
	AgentConfigControlStore
	versions *lru.Cache
}

// NewCachingAgentConfigControlStore wraps delegate with a GetVersion cache of
// the given size. A size <= 0 falls back to DefaultConfigVersionCacheSize. It
// returns the bare delegate if wrapping is not possible so callers can use the
// result unconditionally.
func NewCachingAgentConfigControlStore(delegate AgentConfigControlStore, size int) AgentConfigControlStore {
	if delegate == nil {
		return nil
	}
	if size <= 0 {
		size = DefaultConfigVersionCacheSize
	}
	cache, err := lru.New(size)
	if err != nil {
		return delegate
	}
	return &cachingConfigControlStore{AgentConfigControlStore: delegate, versions: cache}
}

func versionCacheKey(tenantID, agentID, version string) string {
	// NUL separators keep the composite key unambiguous regardless of the
	// characters allowed in the individual identity fields.
	return tenantID + "\x00" + agentID + "\x00" + version
}

func (s *cachingConfigControlStore) GetVersion(ctx context.Context, tenantID, agentID, version string) (AgentConfigVersion, error) {
	key := versionCacheKey(tenantID, agentID, version)
	if cached, ok := s.versions.Get(key); ok {
		return cached.(AgentConfigVersion), nil
	}
	record, err := s.AgentConfigControlStore.GetVersion(ctx, tenantID, agentID, version)
	if err != nil {
		return AgentConfigVersion{}, err
	}
	s.versions.Add(key, record)
	return record, nil
}
