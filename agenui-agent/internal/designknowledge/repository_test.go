package designknowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"testing"
	"testing/fstest"
)

func TestCatalogAndResolveDependencyClosure(t *testing.T) {
	t.Parallel()
	repository, err := Load(context.Background(), testRevision(t, nil), "revisions/r1")
	if err != nil {
		t.Fatal(err)
	}
	catalog := repository.Catalog(KindLayout)
	if len(catalog) != 1 || catalog[0].ID != "layout.parking" {
		t.Fatalf("Catalog(layout) = %#v", catalog)
	}
	documents, err := repository.Resolve(
		[]string{"layout.parking"},
		ResolveOptions{MaxDocuments: 2, ByteBudget: 4096},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := documentIDs(documents); len(got) != 2 ||
		got[0] != "layout.parking" || got[1] != "rule.foundation" {
		t.Fatalf("Resolve(layout.parking) = %#v", got)
	}
}

func TestResolveFailsClosedWhenMandatoryClosureExceedsBudget(t *testing.T) {
	t.Parallel()
	repository, err := Load(context.Background(), testRevision(t, nil), "revisions/r1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Resolve(
		[]string{"layout.parking"},
		ResolveOptions{MaxDocuments: 1, ByteBudget: 4096},
	); err == nil {
		t.Fatal("Resolve() silently omitted a mandatory dependency")
	}
	if _, err := repository.Resolve(
		[]string{"layout.parking"},
		ResolveOptions{MaxDocuments: 2, ByteBudget: 8},
	); err == nil {
		t.Fatal("Resolve() accepted an undersized byte budget")
	}
}

func TestLoadRejectsCorruptionAndInvalidReferences(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(fstest.MapFS)
	}{
		{
			name: "document hash",
			mutate: func(source fstest.MapFS) {
				source["revisions/r1/rules/foundation.md"] = &fstest.MapFile{Data: []byte("tampered")}
			},
		},
		{
			name: "index hash",
			mutate: func(source fstest.MapFS) {
				source["revisions/r1/index.json"] = &fstest.MapFile{Data: append(
					[]byte(nil), source["revisions/r1/index.json"].Data...,
				)}
				source["revisions/r1/index.json"].Data = append(
					source["revisions/r1/index.json"].Data,
					'\n',
				)
			},
		},
		{
			name: "path traversal",
			mutate: func(source fstest.MapFS) {
				rewriteIndex(t, source, func(index *Index) {
					index.Documents[0].ContentRef = "../secret.md"
				})
			},
		},
		{
			name: "missing reference",
			mutate: func(source fstest.MapFS) {
				rewriteIndex(t, source, func(index *Index) {
					index.Documents[1].References = []string{"rule.missing"}
				})
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			source := testRevision(t, tt.mutate)
			if _, err := Load(context.Background(), source, "revisions/r1"); err == nil {
				t.Fatal("Load() accepted invalid revision")
			}
		})
	}
}

func TestRepositoryConcurrentReadsAreImmutable(t *testing.T) {
	t.Parallel()
	repository, err := Load(context.Background(), testRevision(t, nil), "revisions/r1")
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 100; iteration++ {
				index := repository.Index()
				index.Documents[0].Tags[0] = "mutated"
				got, err := repository.Resolve(
					[]string{"layout.parking"},
					ResolveOptions{MaxDocuments: 2, ByteBudget: 4096},
				)
				if err != nil || len(got) != 2 || got[0].Metadata.Tags[0] == "mutated" {
					t.Errorf("repository leaked mutable state: %#v", got)
					return
				}
			}
		}()
	}
	wait.Wait()
}

func testRevision(t *testing.T, mutate func(fstest.MapFS)) fstest.MapFS {
	t.Helper()
	foundation := []byte("# 基础\n所有卡片遵循协议。")
	parking := []byte("# 停车列表\n停车场使用纵向列表。")
	index := Index{
		SchemaVersion: "design_knowledge_index.v1",
		RevisionID:    "r1",
		Documents: []Metadata{
			{
				ID: "rule.foundation", Kind: KindRule, Version: "1", Status: "enabled",
				Summary: "所有卡片的基础规则", Tags: []string{"基础"},
				ContentRef:  "rules/foundation.md",
				ContentHash: contentHash(foundation),
			},
			{
				ID: "layout.parking", Kind: KindLayout, Version: "1", Status: "enabled",
				Summary: "停车场推荐列表", Tags: []string{"停车", "停车场", "列表"},
				Requires: []string{"rule.foundation"}, ContentRef: "layouts/parking.md",
				ContentHash: contentHash(parking),
			},
		},
	}
	indexBytes, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := json.Marshal(Manifest{
		SchemaVersion: "design_knowledge_manifest.v1",
		RevisionID:    "r1", Status: "published", IndexRef: "index.json",
		IndexHash: contentHash(indexBytes),
	})
	if err != nil {
		t.Fatal(err)
	}
	source := fstest.MapFS{
		"revisions/r1/manifest.json":       &fstest.MapFile{Data: manifestBytes},
		"revisions/r1/index.json":          &fstest.MapFile{Data: indexBytes},
		"revisions/r1/rules/foundation.md": &fstest.MapFile{Data: foundation},
		"revisions/r1/layouts/parking.md":  &fstest.MapFile{Data: parking},
	}
	if mutate != nil {
		mutate(source)
	}
	return source
}

func rewriteIndex(t *testing.T, source fstest.MapFS, mutate func(*Index)) {
	t.Helper()
	var index Index
	if err := json.Unmarshal(source["revisions/r1/index.json"].Data, &index); err != nil {
		t.Fatal(err)
	}
	mutate(&index)
	raw, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	source["revisions/r1/index.json"] = &fstest.MapFile{Data: raw}
	var manifest Manifest
	if err := json.Unmarshal(source["revisions/r1/manifest.json"].Data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.IndexHash = contentHash(raw)
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	source["revisions/r1/manifest.json"] = &fstest.MapFile{Data: manifestRaw}
}

func contentHash(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func documentIDs(documents []Document) []string {
	result := make([]string, len(documents))
	for index := range documents {
		result[index] = documents[index].Metadata.ID
	}
	return result
}
