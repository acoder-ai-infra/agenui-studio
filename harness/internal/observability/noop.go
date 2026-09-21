package observability

import (
	"context"
	"sync"
	"time"
)

type NoopTracer struct {
	serviceName string
	ids         IDGenerator
}

func NewNoopTracer(serviceName string) *NoopTracer {
	return &NoopTracer{
		serviceName: serviceName,
		ids:         NewULIDGenerator(""),
	}
}

func (t *NoopTracer) Start(ctx context.Context, name string, fields ...Field) (context.Context, Span) {
	tc := MustTraceContext(ctx)
	if tc.TraceID == "" {
		tc.TraceID = t.ids.NewTraceID()
		tc.RootSpanID = t.ids.NewSpanID()
	}
	parentSpanID := tc.SpanID
	spanID := t.ids.NewSpanID()
	tc.ParentSpanID = parentSpanID
	tc.SpanID = spanID
	if tc.RootSpanID == "" {
		tc.RootSpanID = spanID
	}
	ctx = WithTraceContext(ctx, tc)
	return ctx, &NoopSpan{
		name:      name,
		tc:        tc,
		startedAt: time.Now(),
		fields:    fields,
	}
}

type NoopSpan struct {
	mu        sync.Mutex
	name      string
	tc        TraceContext
	startedAt time.Time
	endedAt   time.Time
	fields    []Field
}

func (s *NoopSpan) End() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.endedAt.IsZero() {
		s.endedAt = time.Now()
	}
}

func (s *NoopSpan) AddEvent(_ string, _ ...Field) {}

func (s *NoopSpan) RecordError(_ error, _ ...Field) {}

func (s *NoopSpan) TraceContext() TraceContext {
	return s.tc
}
