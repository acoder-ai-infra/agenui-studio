package app

import (
	"context"
	"database/sql"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
)

func TestBuildArtifactStoreSupportsBundledBackends(t *testing.T) {
	for _, test := range []struct {
		name string
		cfg  ArtifactConfig
	}{
		{name: "memory", cfg: ArtifactConfig{ObjectBackend: "memory", MetadataBackend: "memory"}},
		{name: "file", cfg: ArtifactConfig{ObjectBackend: "file", MetadataBackend: "memory", Root: t.TempDir(), DownloadBase: artifactDownloadBase}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := buildArtifactStore(&test.cfg, false, nil, defaultBuildDependencies())
			if err != nil {
				t.Fatal(err)
			}
			if store == nil {
				t.Fatal("artifact store is nil")
			}
		})
	}
}

func TestBuildArtifactStoreRejectsUnknownObjectBackend(t *testing.T) {
	cfg := ArtifactConfig{ObjectBackend: "cloud-specific", MetadataBackend: "memory"}
	if _, err := buildArtifactStore(&cfg, false, nil, defaultBuildDependencies()); err == nil {
		t.Fatal("expected unsupported object backend error")
	}
}

func TestBuildArtifactStoreUsesMySQLMetadata(t *testing.T) {
	db := &sql.DB{}
	deps := defaultBuildDependencies()
	deps.newArtifactMetadata = func(got *sql.DB) (artifact.MetadataStore, error) {
		if got != db {
			t.Fatalf("metadata database = %p, want %p", got, db)
		}
		return metastore.NewMemory(), nil
	}
	cfg := ArtifactConfig{ObjectBackend: "file", MetadataBackend: "mysql", Root: t.TempDir(), DownloadBase: artifactDownloadBase, DownloadTokenKey: string(make([]byte, artifactTokenSecretBytes))}
	store, err := buildArtifactStore(&cfg, true, db, deps)
	if err != nil {
		t.Fatal(err)
	}
	if store == nil {
		t.Fatal("artifact store is nil")
	}
	_ = context.Background()
}
