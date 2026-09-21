package ruleworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge/documentcontract"
)

// revision.go copies a base design-knowledge revision into a new one, layers
// the agent's RevisionDelta on top with monotonically
// stable semantic document identities, then publishes it — recomputing content_hash and
// index_hash exactly like tools/designknowledge/publish.mjs so the runtime
// loader (internal/designknowledge) accepts it. The result is finally verified
// by loading it back through that same loader.

// revisionSuffix matches a trailing integer, e.g. "revision-4" -> 4.
var revisionSuffix = regexp.MustCompile(`^(.*?)(\d+)$`)

func MaterializeDocumentContracts(delta *RevisionDelta, require bool) error {
	if delta == nil {
		return errors.New("revision delta is required")
	}
	for i := range delta.NewDocuments {
		document := &delta.NewDocuments[i]
		if document.Document == nil {
			if require {
				return fmt.Errorf("newDocuments[%d]: structured document is required for publication", i)
			}
			continue
		}
		raw, err := documentcontract.RoundTrip(*document.Document)
		if err != nil {
			return fmt.Errorf("newDocuments[%d]: %w", i, err)
		}
		document.Content = string(raw)
		document.ID = document.Document.ID
		document.Kind = document.Document.Kind
		document.Version = document.Document.Version
		document.Summary = document.Document.Summary
		document.AppliesTo = append([]string(nil), document.Document.AppliesTo...)
		document.NotFor = append([]string(nil), document.Document.NotFor...)
		document.Requires = append([]string(nil), document.Document.Requires...)
		document.References = append([]string(nil), document.Document.References...)
		document.ConflictsWith = append([]string(nil), document.Document.ConflictsWith...)
	}
	for i := range delta.ModifiedDocuments {
		document := &delta.ModifiedDocuments[i]
		if document.Document == nil {
			if require {
				return fmt.Errorf("modifiedDocuments[%d]: structured document is required for publication", i)
			}
			continue
		}
		if document.ID != "" && document.ID != document.Document.ID {
			return fmt.Errorf("modifiedDocuments[%d]: id does not match document id", i)
		}
		raw, err := documentcontract.RoundTrip(*document.Document)
		if err != nil {
			return fmt.Errorf("modifiedDocuments[%d]: %w", i, err)
		}
		document.ID = document.Document.ID
		document.Version = document.Document.Version
		document.Content = string(raw)
	}
	return nil
}

// NextRevisionID increments the trailing integer of a revision id
// ("revision-4" -> "revision-5"). It errors if the id has no trailing integer.
func NextRevisionID(base string) (string, error) {
	m := revisionSuffix.FindStringSubmatch(base)
	if m == nil {
		return "", fmt.Errorf("revision id %q has no trailing integer to increment", base)
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return "", fmt.Errorf("revision id %q: %w", base, err)
	}
	return fmt.Sprintf("%s%d", m[1], n+1), nil
}

