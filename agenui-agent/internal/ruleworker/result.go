package ruleworker

import "github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge/documentcontract"

// RevisionPlan is the model's sole output: typed document changes only.
type RevisionPlan struct {
	Delta RevisionDelta `json:"revisionDelta"`
}

// RevisionDelta is the set of document changes layered on top of the base
// revision: brand-new documents (新增, incrementing ids) and edits to existing
// baseline documents (same id/path, content overwritten).
type RevisionDelta struct {
	NewDocuments      []NewDocument      `json:"newDocuments"`
	ModifiedDocuments []ModifiedDocument `json:"modifiedDocuments,omitempty"`
}

// ModifiedDocument is an edit to an existing baseline document, identified by
// its id (e.g. "layout.two-column-media-list"). The
// generator finds that document's content_ref in the base index and overwrites
// its file with Content; the id, kind and path stay fixed (旧编号不变).
type ModifiedDocument struct {
	ID       string                     `json:"id"`
	Content  string                     `json:"content,omitempty"`
	Document *documentcontract.Document `json:"document,omitempty"`
	Version  string                     `json:"version,omitempty"` // optional bumped version
}

// NewDocument is one design-knowledge document to add to the revision. Kind is
// one of layout|element|rule. RelPath is the content_ref relative to the
// revision dir. Structured documents provide the stable ID; the Host derives
// a safe path only when the model omitted one.
type NewDocument struct {
	ID            string                     `json:"id"`
	Kind          string                     `json:"kind"`
	Version       string                     `json:"version"`
	Summary       string                     `json:"summary"`
	Tags          []string                   `json:"tags,omitempty"`
	AppliesTo     []string                   `json:"appliesTo,omitempty"`
	Requires      []string                   `json:"requires,omitempty"`
	References    []string                   `json:"references,omitempty"`
	RelPath       string                     `json:"relPath"`
	Content       string                     `json:"content"`
	Document      *documentcontract.Document `json:"document,omitempty"`
	NotFor        []string                   `json:"notFor,omitempty"`
	ConflictsWith []string                   `json:"conflictsWith,omitempty"`
}
