package observability

import "context"

type NoopLogger struct{}

func (NoopLogger) Debug(context.Context, string, ...Field)        {}
func (NoopLogger) Info(context.Context, string, ...Field)         {}
func (NoopLogger) Warn(context.Context, string, ...Field)         {}
func (NoopLogger) Error(context.Context, string, error, ...Field) {}
func (NoopLogger) With(...Field) StructuredLogger                 { return NoopLogger{} }
func (NoopLogger) Sync()                                          {}