// GenerateRevision builds the new revision directory from baseDir, applies the
// delta, publishes, and loads it back to self-verify. It returns the resolved
// new revision id and the list of stable layout ids added by this revision.
func GenerateRevision(ctx context.Context, baseDir, outDir, newRevisionID string, delta RevisionDelta) ([]string, error) {
	// excludedDirs are base-revision subtrees the worker does not carry forward:
	// Audit output is derived runtime data and is not carried into a new
	// publishable rule revision. The source mapping is copied back below.
	excludedDirs := map[string]struct{}{"audit": {}}
	if err := copyTree(baseDir, outDir, excludedDirs); err != nil {
		return nil, fmt.Errorf("copy base revision: %w", err)
	}
	if err := copyAuditSourceMapping(baseDir, outDir); err != nil {
		return nil, err
	}

	indexPath := filepath.Join(outDir, "index.json")
	manifestPath := filepath.Join(outDir, "manifest.json")

	var index designknowledge.Index
	if err := readJSONFile(indexPath, &index); err != nil {
		return nil, err
	}
	var manifest designknowledge.Manifest
	if err := readJSONFile(manifestPath, &manifest); err != nil {
		return nil, err
	}
	index.RevisionID = newRevisionID
	manifest.RevisionID = newRevisionID

	existing := make(map[string]struct{}, len(index.Documents))
	existingPaths := make(map[string]struct{}, len(index.Documents))
	for _, doc := range index.Documents {
		existing[doc.ID] = struct{}{}
		existingPaths[filepath.ToSlash(filepath.Clean(filepath.FromSlash(doc.ContentRef)))] = struct{}{}
	}

	assignedLayouts := make([]string, 0)
	for i := range delta.NewDocuments {
		nd := &delta.NewDocuments[i]
		if strings.TrimSpace(nd.Kind) == "" || strings.TrimSpace(nd.Content) == "" {
			return nil, fmt.Errorf("newDocuments[%d]: kind and content are required", i)
		}
		if nd.Kind == string(designknowledge.KindLayout) || nd.Kind == string(designknowledge.KindElement) || nd.Kind == string(designknowledge.KindRule) {
			assignMissingDocumentIdentity(nd, existing, existingPaths)
		}
		if nd.ID == "" || nd.RelPath == "" {
			return nil, fmt.Errorf("newDocuments[%d]: id and relPath are required for %s docs", i, nd.Kind)
		}
		normalizedPath, err := normalizeRevisionRelPath(nd.RelPath)
		if err != nil {
			return nil, fmt.Errorf("newDocuments[%d]: relPath %q: %w", i, nd.RelPath, err)
		}
		nd.RelPath = normalizedPath
		if _, dup := existing[nd.ID]; dup {
			return nil, fmt.Errorf("newDocuments[%d]: id %q already exists in base revision", i, nd.ID)
		}
		if _, dup := existingPaths[nd.RelPath]; dup {
			return nil, fmt.Errorf("newDocuments[%d]: relPath %q already exists in base revision", i, nd.RelPath)
		}
		existing[nd.ID] = struct{}{}
		existingPaths[nd.RelPath] = struct{}{}
		if nd.Kind == string(designknowledge.KindLayout) {
			assignedLayouts = append(assignedLayouts, nd.ID)
		}

		target := filepath.Join(outDir, filepath.FromSlash(nd.RelPath))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, fmt.Errorf("newDocuments[%d]: mkdir: %w", i, err)
		}
		if err := os.WriteFile(target, []byte(nd.Content), 0o644); err != nil {
			return nil, fmt.Errorf("newDocuments[%d]: write %s: %w", i, nd.RelPath, err)
		}
		version := nd.Version
		if version == "" {
			version = "1.0.0"
		}
		summary := strings.TrimSpace(nd.Summary)
		if summary == "" {
			summary = deriveDocumentSummary(nd.Content, nd.ID)
		}
		index.Documents = append(index.Documents, designknowledge.Metadata{
			ID:            nd.ID,
			Kind:          designknowledge.Kind(nd.Kind),
			Version:       version,
			Status:        "enabled",
			Summary:       summary,
			Tags:          nd.Tags,
			AppliesTo:     nd.AppliesTo,
			NotFor:        nd.NotFor,
			Requires:      nd.Requires,
			ConflictsWith: nd.ConflictsWith,
			References:    nd.References,
			ContentRef:    nd.RelPath,
			// ContentHash filled by publishRevision.
		})
	}

	// Apply edits to existing baseline documents (修改): look the doc up by id,
	// overwrite its file in place; id/kind/path stay fixed.
	byID := make(map[string]*designknowledge.Metadata, len(index.Documents))
	for i := range index.Documents {
		byID[index.Documents[i].ID] = &index.Documents[i]
	}
	for i := range delta.ModifiedDocuments {
		md := &delta.ModifiedDocuments[i]
		if strings.TrimSpace(md.ID) == "" || strings.TrimSpace(md.Content) == "" {
			return nil, fmt.Errorf("modifiedDocuments[%d]: id and content are required", i)
		}
		meta, ok := byID[md.ID]
		if !ok {
			return nil, fmt.Errorf("modifiedDocuments[%d]: id %q not found in base revision", i, md.ID)
		}
		target := filepath.Join(outDir, filepath.FromSlash(meta.ContentRef))
		if err := os.WriteFile(target, []byte(md.Content), 0o644); err != nil {
			return nil, fmt.Errorf("modifiedDocuments[%d]: overwrite %s: %w", i, meta.ContentRef, err)
		}
		if v := strings.TrimSpace(md.Version); v != "" {
			meta.Version = v
		}
	}

	if err := publishRevision(outDir, &index, &manifest); err != nil {
		return nil, err
	}

	// Self-verify with the production loader so a bad revision fails here, not at
	// service startup.
	parent := filepath.Dir(outDir)
	base := filepath.Base(outDir)
	if _, err := designknowledge.Load(ctx, os.DirFS(parent), base); err != nil {
		return nil, fmt.Errorf("published revision failed loader self-check: %w", err)
	}
	return assignedLayouts, nil
}

// deriveDocumentSummary is a deterministic last line of defence for older
// agents or replay payloads that predate summary being required by the tool
// schema. Prefer the first Markdown heading/prose line and fall back to the
// already validated document id, so loader metadata can never be incomplete.
func deriveDocumentSummary(content, documentID string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "---" || strings.HasPrefix(line, "```") {
			continue
		}
		line = strings.TrimSpace(strings.TrimLeft(line, "#*- "))
		if line == "" {
			continue
		}
		runes := []rune(line)
		if len(runes) > 120 {
			line = string(runes[:120])
		}
		return line
	}
	return documentID
}

