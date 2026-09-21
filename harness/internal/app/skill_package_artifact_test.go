package app

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
	"github.com/AGenUI/agenui-studio/harness/internal/skill"
)

func TestSkillPackageArtifactStorePersistsOneIdempotentZip(t *testing.T) {
	objects := objectstore.NewMemory()
	metadata := metastore.NewMemory()
	artifacts := artifact.NewStore(artifact.StoreConfig{ObjectStore: objects, MetadataStore: metadata, MaxObjectBytes: skill.DefaultMaxPackageObjectBytes})
	packages := newSkillPackageArtifactStore(artifacts)
	want := []byte("whole immutable skill zip")
	request := skill.PackageObject{TenantID: "tenant-a", SkillID: "planner", Version: "1.0.0", Archive: want}

	first, err := packages.PutPackage(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := packages.PutPackage(context.Background(), request)
	if err != nil || second != first {
		t.Fatalf("idempotent put ref=%q first=%q err=%v", second, first, err)
	}
	conflict := request
	conflict.Archive = []byte("different immutable skill zip")
	if _, err := packages.PutPackage(context.Background(), conflict); !errors.Is(err, skill.ErrVersionConflict) {
		t.Fatalf("different content for same Skill version error=%v", err)
	}
	got, err := packages.GetPackage(context.Background(), "tenant-a", first)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("get package=%q err=%v", got, err)
	}

	ctx := artifact.ContextWithActor(context.Background(), artifact.Actor{TenantID: "tenant-a", Role: artifact.ActorAudit})
	items, err := artifacts.List(ctx, artifact.ListQuery{TenantID: "tenant-a", OwnerModule: artifact.OwnerModuleSkill})
	if err != nil || len(items) != 1 {
		t.Fatalf("skill artifacts=%#v err=%v", items, err)
	}
	if items[0].MimeType != "application/zip" || items[0].RetentionPolicy != artifact.RetentionAuditTTL || !items[0].ExpiresAt.IsZero() {
		t.Fatalf("skill artifact metadata=%#v", items[0])
	}
}
