package operator

import (
	"context"
	"strings"
)

type Executor interface {
	Execute(
		ctx context.Context,
		detail OperatorDetail,
		value any,
		extra map[string]any,
	) ExecuteResult
}

type Registry struct {
	executors map[string]Executor
}

func NewRegistry() Registry {
	return Registry{executors: make(map[string]Executor)}
}

func DefaultRegistry() Registry {
	registry := NewRegistry()
	js := NewGojaExecutor(GojaExecutorConfig{})
	registry.Register("javascript", js)
	registry.Register("js", js)
	registry.Register("typescript", js)
	registry.Register("ts", js)
	registry.Register("builtin", SurfaceFetchBuiltinExecutor())
	return registry
}

func (r Registry) Register(language string, executor Executor) {
	if r.executors == nil || executor == nil {
		return
	}
	language = normalizeLanguage(language)
	if language == "" {
		return
	}
	r.executors[language] = executor
}

func (r Registry) Get(language string) (Executor, bool) {
	if r.executors == nil {
		return nil, false
	}
	executor, ok := r.executors[normalizeLanguage(language)]
	return executor, ok
}

func normalizeLanguage(language string) string {
	return strings.ToLower(strings.TrimSpace(language))
}
