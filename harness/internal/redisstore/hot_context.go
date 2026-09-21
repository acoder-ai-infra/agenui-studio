package redisstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	ctxpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
)

// HotContextStore is a distributed context.HotContextStore backed by Redis. The
// compare-and-swap operation uses a Lua script for atomicity: the version check
// and write execute in a single Redis EVAL, preventing concurrent turns on
// different instances from silently overwriting a newer hot projection.
type HotContextStore struct {
	client Client
	prefix string
	ttl    time.Duration
}

// NewHotContextStore builds a Redis-backed HotContextStore.
// When cfg.SnapshotTTL is 0, the hot context key never expires (not
// recommended for production — abandoned sessions will accumulate).
// Negative values fall back to the 24h default.
func NewHotContextStore(client Client, cfg Config) *HotContextStore {
	ttl := cfg.SnapshotTTL
	if ttl < 0 {
		ttl = 24 * time.Hour
	}
	return &HotContextStore{
		client: client,
		prefix: prefix(cfg) + "hot-ctx:",
		ttl:    ttl,
	}
}

func (s *HotContextStore) key(sessionID string) string {
	return s.prefix + "{" + sessionID + "}"
}

// hotContextEnvelope wraps HotContextView for JSON serialization. The Version
// field lives here so the Lua CAS script can extract it without parsing the
// full Messages/Fragments payload.
type hotContextEnvelope struct {
	Messages  []ctxpkg.Message         `json:"messages,omitempty"`
	Fragments []ctxpkg.ContextFragment `json:"fragments,omitempty"`
	Version   int64                    `json:"version"`
}

// casScript atomically checks the stored version and writes the new payload.
// KEYS[1] = hot context key
// ARGV[1] = expected version (as string)
// ARGV[2] = new payload (JSON)
// ARGV[3] = TTL seconds (0 means no expiry)
// Returns: 1 = success, 0 = version conflict, -1 = key did not exist but expected version was 0 (first write, success),
//          -2 = key did not exist but expected version was > 0 (stale CAS after expiry, caller must reload)
var casScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
local expected = tonumber(ARGV[1])
local ttl = tonumber(ARGV[3])
if current == false then
  if expected == 0 then
    if ttl > 0 then
      redis.call('SET', KEYS[1], ARGV[2], 'EX', ttl)
    else
      redis.call('SET', KEYS[1], ARGV[2])
    end
    return -1
  end
  return -2
end
local stored_ver = tonumber(cjson.decode(current).version or 0)
if stored_ver ~= expected then
  return 0
end
if ttl > 0 then
  redis.call('SET', KEYS[1], ARGV[2], 'EX', ttl)
else
  redis.call('SET', KEYS[1], ARGV[2])
end
return 1
`)

// Load returns the current hot context view for a session. Returns
// ErrHotContextMiss when the key does not exist (cold start or expired).
func (s *HotContextStore) Load(ctx context.Context, sessionID string, maxMessages int) (ctxpkg.HotContextView, error) {
	if sessionID == "" {
		return ctxpkg.HotContextView{}, ctxpkg.ErrSessionIDMissing
	}
	data, err := s.client.Get(ctx, s.key(sessionID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return ctxpkg.HotContextView{}, ctxpkg.ErrHotContextMiss
	}
	if err != nil {
		return ctxpkg.HotContextView{}, fmt.Errorf("redis get: %w", err)
	}
	var env hotContextEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return ctxpkg.HotContextView{}, fmt.Errorf("unmarshal hot context: %w", err)
	}
	view := ctxpkg.HotContextView{
		Messages:  env.Messages,
		Fragments: env.Fragments,
		Version:   env.Version,
	}
	// Clear transient CurrentInput flag (json:"-" means it's already false after
	// unmarshal, but be explicit for defense-in-depth).
	for i := range view.Messages {
		view.Messages[i].CurrentInput = false
	}
	if maxMessages > 0 && len(view.Messages) > maxMessages {
		view.Messages = view.Messages[len(view.Messages)-maxMessages:]
	}
	return view, nil
}

// CompareAndSwap atomically replaces the hot context view if the stored version
// matches expectedVersion. On conflict, returns ErrHotContextConflict so the
// caller can reload and retry.
func (s *HotContextStore) CompareAndSwap(ctx context.Context, sessionID string, expectedVersion int64, next ctxpkg.HotContextView) (ctxpkg.HotContextView, error) {
	if sessionID == "" {
		return ctxpkg.HotContextView{}, ctxpkg.ErrSessionIDMissing
	}
	if expectedVersion < 0 {
		return ctxpkg.HotContextView{}, fmt.Errorf("%w: negative expected version", ctxpkg.ErrHotContextConflict)
	}
	// Clear transient flags before serializing.
	for i := range next.Messages {
		next.Messages[i].CurrentInput = false
	}
	// Compute the new version: expectedVersion + 1.
	next.Version = expectedVersion + 1
	env := hotContextEnvelope{
		Messages:  next.Messages,
		Fragments: next.Fragments,
		Version:   next.Version,
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return ctxpkg.HotContextView{}, fmt.Errorf("marshal hot context: %w", err)
	}
	// TTL in seconds. 0 means no expiry (handled by the Lua script).
	ttlSec := int64(s.ttl.Seconds())
	if ttlSec < 0 {
		ttlSec = 0
	}
	result, err := casScript.Run(ctx, s.client,
		[]string{s.key(sessionID)},
		expectedVersion, payload, ttlSec,
	).Int()
	if err != nil {
		return ctxpkg.HotContextView{}, fmt.Errorf("redis lua cas: %w", err)
	}
	if result == -2 {
		// Key expired after caller loaded expectedVersion. The caller's version is
		// stale; return a conflict so it can reload from the ledger and retry.
		return ctxpkg.HotContextView{}, fmt.Errorf("%w: expected=%d but key expired", ctxpkg.ErrHotContextConflict, expectedVersion)
	}
	if result == 0 {
		// Version conflict: load current state so caller can retry with fresh data.
		current, loadErr := s.Load(ctx, sessionID, 0)
		if loadErr != nil {
			return ctxpkg.HotContextView{}, fmt.Errorf("%w: expected=%d", ctxpkg.ErrHotContextConflict, expectedVersion)
		}
		return current, fmt.Errorf("%w: expected=%d actual=%d", ctxpkg.ErrHotContextConflict, expectedVersion, current.Version)
	}
	// result == 1 (success) or -1 (first write with expectedVersion==0, also success)
	return next, nil
}

var _ ctxpkg.HotContextStore = (*HotContextStore)(nil)