// assignMissingDocumentIdentity gives element/rule documents a deterministic
// id and content path when an older or imperfect model omits either field. A
// short content hash is added only when the readable slug would collide.
func assignMissingDocumentIdentity(nd *NewDocument, existingIDs, existingPaths map[string]struct{}) {
	autoID := strings.TrimSpace(nd.ID) == ""
	autoPath := strings.TrimSpace(nd.RelPath) == ""
	if !autoID && !autoPath {
		return
	}
	slug := deriveNewDocumentSlug(*nd)
	directory := nd.Kind + "s"
	assign := func(candidate string) {
		if autoID {
			nd.ID = nd.Kind + "." + candidate
		}
		if autoPath {
			nd.RelPath = directory + "/" + candidate + ".md"
		}
	}
	assign(slug)
	_, idCollision := existingIDs[nd.ID]
	_, pathCollision := existingPaths[filepath.ToSlash(filepath.Clean(filepath.FromSlash(nd.RelPath)))]
	if (autoID && idCollision) || (autoPath && pathCollision) {
		sum := sha256.Sum256([]byte(nd.Kind + "\x00" + nd.Content))
		assign(slug + "-" + hex.EncodeToString(sum[:4]))
	}
}

func deriveNewDocumentSlug(nd NewDocument) string {
	candidates := []string{nd.ID, strings.TrimSuffix(filepath.Base(filepath.FromSlash(nd.RelPath)), filepath.Ext(nd.RelPath)), nd.Summary}
	for _, line := range strings.Split(nd.Content, "\n") {
		candidates = append(candidates, strings.TrimSpace(strings.TrimLeft(line, "#*- ")))
	}
	for _, candidate := range candidates {
		if slug := safeSlug(candidate); slug != "" {
			return slug
		}
	}
	sum := sha256.Sum256([]byte(nd.Kind + "\x00" + nd.Content))
	return "generated-" + hex.EncodeToString(sum[:6])
}

func normalizeRevisionRelPath(relPath string) (string, error) {
	cleaned := filepath.Clean(filepath.FromSlash(strings.TrimSpace(relPath)))
	if cleaned == "." || filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("must stay inside the revision directory")
	}
	return filepath.ToSlash(cleaned), nil
}

// publishRevision recomputes every content_hash from raw file bytes, sorts the
// documents by id, serialises the index as JSON.stringify(index,null,2)+"\n"
// (2-space indent, no HTML escaping), computes index_hash over those exact
// bytes, and writes index.json + manifest.json. This mirrors publish.mjs so the
// produced hashes match the canonical tool.
func publishRevision(outDir string, index *designknowledge.Index, manifest *designknowledge.Manifest) error {
	seen := make(map[string]struct{}, len(index.Documents))
	for i := range index.Documents {
		doc := &index.Documents[i]
		if doc.ID == "" {
			return fmt.Errorf("publish: empty document id")
		}
		if _, dup := seen[doc.ID]; dup {
			return fmt.Errorf("publish: duplicate document id %q", doc.ID)
		}
		seen[doc.ID] = struct{}{}
		raw, err := os.ReadFile(filepath.Join(outDir, filepath.FromSlash(doc.ContentRef)))
		if err != nil {
			return fmt.Errorf("publish: read %s: %w", doc.ContentRef, err)
		}
		if len(raw) == 0 {
			return fmt.Errorf("publish: empty document %s", doc.ID)
		}
		doc.ContentHash = digest(raw)
	}

	sort.Slice(index.Documents, func(a, b int) bool {
		return index.Documents[a].ID < index.Documents[b].ID
	})

	indexRaw, err := marshalCanonical(index)
	if err != nil {
		return fmt.Errorf("publish: encode index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "index.json"), indexRaw, 0o644); err != nil {
		return fmt.Errorf("publish: write index.json: %w", err)
	}

	manifest.SchemaVersion = "design_knowledge_manifest.v1"
	manifest.IndexRef = "index.json"
	manifest.IndexHash = digest(indexRaw)
	manifest.Status = "published"
	manifestRaw, err := marshalCanonical(manifest)
	if err != nil {
		return fmt.Errorf("publish: encode manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), manifestRaw, 0o644); err != nil {
		return fmt.Errorf("publish: write manifest.json: %w", err)
	}
	return nil
}

// marshalCanonical replicates `JSON.stringify(value, null, 2) + "\n"`: 2-space
// indent, no HTML escaping, single trailing newline (json.Encoder.Encode already
// appends one newline).
func marshalCanonical(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func safeSlug(id string) string {
	slug := id
	if i := strings.LastIndex(slug, "."); i >= 0 {
		slug = slug[i+1:]
	}
	slug = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, slug)
	slug = strings.Trim(slug, "-")
	return slug
}

func readJSONFile(path string, target any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// copyTree recursively copies src into dst, creating dst fresh (an existing dst
// is removed first so re-runs are deterministic). Top-level directories whose
// name is in excludeDirs are pruned entirely.
func copyTree(src, dst string, excludeDirs map[string]struct{}) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if _, skip := excludeDirs[rel]; skip {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		return copyFile(path, filepath.Join(dst, rel))
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func copyAuditSourceMapping(baseDir, outDir string) error {
	source := filepath.Join(baseDir, "audit", "source-mapping.tsv")
	if _, err := os.Stat(source); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat audit source mapping: %w", err)
	}
	target := filepath.Join(outDir, "audit", "source-mapping.tsv")
	if err := copyFile(source, target); err != nil {
		return fmt.Errorf("copy audit source mapping: %w", err)
	}
	return nil
}
