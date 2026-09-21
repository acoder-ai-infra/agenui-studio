package modelgateway

import (
	"net"
	"net/url"
	"path"
	"strings"
)

// ProviderKind identifies a provider deployment boundary. It is deliberately
// separate from protocol: two endpoints can both speak Anthropic while only a
// controlled company gateway is allowed to receive company-only routing
// headers.
type ProviderKind string

const (
	// ProviderKindStandard is the default for public and arbitrary endpoints.
	ProviderKindStandard ProviderKind = ""
	// ProviderKindSessionAffinity is an explicit opt-in for Anthropic-compatible
	// providers that accept a session-affinity header.
	ProviderKindSessionAffinity ProviderKind = "session_affinity"
)

// PromptCacheSessionAffinityAllowed is the single provider-boundary policy used
// by configuration validation and adapter assembly. A boolean opt-in alone is
// insufficient: only an explicitly configured Anthropic-compatible provider
// may receive the session-affinity header.
func PromptCacheSessionAffinityAllowed(kind ProviderKind, protocol, rawBaseURL string) bool {
	return kind == ProviderKindSessionAffinity &&
		strings.EqualFold(protocol, "anthropic") &&
		isSafeProviderEndpoint(rawBaseURL)
}

// IsKnownProviderKind reports whether the configuration value is supported.
func IsKnownProviderKind(kind ProviderKind) bool {
	return kind == ProviderKindStandard || kind == ProviderKindSessionAffinity
}

func isSafeProviderEndpoint(rawBaseURL string) bool {
	endpoint, err := url.Parse(rawBaseURL)
	if err != nil || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return false
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return false
	}
	host := endpoint.Hostname()
	if host == "" || (host != "localhost" && !net.ParseIP(host).IsLoopback()) {
		return false
	}
	return path.Clean(endpoint.Path) == "/open_api/anthropic"
}
