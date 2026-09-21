package observability

import (
	"context"
	"time"
)

type Span interface {
	End()
	AddEvent(name string, fields ...Field)
	RecordError(err error, fields ...Field)
	TraceContext() TraceContext
}

type TraceProvider interface {
	Start(ctx context.Context, name string, fields ...Field) (context.Context, Span)
}

type SpanSnapshot struct {
	Name      string
	StartTime time.Time
	EndTime   time.Time
	Fields    []Field
}
