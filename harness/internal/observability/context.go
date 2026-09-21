package observability

import "context"

type TraceContext struct {
	TraceID      string `json:"trace_id"`
	RootSpanID   string `json:"root_span_id,omitempty"`
	SpanID       string `json:"span_id,omitempty"`
	ParentSpanID string `json:"parent_span_id,omitempty"`

	SessionID      string `json:"session_id,omitempty"`
	RunID          string `json:"run_id,omitempty"`
	ParentRunID    string `json:"parent_run_id,omitempty"`
	RootRunID      string `json:"root_run_id,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	RequestID      string `json:"request_id,omitempty"`

	UserID   string `json:"user_id,omitempty"`
	TenantID string `json:"tenant_id,omitempty"`

	AgentID      string `json:"agent_id,omitempty"`
	AgentType    string `json:"agent_type,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`
	Runtime      string `json:"runtime,omitempty"`

	Channel  string `json:"channel,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Source   string `json:"source,omitempty"`

	Sampled      bool              `json:"sampled"`
	DebugEnabled bool              `json:"debug_enabled"`
	Baggage      map[string]string `json:"baggage,omitempty"`
}

type traceContextKey struct{}

func WithTraceContext(ctx context.Context, tc TraceContext) context.Context {
	return context.WithValue(ctx, traceContextKey{}, tc)
}

func TraceContextFrom(ctx context.Context) (TraceContext, bool) {
	tc, ok := ctx.Value(traceContextKey{}).(TraceContext)
	return tc, ok
}

func MustTraceContext(ctx context.Context) TraceContext {
	tc, _ := TraceContextFrom(ctx)
	return tc
}

func WithRun(ctx context.Context, sessionID, runID string) context.Context {
	tc := MustTraceContext(ctx)
	tc.SessionID = sessionID
	tc.RunID = runID
	return WithTraceContext(ctx, tc)
}

func WithAgent(ctx context.Context, agentID, agentType, version string) context.Context {
	tc := MustTraceContext(ctx)
	tc.AgentID = agentID
	tc.AgentType = agentType
	tc.AgentVersion = version
	return WithTraceContext(ctx, tc)
}

func WithRequest(ctx context.Context, requestID string) context.Context {
	tc := MustTraceContext(ctx)
	tc.RequestID = requestID
	return WithTraceContext(ctx, tc)
}
