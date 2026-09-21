package protocol

import (
	"html"
	"net/url"
	"strings"
)

// FallbackAppliedMarker is the marker recorded when a protocol degradation was
// applied. Upstream may emit it as an AgentEvent (fallback_applied); the protocol
// layer records it on the fallback result so the frame can carry a hint.
const FallbackAppliedMarker = "protocol_fallback_applied"

// DefaultURLAllowlist is the P0 URL/deeplink scheme allowlist. Anything outside
// this set is neutralized during sanitize (P3-D3: Protocol does minimal
// mechanical sanitize; deep content safety is deferred to Guardrail / P1).
var DefaultURLAllowlist = []string{"http", "https", "mailto", "tel"}

// FallbackResult is the outcome of a markdown fallback.
type FallbackResult struct {
	// Content is the sanitized markdown text.
	Content string
	// Applied is true when a structural degradation and/or sanitize was applied.
	Applied bool
	// Reason is a short machine-readable reason for the fallback.
	Reason string
}

// MarkdownFallback degrades a rich frame to safe markdown text (canonical §5:
// AToUI -> Adaptive Card -> Markdown). P0 does Markdown fallback + minimal
// sanitize only; structured card / schema validation is P1. The returned result
// is flagged Applied so callers can attach protocol_fallback_applied.
func MarkdownFallback(raw string, reason string, allowlist []string) FallbackResult {
	if allowlist == nil {
		allowlist = DefaultURLAllowlist
	}
	return FallbackResult{
		Content: SanitizeMarkdown(raw, allowlist),
		Applied: true,
		Reason:  reason,
	}
}

// SanitizeMarkdown applies the P0 minimal mechanical sanitize: HTML-escape the
// text and neutralize disallowed URL/deeplink schemes inside markdown links.
// This is NOT semantic content safety (PII / over-privileged actions) — that is
// deferred to Guardrail (P1).
func SanitizeMarkdown(s string, allowlist []string) string {
	s = sanitizeMarkdownLinks(s, allowlist)
	return html.EscapeString(s)
}

// SanitizeURL returns (url, true) when raw uses an allowed scheme (or is a
// scheme-relative / relative reference), else ("", false).
func SanitizeURL(raw string, allowlist []string) (string, bool) {
	if allowlist == nil {
		allowlist = DefaultURLAllowlist
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", false
	}
	if u.Scheme == "" {
		// relative / anchor / scheme-relative reference: allowed.
		return trimmed, true
	}
	scheme := strings.ToLower(u.Scheme)
	for _, allowed := range allowlist {
		if scheme == strings.ToLower(allowed) {
			return trimmed, true
		}
	}
	return "", false
}

// sanitizeMarkdownLinks rewrites markdown links [text](url) whose URL uses a
// disallowed scheme, dropping the link and keeping the visible text.
func sanitizeMarkdownLinks(s string, allowlist []string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '[' {
			if closeText := strings.IndexByte(s[i:], ']'); closeText > 0 {
				textEnd := i + closeText
				if textEnd+1 < len(s) && s[textEnd+1] == '(' {
					if closeURL := strings.IndexByte(s[textEnd+1:], ')'); closeURL > 0 {
						urlEnd := textEnd + 1 + closeURL
						text := s[i+1 : textEnd]
						rawURL := s[textEnd+2 : urlEnd]
						if safe, ok := SanitizeURL(rawURL, allowlist); ok {
							b.WriteString("[")
							b.WriteString(text)
							b.WriteString("](")
							b.WriteString(safe)
							b.WriteString(")")
						} else {
							// Drop the disallowed link; keep the visible text.
							b.WriteString(text)
						}
						i = urlEnd + 1
						continue
					}
				}
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
