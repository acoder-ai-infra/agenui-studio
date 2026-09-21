package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge"
)

const defaultRetainRevisions = 3

type ManagerOptions struct {
	RetainRevisions int
}

type Manager struct {
	active atomic.Value

	mu     sync.RWMutex
	cache  map[string]*snapshot
	order  []string
	retain int
}

type snapshot struct {
	repository *designknowledge.Repository
	info       SnapshotInfo
}

func NewLocalFallbackManager(
	ctx context.Context,
	source fs.FS,
	revisionDir string,
	options ManagerOptions,
) (*Manager, error) {
	repository, err := designknowledge.Load(ctx, source, revisionDir)
	if err != nil {
		return nil, fmt.Errorf("design knowledge runtime: load local fallback: %w", err)
	}
	return NewManager(
		repository,
		SnapshotInfo{
			Version:    repository.RevisionID(),
			Source:     SourceLocalFallback,
			LoadedAt:   time.Now().UTC(),
			RevisionID: repository.RevisionID(),
		},
		options,
	)
}

func NewManager(
	repository *designknowledge.Repository,
	info SnapshotInfo,
	options ManagerOptions,
) (*Manager, error) {
	if repository == nil {
		return nil, errors.New("design knowledge runtime: fallback repository is required")
	}
	retain := options.RetainRevisions
	if retain <= 0 {
		retain = defaultRetainRevisions
	}
	manager := &Manager{
		cache:  make(map[string]*snapshot),
		retain: retain,
	}
	if err := manager.Activate(repository, info); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *Manager) Current() *designknowledge.Repository {
	current := m.currentSnapshot()
	if current == nil {
		return nil
	}
	return current.repository
}

func (m *Manager) CurrentInfo() SnapshotInfo {
	current := m.currentSnapshot()
	if current == nil {
		return SnapshotInfo{}
	}
	return current.info
}

func (m *Manager) Get(
	revisionID string,
	revisionHash string,
) (*designknowledge.Repository, bool) {
	if m == nil {
		return nil, false
	}
	key := snapshotKey(revisionID, revisionHash)
	if key == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	value := m.cache[key]
	if value == nil || value.repository == nil {
		return nil, false
	}
	return value.repository, true
}

func (m *Manager) Activate(
	repository *designknowledge.Repository,
	info SnapshotInfo,
) error {
	if m == nil {
		return errors.New("design knowledge runtime: manager is nil")
	}
	if repository == nil {
		return errors.New("design knowledge runtime: repository is required")
	}
	info = normalizeSnapshotInfo(repository, info)
	if info.RevisionID != repository.RevisionID() ||
		info.RevisionHash != repository.RevisionHash() {
		return fmt.Errorf(
			"design knowledge runtime: snapshot identity %s/%s does not match repository %s/%s",
			info.RevisionID,
			info.RevisionHash,
			repository.RevisionID(),
			repository.RevisionHash(),
		)
	}
	value := &snapshot{repository: repository, info: info}
	key := snapshotKey(info.RevisionID, info.RevisionHash)
	if key == "" {
		return errors.New("design knowledge runtime: snapshot identity is required")
	}

	m.mu.Lock()
	if _, exists := m.cache[key]; !exists {
		m.order = append(m.order, key)
	}
	m.cache[key] = value
	for len(m.order) > m.retain {
		delete(m.cache, m.order[0])
		m.order = m.order[1:]
	}
	m.mu.Unlock()

	m.active.Store(value)
	return nil
}

func (m *Manager) currentSnapshot() *snapshot {
	if m == nil {
		return nil
	}
	current, _ := m.active.Load().(*snapshot)
	return current
}

func normalizeSnapshotInfo(
	repository *designknowledge.Repository,
	info SnapshotInfo,
) SnapshotInfo {
	info.RevisionID = strings.TrimSpace(info.RevisionID)
	if info.RevisionID == "" {
		info.RevisionID = repository.RevisionID()
	}
	info.RevisionHash = strings.TrimSpace(info.RevisionHash)
	if info.RevisionHash == "" {
		info.RevisionHash = repository.RevisionHash()
	}
	info.Version = strings.TrimSpace(info.Version)
	if info.Version == "" {
		info.Version = info.RevisionID
	}
	info.Source = strings.TrimSpace(info.Source)
	if info.Source == "" {
		info.Source = SourceLocalFallback
	}
	if info.LoadedAt.IsZero() {
		info.LoadedAt = time.Now().UTC()
	}
	return info
}

func snapshotKey(revisionID string, revisionHash string) string {
	revisionID = strings.TrimSpace(revisionID)
	revisionHash = strings.TrimSpace(revisionHash)
	if revisionID == "" || revisionHash == "" {
		return ""
	}
	return revisionID + "\x00" + revisionHash
}
