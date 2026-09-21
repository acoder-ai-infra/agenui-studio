package designknowledge

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
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	defaultMaxDocumentBytes = 64 << 10
	defaultMaxIndexBytes    = 1 << 20
)

type Kind string

const (
	KindLayout  Kind = "layout"
	KindElement Kind = "element"
	KindRule    Kind = "rule"
)

type Manifest struct {
	SchemaVersion string `json:"schema_version"`
	RevisionID    string `json:"revision_id"`
	Status        string `json:"status"`
	IndexRef      string `json:"index_ref"`
	IndexHash     string `json:"index_hash"`
}

type Metadata struct {
	ID            string   `json:"id"`
	Kind          Kind     `json:"kind"`
	Version       string   `json:"version"`
	Status        string   `json:"status"`
	Summary       string   `json:"summary"`
	Tags          []string `json:"tags,omitempty"`
	AppliesTo     []string `json:"applies_to,omitempty"`
	NotFor        []string `json:"not_for,omitempty"`
	Requires      []string `json:"requires,omitempty"`
	ConflictsWith []string `json:"conflicts_with,omitempty"`
	References    []string `json:"references,omitempty"`
	ContentRef    string   `json:"content_ref"`
	ContentHash   string   `json:"content_hash"`
}

type Index struct {
	SchemaVersion string     `json:"schema_version"`
	RevisionID    string     `json:"revision_id"`
	Documents     []Metadata `json:"documents"`
}

type Document struct {
	Metadata Metadata
	Content  string
}

type ResolveOptions struct {
	MaxDocuments int
	ByteBudget   int
}

type Repository struct {
	manifest Manifest
	index    Index
	byID     map[string]Document
}

