package ruleworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge/documentcontract"
)

const baseRevisionDir = "../../configs/design-public/revisions/demo-v1"

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}

func TestNextRevisionID(t *testing.T) {
	got, err := NextRevisionID("revision-4")
	if err != nil || got != "revision-5" {
		t.Fatalf("NextRevisionID(revision-4) = %q, %v; want revision-5", got, err)
	}
	if _, err := NextRevisionID("no-number"); err == nil {
		t.Fatalf("expected error for id without trailing integer")
	}
}

func TestMaterializeDocumentContractsRendersAndRejectsLegacyInEnforceMode(t *testing.T) {
	delta := RevisionDelta{NewDocuments: []NewDocument{{
		Kind: "rule", Summary: "间距规则", Document: &documentcontract.Document{
			DocumentSchema: documentcontract.RuleSchema, ID: "rule.spacing.card", Version: "1.0.0",
			Kind: "rule", Title: "卡片间距", Summary: "卡片内容区间距规则",
			Rules: []documentcontract.AtomicRule{{ID: "rule.spacing.card.padding", Target: "card", Strength: documentcontract.StrengthRequired, Effect: "内容区四周保留 32px 内边距", SourceTrace: []string{"rules.md#spacing"}}},
		},
	}}}
	if err := MaterializeDocumentContracts(&delta, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(delta.NewDocuments[0].Content, "agenui-document-contract") || delta.NewDocuments[0].ID != "rule.spacing.card" {
		t.Fatalf("materialized document=%+v", delta.NewDocuments[0])
	}
	legacy := RevisionDelta{NewDocuments: []NewDocument{{Kind: "rule", Summary: "legacy", Content: "# legacy"}}}
	if err := MaterializeDocumentContracts(&legacy, true); err == nil {
		t.Fatal("expected review mode to reject free-form Markdown")
	}
}

func TestMarshalCanonicalNoHTMLEscape(t *testing.T) {
	raw, err := marshalCanonical(map[string]string{"html": "<div>&</div>"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("<div>&</div>")) {
		t.Fatalf("expected unescaped HTML, got %s", raw)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		t.Fatalf("expected trailing newline")
	}
	if bytes.Contains(raw, []byte("\\u003c")) || bytes.Contains(raw, []byte("\\u0026")) {
		t.Fatalf("HTML must not be unicode-escaped: %s", raw)
	}
}

func TestGenerateRevisionAppendsLayoutAndPublishes(t *testing.T) {
	if _, err := os.Stat(baseRevisionDir); err != nil {
		t.Skipf("base revision not available: %v", err)
	}
	ctx := context.Background()
	outDir := filepath.Join(t.TempDir(), "revision-5")

	delta := RevisionDelta{NewDocuments: []NewDocument{
		{
			Kind: "layout",
			ID:   "layout.circle-grid", RelPath: "layouts/circle-grid.md",
			Summary: "环形宫格布局",
			Content: "# P?? 环形宫格\n\n新增布局占位内容。\n",
		},
	}}

	assigned, err := GenerateRevision(ctx, baseRevisionDir, outDir, "revision-5", delta)
	if err != nil {
		t.Fatalf("GenerateRevision: %v", err)
	}
	if len(assigned) != 1 || assigned[0] != "layout.circle-grid" {
		t.Fatalf("assigned layouts = %v, want [layout.circle-grid]", assigned)
	}

	// The new layout file exists.
	if _, err := os.Stat(filepath.Join(outDir, "layouts", "circle-grid.md")); err != nil {
		t.Fatalf("expected layouts/circle-grid.md: %v", err)
	}
	// Existing semantic layout documents are carried forward unchanged.
	if _, err := os.Stat(filepath.Join(outDir, "layouts", "summary-card.md")); err != nil {
		t.Fatalf("expected carried-forward layouts/summary-card.md: %v", err)
	}

	// The public baseline has no process audit payload; generated revisions do
	// not invent one. Deployment-specific profiles remain excluded as well.
	if _, err := os.Stat(filepath.Join(outDir, "audit")); !os.IsNotExist(err) {
		t.Fatalf("expected audit/ to be absent, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "profiles")); !os.IsNotExist(err) {
		t.Fatalf("expected profiles/ to be absent from generated revision, stat err=%v", err)
	}
	if strings.Contains(string(mustRead(t, filepath.Join(outDir, "index.json"))), `"kind": "profile"`) {
		t.Fatalf("index.json must not contain profile documents")
	}

	// manifest bumped + published; index self-consistent (GenerateRevision already
	// ran designknowledge.Load, but assert the observable facts too).
	manifestRaw, _ := os.ReadFile(filepath.Join(outDir, "manifest.json"))
	if !strings.Contains(string(manifestRaw), `"revision_id": "revision-5"`) {
		t.Fatalf("manifest revision_id not bumped: %s", manifestRaw)
	}
	if !strings.Contains(string(manifestRaw), `"status": "published"`) {
		t.Fatalf("manifest not published: %s", manifestRaw)
	}

	// content_hash for the new layout equals sha256 of its file bytes.
	indexRaw, _ := os.ReadFile(filepath.Join(outDir, "index.json"))
	fileBytes, _ := os.ReadFile(filepath.Join(outDir, "layouts", "circle-grid.md"))
	sum := sha256.Sum256(fileBytes)
	wantHash := "sha256:" + hex.EncodeToString(sum[:])
	if !strings.Contains(string(indexRaw), wantHash) {
		t.Fatalf("index.json missing correct content_hash %s for circle-grid.md", wantHash)
	}
}

func TestGenerateRevisionDerivesMissingNewDocumentSummary(t *testing.T) {
	if _, err := os.Stat(baseRevisionDir); err != nil {
		t.Skipf("base revision not available: %v", err)
	}
	outDir := filepath.Join(t.TempDir(), "revision-5")
	if _, err := GenerateRevision(context.Background(), baseRevisionDir, outDir, "revision-5", RevisionDelta{
		NewDocuments: []NewDocument{{
			Kind: "layout", ID: "layout.summary-fallback", RelPath: "layouts/summary-fallback.md",
			Content: "# 自动派生的摘要\n\n正文。\n",
		}},
	}); err != nil {
		t.Fatalf("GenerateRevision with missing summary: %v", err)
	}
	var index designknowledge.Index
	if err := json.Unmarshal(mustRead(t, filepath.Join(outDir, "index.json")), &index); err != nil {
		t.Fatal(err)
	}
	for _, document := range index.Documents {
		if document.ID == "layout.summary-fallback" {
			if document.Summary != "自动派生的摘要" {
				t.Fatalf("derived summary=%q", document.Summary)
			}
			return
		}
	}
	t.Fatal("generated layout metadata not found")
}

func TestGenerateRevisionAssignsMissingRuleIdentityWithoutOverwrite(t *testing.T) {
	if _, err := os.Stat(baseRevisionDir); err != nil {
		t.Skipf("base revision not available: %v", err)
	}
	outDir := filepath.Join(t.TempDir(), "revision-5")
	if _, err := GenerateRevision(context.Background(), baseRevisionDir, outDir, "revision-5", RevisionDelta{
		NewDocuments: []NewDocument{{
			Kind: "rule", Summary: "购买价格规则",
			Content: "# pre-trip-purchase-price\n\n购买价格展示约束。\n",
		}},
	}); err != nil {
		t.Fatalf("GenerateRevision with missing rule identity: %v", err)
	}
	var index designknowledge.Index
	if err := json.Unmarshal(mustRead(t, filepath.Join(outDir, "index.json")), &index); err != nil {
		t.Fatal(err)
	}
	for _, document := range index.Documents {
		if document.ID == "rule.pre-trip-purchase-price" {
			if document.ContentRef != "rules/pre-trip-purchase-price.md" {
				t.Fatalf("generated content_ref=%q", document.ContentRef)
			}
			return
		}
	}
	t.Fatal("generated rule metadata not found")
}

func TestGenerateRevisionRejectsNewDocumentPathTraversal(t *testing.T) {
	if _, err := os.Stat(baseRevisionDir); err != nil {
		t.Skipf("base revision not available: %v", err)
	}
	_, err := GenerateRevision(context.Background(), baseRevisionDir, filepath.Join(t.TempDir(), "revision-5"), "revision-5", RevisionDelta{
		NewDocuments: []NewDocument{{
			ID: "rule.escape", Kind: "rule", Summary: "非法路径", RelPath: "../escape.md", Content: "# escape\n",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "must stay inside") {
		t.Fatalf("path traversal error=%v", err)
	}
}

func TestGenerateRevisionAppliesModifiedDocument(t *testing.T) {
	if _, err := os.Stat(baseRevisionDir); err != nil {
		t.Skipf("base revision not available: %v", err)
	}
	ctx := context.Background()

	// Pick a real existing document (id + content_ref) from the base index.
	var idx struct {
		Documents []struct {
			ID         string `json:"id"`
			ContentRef string `json:"content_ref"`
		} `json:"documents"`
	}
	if err := json.Unmarshal(mustRead(t, filepath.Join(baseRevisionDir, "index.json")), &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Documents) == 0 {
		t.Fatal("base index has no documents")
	}
	target := idx.Documents[0]

	outDir := filepath.Join(t.TempDir(), "revision-5")
	const newContent = "# MODIFIED BY TEST\n\n覆盖后的内容。\n"
	if _, err := GenerateRevision(ctx, baseRevisionDir, outDir, "revision-5", RevisionDelta{
		ModifiedDocuments: []ModifiedDocument{{ID: target.ID, Content: newContent}},
	}); err != nil {
		t.Fatalf("GenerateRevision with modified doc: %v", err)
	}
	got := string(mustRead(t, filepath.Join(outDir, filepath.FromSlash(target.ContentRef))))
	if got != newContent {
		t.Fatalf("modified doc %s content = %q, want overwritten", target.ID, got)
	}

	// An unknown id must be rejected.
	if _, err := GenerateRevision(ctx, baseRevisionDir, filepath.Join(t.TempDir(), "revision-5b"), "revision-5", RevisionDelta{
		ModifiedDocuments: []ModifiedDocument{{ID: "layout.pZZ.nope", Content: "x"}},
	}); err == nil {
		t.Fatalf("expected error for unknown modified doc id")
	}
}

// TestGoPublisherMatchesNodeCanonical proves the Go publisher is byte-identical
// to tools/designknowledge/publish.mjs: after Go publishes a revision, running
// the canonical node publisher over the same directory must leave index_hash
// unchanged (it recomputes content_hash from identical files and re-serialises
// the index the same way). Skipped when node is unavailable.
func TestGoPublisherMatchesNodeCanonical(t *testing.T) {
	if _, err := os.Stat(baseRevisionDir); err != nil {
		t.Skipf("base revision not available: %v", err)
	}
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping canonical crlocal artifact-check")
	}
	publishScript, err := filepath.Abs(filepath.Join("..", "..", "tools", "designknowledge", "publish.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(publishScript); err != nil {
		t.Skipf("publish.mjs not available: %v", err)
	}

	outDir := filepath.Join(t.TempDir(), "revision-5")
	if _, err := GenerateRevision(context.Background(), baseRevisionDir, outDir, "revision-5", RevisionDelta{
		NewDocuments: []NewDocument{{Kind: "layout", ID: "layout.circle-grid", RelPath: "layouts/circle-grid.md", Summary: "环形宫格", Content: "# 新增\n\n占位。\n"}},
	}); err != nil {
		t.Fatalf("GenerateRevision: %v", err)
	}

	readIndexHash := func() string {
		var m designknowledge.Manifest
		raw, _ := os.ReadFile(filepath.Join(outDir, "manifest.json"))
		_ = json.Unmarshal(raw, &m)
		return m.IndexHash
	}
	goHash := readIndexHash()

	cmd := exec.Command(nodeBin, publishScript, outDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node publish.mjs failed: %v\n%s", err, out)
	}
	nodeHash := readIndexHash()

	if goHash != nodeHash {
		t.Fatalf("index_hash mismatch: go=%s node=%s (Go serialisation diverges from publish.mjs)", goHash, nodeHash)
	}
}
