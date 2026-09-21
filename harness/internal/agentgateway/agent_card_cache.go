package agentgateway

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
)

// DefaultAgentCardTTL is the anonymous AgentCard cache lifetime.
const DefaultAgentCardTTL = 5 * time.Minute

// agentCardCache discovers AgentCards, filters them to the exact A2A v1.0
// JSON-RPC interface, and caches anonymous cards with a TTL. Protected cards
// (non-empty AuthRef) are never cached across invocations.
type agentCardCache struct {
	credentials CredentialsProvider
	ttl         time.Duration
	now         func() time.Time

	mu       sync.Mutex
	entries  map[string]*cardCacheEntry
	inflight map[string]chan struct{}
}

type cardCacheEntry struct {
	card      *a2a.AgentCard
	fetchedAt time.Time
}

func newAgentCardCache(credentials CredentialsProvider, ttl time.Duration, now func() time.Time) *agentCardCache {
	if ttl <= 0 {
		ttl = DefaultAgentCardTTL
	}
	if now == nil {
		now = time.Now
	}
	return &agentCardCache{
		credentials: credentials,
		ttl:         ttl,
		now:         now,
		entries:     make(map[string]*cardCacheEntry),
		inflight:    make(map[string]chan struct{}),
	}
}

func cardCacheKey(target RemoteAgent) string {
	// Anonymous cards vary by ref/base/path and the normalized allowed origins,
	// so different trust policies never share a cached result.
	return target.AgentRef + "\x00" + target.BaseURL + "\x00" + target.CardPath + "\x00" + target.AuthRef + "\x00" + joinOrigins(target.AllowedEndpointOrigins)
}

func joinOrigins(origins []string) string {
	return "[" + stringsJoin(origins, ",") + "]"
}

func stringsJoin(values []string, sep string) string {
	out := ""
	for i, v := range values {
		if i > 0 {
			out += sep
		}
		out += v
	}
	return out
}

// Resolve returns a filtered, immutable AgentCard copy suitable for a JSON-RPC
// client. Protected targets always re-discover; anonymous targets use the TTL
// cache with single-flight refresh.
func (c *agentCardCache) Resolve(ctx context.Context, target RemoteAgent) (*a2a.AgentCard, error) {
	credentialHeader, err := c.credentialHeader(ctx, target)
	if err != nil {
		return nil, err
	}
	if target.AuthRef != "" {
		// Protected cards may vary per tenant/session; never share across calls.
		card, err := c.discover(ctx, target, credentialHeader)
		if err != nil {
			return nil, err
		}
		return filterCardForJSONRPC(card, target)
	}
	return c.resolveAnonymous(ctx, target, credentialHeader)
}

func (c *agentCardCache) resolveAnonymous(ctx context.Context, target RemoteAgent, credentialHeader http.Header) (*a2a.AgentCard, error) {
	key := cardCacheKey(target)
	for {
		c.mu.Lock()
		if entry, ok := c.entries[key]; ok && c.now().Sub(entry.fetchedAt) < c.ttl {
			card := entry.card
			c.mu.Unlock()
			return filterCardForJSONRPC(card, target)
		}
		if wait, ok := c.inflight[key]; ok {
			c.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		wait := make(chan struct{})
		c.inflight[key] = wait
		c.mu.Unlock()

		card, err := c.discover(ctx, target, credentialHeader)

		c.mu.Lock()
		delete(c.inflight, key)
		close(wait)
		if err == nil {
			c.entries[key] = &cardCacheEntry{card: card, fetchedAt: c.now()}
		}
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return filterCardForJSONRPC(card, target)
	}
}

func (c *agentCardCache) credentialHeader(ctx context.Context, target RemoteAgent) (http.Header, error) {
	if target.AuthRef == "" {
		return nil, nil
	}
	if c.credentials == nil {
		return nil, newGatewayError(CodeRemoteCredentials, "credentials provider unavailable for auth ref", nil)
	}
	header, err := c.credentials.RequestHeaders(ctx, target.AuthRef)
	if err != nil {
		return nil, newGatewayError(CodeRemoteCredentials, "failed to resolve credentials", err)
	}
	return cloneHeader(header), nil
}

func (c *agentCardCache) discover(ctx context.Context, target RemoteAgent, credentialHeader http.Header) (*a2a.AgentCard, error) {
	base, err := url.Parse(target.BaseURL)
	if err != nil {
		return nil, newGatewayError(CodeRemoteTargetInvalid, "invalid base url", err)
	}
	client := newBoundedA2AClient(requestOrigin(base), credentialHeader, agentCardMaxWireBytes, target.Timeout)
	resolver := agentcard.NewResolver(client)
	opts := []agentcard.ResolveOption{}
	if target.CardPath != "" {
		opts = append(opts, agentcard.WithPath(target.CardPath))
	}
	card, err := resolver.Resolve(ctx, target.BaseURL, opts...)
	if err != nil {
		return nil, newGatewayError(CodeAgentCardDiscoveryFailed, "agent card discovery failed", err)
	}
	return card, nil
}

// filterCardForJSONRPC returns a copy of the card exposing only the exact A2A
// v1.0 JSON-RPC interface whose endpoint is same-origin with BaseURL or matches
// the allowed origins. Unknown required extensions are rejected.
func filterCardForJSONRPC(card *a2a.AgentCard, target RemoteAgent) (*a2a.AgentCard, error) {
	if card == nil {
		return nil, newGatewayError(CodeAgentCardDiscoveryFailed, "empty agent card", nil)
	}
	for _, extension := range card.Capabilities.Extensions {
		if extension.Required {
			return nil, newGatewayError(CodeA2AProtocolIncompatible, "unknown required agent card extension", nil)
		}
	}
	var selected *a2a.AgentInterface
	for _, iface := range card.SupportedInterfaces {
		if iface == nil {
			continue
		}
		if iface.ProtocolBinding != a2a.TransportProtocolJSONRPC || iface.ProtocolVersion != a2a.Version {
			continue
		}
		if !endpointAllowed(target.BaseURL, target.AllowedEndpointOrigins, iface.URL) {
			continue
		}
		ifaceCopy := *iface
		selected = &ifaceCopy
		break
	}
	if selected == nil {
		return nil, newGatewayError(CodeA2AProtocolIncompatible, "no exact A2A v1.0 JSON-RPC interface", nil)
	}
	filtered := *card
	filtered.SupportedInterfaces = []*a2a.AgentInterface{selected}
	return &filtered, nil
}
