package kernel

import (
	"errors"
	"sort"
	"time"
)

// ExtensionKind labels one of the eight canonical extension pipeline stages.
// The values are strings so composers and SDK adapters can share them without
// importing the public harness/extension package.
type ExtensionKind string

const (
	ExtIdentityResolver    ExtensionKind = "identity_resolver"
	ExtRunInitializer      ExtensionKind = "run_initializer"
	ExtContextContributor  ExtensionKind = "context_contributor"
	ExtInputNormalizer     ExtensionKind = "input_normalizer"
	ExtToolProvider        ExtensionKind = "tool_provider"
	ExtOutputValidator     ExtensionKind = "output_validator"
	ExtProtocolProjector   ExtensionKind = "protocol_projector"
	ExtEventObserver       ExtensionKind = "event_observer"
	ExtBeforeModelHook     ExtensionKind = "before_model_hook"
	ExtToolCallInterceptor ExtensionKind = "tool_call_interceptor"
)

// ExtensionFailurePolicy captures fail-closed vs fail-open decisions.
type ExtensionFailurePolicy string

const (
	FailClosed ExtensionFailurePolicy = "fail_closed"
	FailOpen   ExtensionFailurePolicy = "fail_open"
)

// ExtensionEntry is one frozen extension binding used by the kernel. Both the
// SDK-registered Go implementation and the YAML-declared policy are captured;
// composers project it into per-stage runtime pipelines during Build.
type ExtensionEntry struct {
	ID             string
	Kind           ExtensionKind
	Order          int
	Failure        ExtensionFailurePolicy
	Timeout        time.Duration
	Fingerprint    string
	Implementation any
}

// ExtensionCatalog is the read-only view the SDK hands to the composer. It is
// deterministic-ordered (Kind then Order then ID) so BuildReport listings are
// stable.
type ExtensionCatalog struct {
	entries []ExtensionEntry
}

// NewExtensionCatalog builds an ExtensionCatalog by deduplicating entries by
// ID+Kind and sorting them deterministically. Duplicate IDs within a single
// Kind are rejected fail-closed.
func NewExtensionCatalog(entries []ExtensionEntry) (*ExtensionCatalog, error) {
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			return nil, errors.New("kernel: extension entry ID is required")
		}
		if e.Kind == "" {
			return nil, errors.New("kernel: extension entry Kind is required")
		}
		key := string(e.Kind) + "\x00" + e.ID
		if _, exists := seen[key]; exists {
			return nil, errors.New("kernel: duplicate extension (Kind, ID): " + key)
		}
		seen[key] = struct{}{}
	}
	copyEntries := append([]ExtensionEntry(nil), entries...)
	sort.SliceStable(copyEntries, func(i, j int) bool {
		if copyEntries[i].Kind != copyEntries[j].Kind {
			return copyEntries[i].Kind < copyEntries[j].Kind
		}
		if copyEntries[i].Order != copyEntries[j].Order {
			return copyEntries[i].Order < copyEntries[j].Order
		}
		return copyEntries[i].ID < copyEntries[j].ID
	})
	return &ExtensionCatalog{entries: copyEntries}, nil
}

// Entries returns a deterministic-order copy of every entry.
func (c *ExtensionCatalog) Entries() []ExtensionEntry {
	if c == nil {
		return nil
	}
	out := make([]ExtensionEntry, len(c.entries))
	copy(out, c.entries)
	return out
}

// ByKind returns entries filtered to the given Kind, preserving relative order.
func (c *ExtensionCatalog) ByKind(kind ExtensionKind) []ExtensionEntry {
	if c == nil {
		return nil
	}
	out := make([]ExtensionEntry, 0, len(c.entries))
	for _, e := range c.entries {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// Find 按 (Kind, ID) 查找单个条目，供 agent 级扩展绑定按声明 ID 解析使用。
func (c *ExtensionCatalog) Find(kind ExtensionKind, id string) (ExtensionEntry, bool) {
	if c == nil {
		return ExtensionEntry{}, false
	}
	for _, e := range c.entries {
		if e.Kind == kind && e.ID == id {
			return e, true
		}
	}
	return ExtensionEntry{}, false
}

// PhaseOfKernel returns a monotonic phase index for known ExtensionKind
// values so composers can enforce the fixed pipeline order documented in
// the public SDK contract Unknown kinds return 0 (fail-closed).
func PhaseOfKernel(k ExtensionKind) int {
	switch k {
	case ExtIdentityResolver:
		return 1
	case ExtRunInitializer:
		return 2
	case ExtContextContributor:
		return 3
	case ExtInputNormalizer:
		return 4
	case ExtToolProvider:
		return 5
	case ExtOutputValidator:
		return 6
	case ExtProtocolProjector:
		return 7
	case ExtEventObserver:
		return 8
	case ExtBeforeModelHook:
		return 9
	case ExtToolCallInterceptor:
		return 10
	}
	return 0
}