func Load(ctx context.Context, source fs.FS, revisionDir string) (*Repository, error) {
	if source == nil {
		return nil, errors.New("design knowledge: filesystem is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	revisionDir, err := cleanRelativePath(revisionDir)
	if err != nil {
		return nil, fmt.Errorf("design knowledge: revision directory: %w", err)
	}
	var manifest Manifest
	if err := readStrictJSON(source, path.Join(revisionDir, "manifest.json"), defaultMaxIndexBytes, &manifest); err != nil {
		return nil, fmt.Errorf("design knowledge: manifest: %w", err)
	}
	if manifest.SchemaVersion != "design_knowledge_manifest.v1" ||
		strings.TrimSpace(manifest.RevisionID) == "" ||
		manifest.Status != "published" {
		return nil, errors.New("design knowledge: manifest must be a published v1 revision")
	}
	indexRef, err := cleanRelativePath(manifest.IndexRef)
	if err != nil {
		return nil, fmt.Errorf("design knowledge: index_ref: %w", err)
	}
	indexBytes, err := readLimited(source, path.Join(revisionDir, indexRef), defaultMaxIndexBytes)
	if err != nil {
		return nil, fmt.Errorf("design knowledge: index: %w", err)
	}
	if err := verifyHash(indexBytes, manifest.IndexHash); err != nil {
		return nil, fmt.Errorf("design knowledge: index: %w", err)
	}
	var index Index
	if err := decodeStrict(indexBytes, &index); err != nil {
		return nil, fmt.Errorf("design knowledge: index: %w", err)
	}
	if index.SchemaVersion != "design_knowledge_index.v1" ||
		index.RevisionID != manifest.RevisionID {
		return nil, errors.New("design knowledge: index does not match manifest revision")
	}
	byID := make(map[string]Document, len(index.Documents))
	for i := range index.Documents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		meta := cloneMetadata(index.Documents[i])
		if err := validateMetadata(meta); err != nil {
			return nil, fmt.Errorf("design knowledge: document %d: %w", i, err)
		}
		if _, exists := byID[meta.ID]; exists {
			return nil, fmt.Errorf("design knowledge: duplicate document id %q", meta.ID)
		}
		contentRef, err := cleanRelativePath(meta.ContentRef)
		if err != nil {
			return nil, fmt.Errorf("design knowledge: document %q content_ref: %w", meta.ID, err)
		}
		content, err := readLimited(source, path.Join(revisionDir, contentRef), defaultMaxDocumentBytes)
		if err != nil {
			return nil, fmt.Errorf("design knowledge: document %q: %w", meta.ID, err)
		}
		if !utf8.Valid(content) {
			return nil, fmt.Errorf("design knowledge: document %q is not UTF-8", meta.ID)
		}
		if err := verifyHash(content, meta.ContentHash); err != nil {
			return nil, fmt.Errorf("design knowledge: document %q: %w", meta.ID, err)
		}
		byID[meta.ID] = Document{Metadata: meta, Content: string(content)}
	}
	for id, document := range byID {
		for _, ref := range append(append([]string(nil), document.Metadata.Requires...), document.Metadata.References...) {
			if _, ok := byID[ref]; !ok {
				return nil, fmt.Errorf("design knowledge: document %q references missing %q", id, ref)
			}
		}
		for _, conflict := range document.Metadata.ConflictsWith {
			if conflict == id {
				return nil, fmt.Errorf("design knowledge: document %q conflicts with itself", id)
			}
			if _, ok := byID[conflict]; !ok {
				return nil, fmt.Errorf("design knowledge: document %q conflicts with missing %q", id, conflict)
			}
			if !containsString(byID[conflict].Metadata.ConflictsWith, id) {
				return nil, fmt.Errorf(
					"design knowledge: conflict %q -> %q is not symmetric",
					id,
					conflict,
				)
			}
		}
	}
	if err := validateRequiresAcyclic(byID); err != nil {
		return nil, err
	}
	index.Documents = cloneMetadataSlice(index.Documents)
	return &Repository{manifest: manifest, index: index, byID: byID}, nil
}

func validateRequiresAcyclic(documents map[string]Document) error {
	const (
		visiting = 1
		visited  = 2
	)
	state := make(map[string]int, len(documents))
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case visiting:
			return fmt.Errorf("design knowledge: requires cycle contains %q", id)
		case visited:
			return nil
		}
		state[id] = visiting
		for _, dependency := range documents[id].Metadata.Requires {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[id] = visited
		return nil
	}
	for id := range documents {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (r *Repository) RevisionID() string { return r.manifest.RevisionID }

func (r *Repository) RevisionHash() string { return r.manifest.IndexHash }

func (r *Repository) Metadata(id string) (Metadata, bool) {
	if r == nil {
		return Metadata{}, false
	}
	document, ok := r.byID[strings.TrimSpace(id)]
	if !ok {
		return Metadata{}, false
	}
	return cloneMetadata(document.Metadata), true
}

func (r *Repository) Document(id string) (Document, bool) {
	if r == nil {
		return Document{}, false
	}
	document, ok := r.byID[strings.TrimSpace(id)]
	if !ok || document.Metadata.Status != "enabled" {
		return Document{}, false
	}
	return cloneDocument(document), true
}

func (r *Repository) Index() Index {
	return Index{
		SchemaVersion: r.index.SchemaVersion,
		RevisionID:    r.index.RevisionID,
		Documents:     cloneMetadataSlice(r.index.Documents),
	}
}

// Catalog returns the enabled metadata projection used for L0 disclosure.
// Bodies and dependency documents stay out of the model context until the
// model explicitly selects a layout.
func (r *Repository) Catalog(kinds ...Kind) []Metadata {
	if r == nil {
		return nil
	}
	allowed := make(map[Kind]struct{}, len(kinds))
	for _, kind := range kinds {
		allowed[kind] = struct{}{}
	}
	result := make([]Metadata, 0, len(r.index.Documents))
	for _, metadata := range r.index.Documents {
		if metadata.Status != "enabled" {
			continue
		}
		if len(allowed) > 0 {
			if _, ok := allowed[metadata.Kind]; !ok {
				continue
			}
		}
		result = append(result, cloneMetadata(metadata))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// Resolve loads selected documents and their mandatory dependency closure.
// A mandatory document is never silently dropped to fit the budget: callers
// receive an explicit error and can select fewer layouts or ask the user.
func (r *Repository) Resolve(ids []string, options ResolveOptions) ([]Document, error) {
	if r == nil {
		return nil, errors.New("design knowledge: repository is required")
	}
	if len(ids) == 0 {
		return nil, errors.New("design knowledge: at least one document id is required")
	}
	maxDocuments := options.MaxDocuments
	if maxDocuments <= 0 {
		maxDocuments = 24
	}
	byteBudget := options.ByteBudget
	if byteBudget <= 0 {
		byteBudget = 96 << 10
	}
	requested := make(map[string]struct{}, len(ids))
	selected := make(map[string]Document)
	var visit func(string) error
	visit = func(id string) error {
		id = strings.TrimSpace(id)
		if id == "" {
			return errors.New("design knowledge: document id must not be empty")
		}
		if _, ok := selected[id]; ok {
			return nil
		}
		document, ok := r.byID[id]
		if !ok || document.Metadata.Status != "enabled" {
			return fmt.Errorf("design knowledge: document %q is not enabled", id)
		}
		selected[id] = document
		if len(selected) > maxDocuments {
			return fmt.Errorf(
				"design knowledge: dependency closure exceeds %d documents",
				maxDocuments,
			)
		}
		for _, dependency := range document.Metadata.Requires {
			if err := visit(dependency); err != nil {
				return fmt.Errorf("%s requires %s: %w", id, dependency, err)
			}
		}
		return nil
	}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if _, duplicate := requested[id]; duplicate {
			continue
		}
		requested[id] = struct{}{}
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	for id, document := range selected {
		for _, conflict := range document.Metadata.ConflictsWith {
			if _, exists := selected[conflict]; exists {
				return nil, fmt.Errorf(
					"design knowledge: selected documents %q and %q conflict",
					id,
					conflict,
				)
			}
		}
	}
	result := make([]Document, 0, len(selected))
	for _, document := range selected {
		result = append(result, cloneDocument(document))
	}
	sort.Slice(result, func(i, j int) bool {
		_, iRequested := requested[result[i].Metadata.ID]
		_, jRequested := requested[result[j].Metadata.ID]
		if iRequested != jRequested {
			return iRequested
		}
		iRank := kindRank(result[i].Metadata.Kind)
		jRank := kindRank(result[j].Metadata.Kind)
		if iRank != jRank {
			return iRank < jRank
		}
		return result[i].Metadata.ID < result[j].Metadata.ID
	})
	used := 0
	for _, document := range result {
		used += len(document.Content)
	}
	if used > byteBudget {
		return nil, fmt.Errorf(
			"design knowledge: dependency closure is %d bytes, exceeds %d",
			used,
			byteBudget,
		)
	}
	return result, nil
}

func validateMetadata(meta Metadata) error {
	if strings.TrimSpace(meta.ID) == "" || strings.TrimSpace(meta.Version) == "" ||
		strings.TrimSpace(meta.Summary) == "" || meta.Status == "" {
		return errors.New("id, version, status, and summary are required")
	}
	switch meta.Kind {
	case KindLayout, KindElement, KindRule:
	default:
		return fmt.Errorf("unsupported kind %q", meta.Kind)
	}
	if meta.Status != "enabled" && meta.Status != "disabled" {
		return fmt.Errorf("unsupported status %q", meta.Status)
	}
	if strings.TrimSpace(meta.ContentRef) == "" {
		return errors.New("content_ref is required")
	}
	return nil
}

func kindRank(kind Kind) int {
	switch kind {
	case KindLayout:
		return 0
	case KindElement:
		return 1
	case KindRule:
		return 2
	default:
		return 4
	}
}

func readStrictJSON(source fs.FS, name string, limit int64, target any) error {
	raw, err := readLimited(source, name, limit)
	if err != nil {
		return err
	}
	return decodeStrict(raw, target)
}

func decodeStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func readLimited(source fs.FS, name string, limit int64) ([]byte, error) {
	file, err := source.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	return raw, nil
}

func cleanRelativePath(value string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" || path.IsAbs(value) {
		return "", errors.New("path must be relative")
	}
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("path escapes revision root")
	}
	return cleaned, nil
}

func verifyHash(content []byte, expected string) error {
	if !strings.HasPrefix(expected, "sha256:") {
		return errors.New("sha256 hash is required")
	}
	sum := sha256.Sum256(content)
	actual := "sha256:" + hex.EncodeToString(sum[:])
	if actual != expected {
		return fmt.Errorf("hash mismatch: got %s", actual)
	}
	return nil
}

func cloneDocument(source Document) Document {
	return Document{Metadata: cloneMetadata(source.Metadata), Content: source.Content}
}

func cloneMetadataSlice(source []Metadata) []Metadata {
	result := make([]Metadata, len(source))
	for index := range source {
		result[index] = cloneMetadata(source[index])
	}
	return result
}

func cloneMetadata(source Metadata) Metadata {
	source.Tags = append([]string(nil), source.Tags...)
	source.AppliesTo = append([]string(nil), source.AppliesTo...)
	source.NotFor = append([]string(nil), source.NotFor...)
	source.Requires = append([]string(nil), source.Requires...)
	source.ConflictsWith = append([]string(nil), source.ConflictsWith...)
	source.References = append([]string(nil), source.References...)
	return source
}
