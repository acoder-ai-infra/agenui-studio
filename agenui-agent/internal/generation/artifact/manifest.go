package artifact

import (
	"context"
	"errors"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/sdk"
)

// AGenUIManifest is a read projection of domain artifacts owned by one Harness
// Run. It intentionally has no lifecycle status, retry state, transcript or
// progress fields: those facts belong to Harness.
type AGenUIManifest struct {
	RunID              string   `json:"run_id"`
	PreviousFinal      *Pointer `json:"previous_final,omitempty"`
	Contract           *Pointer `json:"contract,omitempty"`
	EditContract       *Pointer `json:"edit_contract,omitempty"`
	Preflight          *Pointer `json:"preflight,omitempty"`
	CapabilityEvidence *Pointer `json:"capability_evidence,omitempty"`
	Design             *Pointer `json:"design,omitempty"`
	Requirements       *Pointer `json:"requirements,omitempty"`
	BindingSources     *Pointer `json:"binding_sources,omitempty"`
	Binding            *Pointer `json:"binding,omitempty"`
	Final              *Pointer `json:"final,omitempty"`
	RuleRevision       string   `json:"rule_revision,omitempty"`
	CatalogRevision    string   `json:"catalog_revision,omitempty"`
	MaterializationCAS string   `json:"materialization_cas,omitempty"`
}

// Manifest derives the current domain index from Harness Artifact history. It
// is not persisted as a mutable shadow row, so restart recovery cannot diverge
// from the Run that owns the artifacts.
func (s *Store) Manifest(ctx context.Context, identity harness.Identity) (AGenUIManifest, error) {
	if err := validateStepIdentity(identity); err != nil {
		return AGenUIManifest{}, err
	}
	page, err := s.client.List(ctx, harness.ListArtifactsRequest{Identity: identity, Kind: harness.ArtifactKindHostData, Limit: 2000})
	if err != nil {
		return AGenUIManifest{}, err
	}
	manifest := AGenUIManifest{RunID: identity.RunID}
	for _, info := range page.Items {
		if info.ArtifactRef.RunID != identity.RunID || info.MIME != stepMIME || info.Kind != string(harness.ArtifactKindHostData) {
			continue
		}
		pointer := Pointer{Ref: info.Ref, Hash: info.Hash, MIME: info.MIME, Size: info.SizeBytes, SchemaVersion: SchemaVersion}
		switch {
		case info.Name == artifactName(StepContract):
			manifest.Contract = singleManifestPointer(manifest.Contract, pointer)
		case info.Name == artifactName(StepEditContract):
			manifest.EditContract = singleManifestPointer(manifest.EditContract, pointer)
		case info.Name == artifactName(StepPreflight):
			manifest.Preflight = singleManifestPointer(manifest.Preflight, pointer)
		case info.Name == artifactName(StepCapabilityEvidence):
			manifest.CapabilityEvidence = singleManifestPointer(manifest.CapabilityEvidence, pointer)
		case info.Name == artifactName(StepDesign):
			manifest.Design = singleManifestPointer(manifest.Design, pointer)
		case info.Name == artifactName(StepRequirements):
			manifest.Requirements = singleManifestPointer(manifest.Requirements, pointer)
		case info.Name == artifactName(StepBindingSources):
			manifest.BindingSources = singleManifestPointer(manifest.BindingSources, pointer)
		case info.Name == artifactName(StepBinding):
			manifest.Binding = singleManifestPointer(manifest.Binding, pointer)
		case info.Name == artifactName(StepFinal):
			manifest.Final = singleManifestPointer(manifest.Final, pointer)
		}
	}
	return manifest, nil
}

func singleManifestPointer(current *Pointer, next Pointer) *Pointer {
	if current == nil {
		value := next
		return &value
	}
	if current.Ref == next.Ref {
		return current
	}
	// A single-valued domain artifact with two refs is corrupt. Keep the
	// projection unusable; callers will also receive the ordinary Load conflict.
	value := Pointer{Ref: strings.Join([]string{current.Ref, next.Ref}, "|")}
	return &value
}

func (m AGenUIManifest) Validate() error {
	if strings.TrimSpace(m.RunID) == "" {
		return errors.New("AGenUI manifest: run_id is required")
	}
	for _, pointer := range []*Pointer{m.Contract, m.EditContract, m.Preflight, m.CapabilityEvidence, m.Design, m.Requirements, m.BindingSources, m.Binding, m.Final} {
		if pointer != nil && (pointer.Ref == "" || strings.Contains(pointer.Ref, "|")) {
			return errors.New("AGenUI manifest: conflicting artifact refs")
		}
	}
	return nil
}
