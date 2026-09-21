package context

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Snapshot is the immutable factual boundary used for one model-context build.
// It stores refs and a content hash; messages remain owned by MessageLedger.
type Snapshot struct {
	SchemaVersion string            `json:"schema_version"`
	ID            string            `json:"id"`
	SessionID     string            `json:"session_id"`
	RunID         string            `json:"run_id"`
	MessageIDs    []string          `json:"message_ids"`
	Fragments     []ContextFragment `json:"fragments,omitempty"`
	Attachments   []AttachmentFact  `json:"attachments,omitempty"`
	LastSequence  int64             `json:"last_sequence"`
	Source        string            `json:"source"`
	HotVersion    int64             `json:"hot_version,omitempty"`
	ContentHash   string            `json:"content_hash"`
	CreatedAt     time.Time         `json:"created_at"`
}

const SnapshotSchemaVersion = "harness.context_snapshot.v1"

// HotContextView is the low-latency projection maintained by the storage
// write path. Fragments carry rolling summaries and other already-governed
// context; complete messages still resolve from the durable ledger by ID.
type HotContextView struct {
	Messages  []Message
	Fragments []ContextFragment
	Version   int64
}

type HotContextReader interface {
	Load(ctx context.Context, sessionID string, maxMessages int) (HotContextView, error)
}

type SnapshotStore interface {
	Save(ctx context.Context, snapshot Snapshot) (snapshotRef string, err error)
	Load(ctx context.Context, snapshotID string) (Snapshot, error)
}

type InMemorySnapshotStore struct {
	mu        sync.RWMutex
	snapshots map[string]Snapshot
}

var _ SnapshotStore = (*InMemorySnapshotStore)(nil)

func NewInMemorySnapshotStore() *InMemorySnapshotStore {
	return &InMemorySnapshotStore{snapshots: make(map[string]Snapshot)}
}

func (s *InMemorySnapshotStore) Save(_ context.Context, snapshot Snapshot) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.snapshots[snapshot.ID]; exists {
		return "", fmt.Errorf("snapshot already exists: %s", snapshot.ID)
	}
	snapshot.MessageIDs = append([]string(nil), snapshot.MessageIDs...)
	snapshot.Fragments = cloneFragments(snapshot.Fragments)
	s.snapshots[snapshot.ID] = snapshot
	return snapshot.ID, nil
}

func (s *InMemorySnapshotStore) Load(_ context.Context, snapshotID string) (Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot, ok := s.snapshots[snapshotID]
	if !ok {
		return Snapshot{}, ErrSnapshotNotFound
	}
	snapshot.MessageIDs = append([]string(nil), snapshot.MessageIDs...)
	snapshot.Fragments = cloneFragments(snapshot.Fragments)
	return snapshot, nil
}

type SnapshotManager struct {
	Ledger      MessageLedger
	Hot         HotContextReader
	Store       SnapshotStore
	IDs         func() string
	Clock       func() time.Time
	MaxMessages int
}

func (m SnapshotManager) Materialize(ctx context.Context, sessionID, runID string) (Snapshot, error) {
	return m.MaterializeWithAttachments(ctx, sessionID, runID, nil)
}

