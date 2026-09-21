package context

// RuntimeContext is a facade combining all context subsystems.
// It serves as the single entry point for session-scoped context management.
type RuntimeContext struct {
	Sessions SessionStore
	Timeline Timeline
	States   StateStore
	Events   EventBus
	Memory   MemoryStore
}

// NewRuntimeContext creates a fully wired RuntimeContext.
func NewRuntimeContext(
	sessions SessionStore,
	timeline Timeline,
	states StateStore,
	events EventBus,
	memory MemoryStore,
) *RuntimeContext {
	return &RuntimeContext{
		Sessions: sessions,
		Timeline: timeline,
		States:   states,
		Events:   events,
		Memory:   memory,
	}
}

// NewLiteRuntimeContext creates an in-memory RuntimeContext for testing and prototyping.
func NewLiteRuntimeContext() *RuntimeContext {
	return &RuntimeContext{
		Sessions: NewInMemorySessionStore(),
		Timeline: NewInMemoryTimeline(),
		States:   NewInMemoryStateStore(nil, nil),
		Events:   NewInMemoryEventBus(),
		Memory:   NewInMemoryMemoryStore(),
	}
}
