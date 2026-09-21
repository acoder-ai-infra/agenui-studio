package agentgateway

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RemoteAgent is the trusted connection configuration for a remote A2A Agent.
// It stores only a credential reference, never a secret.
type RemoteAgent struct {
	AgentRef               string
	Protocol               string // P0 only allows "a2a"
	BaseURL                string // AgentCard discovery base
	CardPath               string // empty uses /.well-known/agent-card.json
	Transport              string // P0 only allows "JSONRPC"
	AuthRef                string // credential reference, never a secret
	AllowedEndpointOrigins []string
	Timeout                time.Duration
}

// RemoteAgentResolver answers "which trusted remote A2A Agent does agentRef map
// to?" It never registers Agents, builds prompts, issues requests, or stores task
// state.
type RemoteAgentResolver interface {
	Resolve(ctx context.Context, agentRef string) (RemoteAgent, error)
}

// CredentialsProvider maps a credential reference to request headers. P0 only
// reserves the interface; it does not build a credential management system.
type CredentialsProvider interface {
	RequestHeaders(ctx context.Context, authRef string) (http.Header, error)
}

// A2AProtocol and A2AJSONRPCTransport are the only P0 remote protocol/transport.
const (
	A2AProtocol          = "a2a"
	A2AJSONRPCTransport  = "JSONRPC"
	defaultAgentCardPath = "/.well-known/agent-card.json"
)

// StaticRemoteAgentResolver is the P0 concurrency-safe, construction-validated
// resolver. It clones all input and normalizes allowed origins.
type StaticRemoteAgentResolver struct {
	byRef map[string]RemoteAgent
}

// NewStaticRemoteAgentResolver validates and freezes the agent map.
func NewStaticRemoteAgentResolver(agents map[string]RemoteAgent) (*StaticRemoteAgentResolver, error) {
	resolver := &StaticRemoteAgentResolver{byRef: make(map[string]RemoteAgent, len(agents))}
	for ref, agent := range agents {
		if strings.TrimSpace(ref) == "" || ref != agent.AgentRef {
			return nil, newGatewayError(CodeRemoteTargetInvalid, "agent ref must be non-empty and match the map key", nil)
		}
		normalized, err := normalizeRemoteAgent(agent)
		if err != nil {
			return nil, err
		}
		resolver.byRef[ref] = normalized
	}
	return resolver, nil
}

// Resolve returns a deep copy of the trusted remote agent for agentRef.
func (r *StaticRemoteAgentResolver) Resolve(ctx context.Context, agentRef string) (RemoteAgent, error) {
	if err := ctx.Err(); err != nil {
		return RemoteAgent{}, err
	}
	agent, ok := r.byRef[agentRef]
	if !ok {
		return RemoteAgent{}, newGatewayError(CodeRemoteTargetNotFound, "no remote agent for ref "+agentRef, nil)
	}
	return cloneRemoteAgent(agent), nil
}

var _ RemoteAgentResolver = (*StaticRemoteAgentResolver)(nil)

func normalizeRemoteAgent(agent RemoteAgent) (RemoteAgent, error) {
	if agent.Protocol != A2AProtocol {
		return RemoteAgent{}, newGatewayError(CodeRemoteTargetInvalid, "protocol must be a2a", nil)
	}
	if agent.Transport != A2AJSONRPCTransport {
		return RemoteAgent{}, newGatewayError(CodeRemoteTargetInvalid, "transport must be JSONRPC", nil)
	}
	if agent.Timeout <= 0 {
		return RemoteAgent{}, newGatewayError(CodeRemoteTargetInvalid, "timeout must be positive", nil)
	}
	base, err := url.Parse(strings.TrimSpace(agent.BaseURL))
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return RemoteAgent{}, newGatewayError(CodeRemoteTargetInvalid, "base url must be a valid http(s) url", nil)
	}
	origins, err := normalizeOrigins(agent.AllowedEndpointOrigins)
	if err != nil {
		return RemoteAgent{}, err
	}
	agent.AllowedEndpointOrigins = origins
	return cloneRemoteAgent(agent), nil
}

// NormalizeRemoteAgent validates and freezes one Registry-resolved remote
// target without requiring a second process-local target registry.
func NormalizeRemoteAgent(agent RemoteAgent) (RemoteAgent, error) {
	return normalizeRemoteAgent(agent)
}

// normalizeOrigins reduces each origin to scheme://host:port and rejects any
// origin carrying a path, query, or userinfo.
func normalizeOrigins(origins []string) ([]string, error) {
	if len(origins) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(origins))
	out := make([]string, 0, len(origins))
	for _, raw := range origins {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, newGatewayError(CodeRemoteTargetInvalid, "invalid allowed endpoint origin", nil)
		}
		if parsed.Path != "" || parsed.RawQuery != "" || parsed.User != nil {
			return nil, newGatewayError(CodeRemoteTargetInvalid, "allowed endpoint origin must not carry path/query/userinfo", nil)
		}
		origin := parsed.Scheme + "://" + parsed.Host
		if _, ok := seen[origin]; ok {
			continue
		}
		seen[origin] = struct{}{}
		out = append(out, origin)
	}
	return out, nil
}

func cloneRemoteAgent(agent RemoteAgent) RemoteAgent {
	agent.AllowedEndpointOrigins = append([]string(nil), agent.AllowedEndpointOrigins...)
	return agent
}

// sameOrigin reports whether candidate has the same scheme://host:port as base.
func sameOrigin(base, candidate *url.URL) bool {
	return base.Scheme == candidate.Scheme && base.Host == candidate.Host
}

// endpointAllowed reports whether the candidate URL is same-origin with the base
// URL or matches a normalized allowed origin.
func endpointAllowed(baseURL string, allowedOrigins []string, candidateURL string) bool {
	base, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	candidate, err := url.Parse(candidateURL)
	if err != nil || candidate.Scheme == "" || candidate.Host == "" {
		return false
	}
	if sameOrigin(base, candidate) {
		return true
	}
	origin := candidate.Scheme + "://" + candidate.Host
	for _, allowed := range allowedOrigins {
		if allowed == origin {
			return true
		}
	}
	return false
}
