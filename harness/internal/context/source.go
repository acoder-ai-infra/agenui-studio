package context

import (
	gocontext "context"
	"fmt"
	"sort"
)

// SourceKind identifies the type of a context source.
type SourceKind string

const (
	SourceSystemPrompt SourceKind = "system_prompt"
	SourceConversation SourceKind = "conversation"
	SourceToolResult   SourceKind = "tool_result"
	SourceWorkspace    SourceKind = "workspace"
	SourceRetrieval    SourceKind = "retrieval"
)

// Source is the unified interface for context data providers.
type Source interface {
	Kind() SourceKind
	Collect(ctx gocontext.Context, req CollectRequest) ([]ContextFragment, error)
}

type requiredSource interface {
	Required() bool
}

// CollectRequest is the input for Source.Collect.
type CollectRequest struct {
	SessionID   string
	Generation  int
	TokenBudget int
	Messages    []*Message
}

// SourceRegistry manages registered sources and collects fragments in order.
type SourceRegistry struct {
	sources []Source
	order   map[SourceKind]int
}

// NewSourceRegistry creates a registry with default collection ordering.
func NewSourceRegistry(sources ...Source) *SourceRegistry {
	return &SourceRegistry{
		sources: sources,
		order:   defaultSourceOrder(),
	}
}

func defaultSourceOrder() map[SourceKind]int {
	return map[SourceKind]int{
		SourceSystemPrompt: 0,
		SourceRetrieval:    1,
		SourceWorkspace:    2,
		SourceConversation: 3,
		SourceToolResult:   4,
	}
}

// CollectAll gathers fragments from all sources in order.
// A single source failure is logged but does not block others.
func (r *SourceRegistry) CollectAll(ctx gocontext.Context, req CollectRequest) ([]ContextFragment, error) {
	sorted := make([]Source, len(r.sources))
	copy(sorted, r.sources)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sourceRank(r.order, sorted[i].Kind()) < sourceRank(r.order, sorted[j].Kind())
	})

	var all []ContextFragment
	seenMessages := make(map[string]bool)
	for _, src := range sorted {
		frags, err := src.Collect(ctx, req)
		if err != nil {
			if required, ok := src.(requiredSource); ok && required.Required() {
				return nil, fmt.Errorf("%w: %s: %v", ErrSourceCollect, src.Kind(), err)
			}
			continue
		}
		for _, fragment := range frags {
			if len(fragment.Messages) > 0 && fragment.Messages[0].ID != "" {
				id := fragment.Messages[0].ID
				if seenMessages[id] {
					continue
				}
				seenMessages[id] = true
			}
			all = append(all, fragment)
		}
	}
	return all, nil
}

// Sources returns the registered sources.
func (r *SourceRegistry) Sources() []Source {
	return append([]Source(nil), r.sources...)
}

func sourceRank(order map[SourceKind]int, kind SourceKind) int {
	if rank, ok := order[kind]; ok {
		return rank
	}
	return 99
}
