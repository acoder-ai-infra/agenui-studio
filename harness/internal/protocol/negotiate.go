package protocol

import "context"

// Negotiator selects a protocol given client capabilities. P1 skeleton: P0 only
// distinguishes SSE vs HTTP JSON via the Registry; capability-driven negotiation
// (websocket / atoui / adaptive_card downgrade chains) is a P1 target
// (extension §4.4).
type Negotiator interface {
	Negotiate(ctx context.Context, caps ClientCapabilities) ProtocolDecision
}

// ProtocolDecision is the outcome of negotiation.
type ProtocolDecision struct {
	Protocol  string   // "sse" | "http_json" | "http_polling"
	Rendering string   // "markdown" | "atoui" | "adaptive_card" | ...
	Fallbacks []string // ordered downgrade chain applied to reach the decision
}

// DefaultNegotiator is the P0 skeleton: prefer SSE when supported, else HTTP
// polling; rendering defaults to markdown (the guaranteed floor of §7's chain).
type DefaultNegotiator struct{}

// Negotiate returns the P0 decision.
func (DefaultNegotiator) Negotiate(_ context.Context, caps ClientCapabilities) ProtocolDecision {
	d := ProtocolDecision{Protocol: "http_polling", Rendering: "markdown"}
	if caps.ProtocolSupport["sse"] {
		d.Protocol = "sse"
	}
	// Rendering downgrade chain (§7): atoui -> adaptive_card -> markdown.
	switch {
	case caps.RenderingSupport["atoui"]:
		d.Rendering = "atoui"
	case caps.RenderingSupport["adaptive_card"]:
		d.Rendering = "adaptive_card"
		d.Fallbacks = append(d.Fallbacks, "atoui->adaptive_card")
	default:
		d.Fallbacks = append(d.Fallbacks, "atoui->adaptive_card->markdown")
	}
	return d
}

var _ Negotiator = DefaultNegotiator{}