func (m SnapshotManager) MaterializeWithAttachments(ctx context.Context, sessionID, runID string, attachments []AttachmentFact) (Snapshot, error) {
	if m.Ledger == nil || m.Store == nil || m.IDs == nil {
		return Snapshot{}, errorsConfiguration("snapshot manager dependencies")
	}
	maxMessages := m.MaxMessages
	if maxMessages <= 0 {
		maxMessages = 200
	}
	if sessionID == "" {
		return Snapshot{}, ErrSessionIDMissing
	}
	var messages []Message
	var fragments []ContextFragment
	source := "message_ledger_fallback"
	var hotVersion int64
	if m.Hot != nil {
		view, err := m.Hot.Load(ctx, sessionID, maxMessages)
		switch {
		case err == nil:
			messages = cloneMessageSlice(view.Messages)
			fragments = cloneFragments(view.Fragments)
			hotVersion = view.Version
			source = "hot_context_cache"
		case !errors.Is(err, ErrHotContextMiss):
			return Snapshot{}, err
		}
	}
	if source != "hot_context_cache" {
		var err error
		messages, err = m.Ledger.ListRecent(ctx, sessionID, maxMessages)
		if err != nil {
			return Snapshot{}, err
		}
	}
	sort.SliceStable(messages, func(i, j int) bool { return messages[i].Sequence < messages[j].Sequence })
	attachments = cloneAttachments(attachments)
	ids := make([]string, 0, len(messages))
	var lastSequence int64
	for _, msg := range messages {
		ids = append(ids, msg.ID)
		lastSequence = msg.Sequence
	}
	createdAt := time.Now()
	if m.Clock != nil {
		createdAt = m.Clock()
	}
	snapshotID := m.IDs()
	if snapshotID == "" {
		return Snapshot{}, errorsConfiguration("snapshot id")
	}
	snapshot := Snapshot{
		SchemaVersion: SnapshotSchemaVersion,
		ID:            snapshotID,
		SessionID:     sessionID,
		RunID:         runID,
		MessageIDs:    ids,
		Fragments:     fragments,
		Attachments:   attachments,
		LastSequence:  lastSequence,
		Source:        source,
		HotVersion:    hotVersion,
		ContentHash:   hashSnapshotFacts(messages, fragments, attachments),
		CreatedAt:     createdAt,
	}
	ref, err := m.Store.Save(ctx, snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	if ref == "" {
		return Snapshot{}, errorsConfiguration("snapshot ref")
	}
	snapshot.ID = ref
	return snapshot, nil
}

// MaterializeDerived creates an immutable snapshot for a child execution from
// already-governed fragments. It deliberately does not inherit the parent
// conversation ledger; the task description remains the child Run input.
func (m SnapshotManager) MaterializeDerived(ctx context.Context, sessionID, runID, source string, fragments []ContextFragment) (Snapshot, error) {
	if m.Store == nil || m.IDs == nil {
		return Snapshot{}, errorsConfiguration("derived snapshot manager dependencies")
	}
	if sessionID == "" || runID == "" {
		return Snapshot{}, errorsConfiguration("derived snapshot identity")
	}
	if source == "" {
		source = "derived_run"
	}
	createdAt := time.Now()
	if m.Clock != nil {
		createdAt = m.Clock()
	}
	id := m.IDs()
	if id == "" {
		return Snapshot{}, errorsConfiguration("snapshot id")
	}
	fragments = cloneFragments(fragments)
	snapshot := Snapshot{
		SchemaVersion: SnapshotSchemaVersion, ID: id, SessionID: sessionID, RunID: runID, Fragments: fragments,
		Source: source, ContentHash: hashSnapshotFacts(nil, fragments, nil), CreatedAt: createdAt,
	}
	ref, err := m.Store.Save(ctx, snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	if ref == "" {
		return Snapshot{}, errorsConfiguration("snapshot ref")
	}
	snapshot.ID = ref
	return snapshot, nil
}

func (m SnapshotManager) Resolve(ctx context.Context, snapshotID string) (Snapshot, []Message, error) {
	if m.Ledger == nil || m.Store == nil {
		return Snapshot{}, nil, errorsConfiguration("snapshot manager dependencies")
	}
	snapshot, err := m.Store.Load(ctx, snapshotID)
	if err != nil {
		return Snapshot{}, nil, err
	}
	messages, err := m.Ledger.GetByIDs(ctx, snapshot.SessionID, snapshot.MessageIDs)
	if err != nil {
		return Snapshot{}, nil, err
	}
	actualHash := hashSnapshotFacts(messages, snapshot.Fragments, snapshot.Attachments)
	if len(messages) != len(snapshot.MessageIDs) || actualHash != snapshot.ContentHash {
		return Snapshot{}, nil, fmt.Errorf("snapshot content mismatch: %s expected=%s actual=%s", snapshotID, snapshot.ContentHash, actualHash)
	}
	return snapshot, messages, nil
}

func hashSnapshotFacts(messages []Message, fragments []ContextFragment, attachments []AttachmentFact) string {
	// Empty fact collections have one canonical representation. Storage
	// adapters commonly return an allocated empty slice even when the snapshot
	// was created from nil; that representation difference must not invalidate
	// an otherwise immutable snapshot.
	if len(messages) == 0 {
		messages = nil
	}
	if len(fragments) == 0 {
		fragments = nil
	}
	if len(attachments) == 0 {
		attachments = nil
	}
	data, _ := json.Marshal(struct {
		Messages    []Message
		Fragments   []ContextFragment
		Attachments []AttachmentFact
	}{messages, fragments, attachments})
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func cloneMessageSlice(messages []Message) []Message {
	if messages == nil {
		return nil
	}
	out := make([]Message, len(messages))
	for i, message := range messages {
		out[i] = cloneMessageValue(message)
	}
	return out
}

func cloneAttachments(attachments []AttachmentFact) []AttachmentFact {
	if attachments == nil {
		return nil
	}
	out := make([]AttachmentFact, len(attachments))
	copy(out, attachments)
	return out
}

func errorsConfiguration(name string) error {
	return fmt.Errorf("context configuration missing: %s", name)
}
