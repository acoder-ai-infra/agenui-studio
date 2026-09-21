package observability

import (
	"context"

	"go.opentelemetry.io/otel"
	oteltrace "go.opentelemetry.io/otel/trace"
)

type OTelTracer struct {
	tracer oteltrace.Tracer
	ids    IDGenerator
}

func NewOTelTracer(serviceName string) *OTelTracer {
	return &OTelTracer{
		tracer: otel.Tracer(serviceName),
		ids:    NewULIDGenerator(""),
	}
}

func (t *OTelTracer) Start(ctx context.Context, name string, fields ...Field) (context.Context, Span) {
	tc := MustTraceContext(ctx)
	ctx, span := t.tracer.Start(ctx, name)
	sc := span.SpanContext()
	if sc.IsValid() {
		tc.TraceID = sc.TraceID().String()
		tc.SpanID = sc.SpanID().String()
		if tc.RootSpanID == "" {
			tc.RootSpanID = tc.SpanID
		}
	} else {
		if tc.TraceID == "" {
			tc.TraceID = t.ids.NewTraceID()
		}
		parentSpanID := tc.SpanID
		tc.ParentSpanID = parentSpanID
		tc.SpanID = t.ids.NewSpanID()
		if tc.RootSpanID == "" {
			tc.RootSpanID = tc.SpanID
		}
	}
	ctx = WithTraceContext(ctx, tc)
	return ctx, &OTelSpan{span: span, tc: tc}
}

type OTelSpan struct {
	span oteltrace.Span
	tc   TraceContext
}

func (s *OTelSpan) End() {
	s.span.End()
}

func (s *OTelSpan) AddEvent(name string, _ ...Field) {
	s.span.AddEvent(name)
}

func (s *OTelSpan) RecordError(err error, _ ...Field) {
	s.span.RecordError(err)
}

func (s *OTelSpan) TraceContext() TraceContext {
	return s.tc
}
