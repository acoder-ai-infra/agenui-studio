package protocol

import (
	"context"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// ClientProtocolRequest is the negotiation input used by Registry.Select and
// ProtocolAdapter.Match.
type ClientProtocolRequest struct {
	Accept       string // HTTP Accept header, e.g. "text/event-stream"
	Protocol     string // explicit protocol id, e.g. "sse" | "http_json"
	Capabilities *ClientCapabilities
}

// ProtocolCapabilities describes what an adapter supports.
type ProtocolCapabilities struct {
	ID        string
	Streaming bool
	MediaType string
}

// ProtocolAdapter (extension §4.4) consumes canonical AgentEvent and produces a
// ProtocolFrame. Adapters never write back to the EventStore (conformance X-004).
type ProtocolAdapter interface {
	ID() string
	Capabilities(ctx context.Context) ProtocolCapabilities
	Match(ctx context.Context, req ClientProtocolRequest) bool
	Convert(ctx context.Context, event observability.AgentEvent, target ClientTarget) (ProtocolFrame, error)
	Validate(ctx context.Context, frame ProtocolFrame) error
}

// Registry registers the available protocols (P0 registers SSE + HTTP JSON).
type Registry interface {
	Register(a ProtocolAdapter)
	Select(ctx context.Context, req ClientProtocolRequest) (ProtocolAdapter, error)
}

// ErrNoAdapter is returned by Registry.Select when nothing matches.
var ErrNoAdapter = &ProtocolError{Code: "PROTOCOL_NOT_SUPPORTED", Message: "no protocol adapter matched the request"}

// ProtocolError is a protocol-layer error with a stable machine-readable code.
type ProtocolError struct {
	Code    string
	Message string
}

func (e *ProtocolError) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

// registry is the default in-memory Registry. Selection is first-match by
// registration order.
type registry struct {
	adapters []ProtocolAdapter
}

// NewRegistry builds an empty Registry.
func NewRegistry() Registry { return &registry{} }

func (r *registry) Register(a ProtocolAdapter) {
	if a == nil {
		return
	}
	r.adapters = append(r.adapters, a)
}

func (r *registry) Select(ctx context.Context, req ClientProtocolRequest) (ProtocolAdapter, error) {
	for _, a := range r.adapters {
		if a.Match(ctx, req) {
			return a, nil
		}
	}
	return nil, ErrNoAdapter
}
