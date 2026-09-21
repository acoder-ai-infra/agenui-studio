package runtime

import (
	"errors"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge"
)

type RepositoryProvider interface {
	Current() *designknowledge.Repository
	CurrentInfo() SnapshotInfo
	Get(revisionID, revisionHash string) (*designknowledge.Repository, bool)
}

type SnapshotInfo struct {
	Version      string
	RevisionID   string
	RevisionHash string
	Source       string
	ArtifactPath string
	LoadedAt     time.Time
}

const SourceLocalFallback = "local_fallback"

func CurrentSnapshot(provider RepositoryProvider) (*designknowledge.Repository, SnapshotInfo) {
	if provider == nil {
		return nil, SnapshotInfo{}
	}
	info := provider.CurrentInfo()
	if repository, ok := provider.Get(info.RevisionID, info.RevisionHash); ok {
		return repository, info
	}
	repository := provider.Current()
	if repository == nil {
		return nil, info
	}
	if info.RevisionID == "" {
		info.RevisionID = repository.RevisionID()
	}
	if info.RevisionHash == "" {
		info.RevisionHash = repository.RevisionHash()
	}
	if info.Version == "" {
		info.Version = info.RevisionID
	}
	return repository, info
}

type StaticProvider struct {
	repository *designknowledge.Repository
	info       SnapshotInfo
}

func NewStaticProvider(repository *designknowledge.Repository) (*StaticProvider, error) {
	if repository == nil {
		return nil, errors.New("design knowledge provider: repository is required")
	}
	revisionID := repository.RevisionID()
	return &StaticProvider{
		repository: repository,
		info: SnapshotInfo{
			Version:      revisionID,
			RevisionID:   revisionID,
			RevisionHash: repository.RevisionHash(),
			Source:       SourceLocalFallback,
			LoadedAt:     time.Now().UTC(),
		},
	}, nil
}

func MustStaticProvider(repository *designknowledge.Repository) RepositoryProvider {
	provider, err := NewStaticProvider(repository)
	if err != nil {
		panic(err)
	}
	return provider
}

func (p *StaticProvider) Current() *designknowledge.Repository {
	if p == nil {
		return nil
	}
	return p.repository
}

func (p *StaticProvider) CurrentInfo() SnapshotInfo {
	if p == nil {
		return SnapshotInfo{}
	}
	return p.info
}

func (p *StaticProvider) Get(
	revisionID string,
	revisionHash string,
) (*designknowledge.Repository, bool) {
	if p == nil || p.repository == nil {
		return nil, false
	}
	revisionID = strings.TrimSpace(revisionID)
	revisionHash = strings.TrimSpace(revisionHash)
	if revisionID == "" || revisionHash == "" {
		return nil, false
	}
	if p.repository.RevisionID() != revisionID ||
		p.repository.RevisionHash() != revisionHash {
		return nil, false
	}
	return p.repository, true
}
