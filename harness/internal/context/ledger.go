package context

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// MessageLedger is the durable fact contract. It owns sequence allocation and
// idempotency. Implementations must never reorder or destructively truncate.
type MessageLedger interface {
	Append(ctx context.Context, sessionID string, msg Message) (Message, error)
	List(ctx context.Context, sessionID string, afterSequence int64, limit int) ([]Message, error)
	ListRecent(ctx context.Context, sessionID string, limit int) ([]Message, error)
	GetByIDs(ctx context.Context, sessionID string, ids []string) ([]Message, error)
}

// InMemoryMessageLedger is deterministic test infrastructure. Production must
// bind this port to an OLTP store with unique(session_id, sequence) and
// unique(session_id, idempotency_key) constraints.
type InMemoryMessageLedger struct {
	mu       sync.RWMutex
	messages map[string][]Message
	next     map[string]int64
	byKey    map[string]map[string]Message
	byID     map[string]map[string]Message
}

var _ MessageLedger = (*InMemoryMessageLedger)(nil)

func NewInMemoryMessageLedger() *InMemoryMessageLedger {
	return &InMemoryMessageLedger{
		messages: make(map[string][]Message),
		next:     make(map[string]int64),
		byKey:    make(map[string]map[string]Message),
		byID:     make(map[string]map[string]Message),
	}
}

func (l *InMemoryMessageLedger) Append(_ context.Context, sessionID string, msg Message) (Message, error) {
	if sessionID == "" {
		return Message{}, ErrSessionIDMissing
	}
	if msg.ID == "" {
		return Message{}, ErrMessageIDMissing
	}
	key := msg.IdempotencyKey
	if key == "" {
		key = msg.ID
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byKey[sessionID] == nil {
		l.byKey[sessionID] = make(map[string]Message)
		l.byID[sessionID] = make(map[string]Message)
	}
	if existing, ok := l.byKey[sessionID][key]; ok {
		candidate := canonicalMessageFact(sessionID, key, existing.Sequence, msg)
		candidate.Timestamp = existing.Timestamp
		if !reflect.DeepEqual(existing, candidate) {
			return Message{}, fmt.Errorf("%w: session=%s key=%s", ErrMessageConflict, sessionID, key)
		}
		return cloneMessageValue(existing), nil
	}
	if existing, ok := l.byID[sessionID][msg.ID]; ok {
		return Message{}, fmt.Errorf("%w: message_id=%s already uses key=%s", ErrMessageConflict, msg.ID, existing.IdempotencyKey)
	}

	l.next[sessionID]++
	stored := canonicalMessageFact(sessionID, key, l.next[sessionID], msg)
	l.messages[sessionID] = append(l.messages[sessionID], stored)
	l.byKey[sessionID][key] = stored
	l.byID[sessionID][stored.ID] = stored
	return cloneMessageValue(stored), nil
}

func (l *InMemoryMessageLedger) List(_ context.Context, sessionID string, afterSequence int64, limit int) ([]Message, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []Message
	for _, msg := range l.messages[sessionID] {
		if msg.Sequence <= afterSequence {
			continue
		}
		out = append(out, cloneMessageValue(msg))
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func (l *InMemoryMessageLedger) ListRecent(_ context.Context, sessionID string, limit int) ([]Message, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	messages := l.messages[sessionID]
	start := 0
	if limit > 0 && len(messages) > limit {
		start = len(messages) - limit
	}
	out := make([]Message, 0, len(messages)-start)
	for _, msg := range messages[start:] {
		out = append(out, cloneMessageValue(msg))
	}
	return out, nil
}

func (l *InMemoryMessageLedger) GetByIDs(_ context.Context, sessionID string, ids []string) ([]Message, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Message, 0, len(ids))
	for _, id := range ids {
		if msg, ok := l.byID[sessionID][id]; ok {
			out = append(out, cloneMessageValue(msg))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out, nil
}

func canonicalMessageFact(sessionID, key string, sequence int64, msg Message) Message {
	stored := cloneMessageValue(msg)
	stored.CurrentInput = false
	stored.SessionID = sessionID
	stored.IdempotencyKey = key
	stored.Sequence = sequence
	return stored
}

func cloneMessageValue(msg Message) Message {
	out := msg
	out.ToolCalls = append([]ToolCall(nil), msg.ToolCalls...)
	for i := range out.ToolCalls {
		if msg.ToolCalls[i].Arguments != nil {
			out.ToolCalls[i].Arguments = make(map[string]any, len(msg.ToolCalls[i].Arguments))
			for k, v := range msg.ToolCalls[i].Arguments {
				out.ToolCalls[i].Arguments[k] = cloneStructuredValue(v)
			}
		}
	}
	if msg.ToolResult != nil {
		result := *msg.ToolResult
		out.ToolResult = &result
	}
	if msg.Extra != nil {
		out.Extra = make(map[string]any, len(msg.Extra))
		for k, v := range msg.Extra {
			out.Extra[k] = cloneStructuredValue(v)
		}
	}
	return out
}

// Message arguments and metadata are JSON-shaped at the persistence boundary.
// Clone recursive containers so callers cannot mutate an already stored fact.
func cloneStructuredValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, item := range typed {
			cloned[key] = cloneStructuredValue(item)
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for i, item := range typed {
			cloned[i] = cloneStructuredValue(item)
		}
		return cloned
	case json.RawMessage:
		return append(json.RawMessage(nil), typed...)
	case []byte:
		return append([]byte(nil), typed...)
	default:
		return value
	}
}
