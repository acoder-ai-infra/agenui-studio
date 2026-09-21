package agentregistry

import (
	"context"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func (s *Service) startSpan(ctx context.Context, name string, fields ...observability.Field) (context.Context, observability.Span) {
	if s.tracer == nil {
		s.tracer = observability.NewNoopTracer("agentregistry")
	}
	return s.tracer.Start(ctx, name, fields...)
}

func (s *Service) withAgentTrace(ctx context.Context, cfg AgentConfig) context.Context {
	ctx = observability.WithAgent(ctx, cfg.AgentID, cfg.AgentType, cfg.Version)
	return ctx
}

func (s *Service) logError(ctx context.Context, msg string, err error, fields ...observability.Field) {
	if s.logger == nil {
		s.logger = observability.NoopLogger{}
	}
	fields = append(fields, observability.String("error_code", string(errorCode(err))))
	s.logger.Error(ctx, msg, err, fields...)
}
