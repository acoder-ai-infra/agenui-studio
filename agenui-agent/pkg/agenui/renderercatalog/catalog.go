// Package renderercatalog loads immutable, renderer-verified AGenUI catalogs.
//
// The current catalog pointer is a small pointer file. It deliberately keeps
// capability selection out of prompts and validators: both consume the same
// checked-in catalog snapshot.
package renderercatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/agenui/schema"
)

const DefaultRoot = "configs/renderer-catalogs"

type Current struct {
	CatalogVersion string `json:"catalog_version"`
	CatalogID      string `json:"catalog_id"`
	CatalogHash    string `json:"catalog_sha256"`
}

type Index struct {
	SchemaVersion string      `json:"schema_version"`
	Catalogs      []IndexItem `json:"catalogs"`
}

type IndexItem struct {
	CatalogVersion string `json:"catalog_version"`
	CatalogID      string `json:"catalog_id"`
	CatalogHash    string `json:"catalog_sha256"`
	Status         string `json:"status"`
}

// Snapshot is the verified selected renderer catalog.
type Snapshot struct {
	Version     string
	CatalogID   string
	CatalogPath string
	CatalogHash string
}

// JSON returns the exact, checksum-verified protocol catalog selected by this
// snapshot. It is intentionally not reduced to a second capability model:
// authors and validators must reason from the same catalog that defines the
// renderer contract.
func (s Snapshot) JSON() ([]byte, error) {
	if s.CatalogPath == "" || s.CatalogHash == "" {
		return nil, errors.New("renderer catalog: empty snapshot")
	}
	content, err := os.ReadFile(s.CatalogPath)
	if err != nil {
		return nil, fmt.Errorf("renderer catalog: read selected catalog: %w", err)
	}
	hash := sha256.Sum256(content)
	if hex.EncodeToString(hash[:]) != s.CatalogHash {
		return nil, fmt.Errorf("renderer catalog: catalog checksum changed for version %s", s.Version)
	}
	return content, nil
}

// AuthoringJSON is a catalog-derived prompt view for Markdown rule parsing.
// It keeps the component schemas and shared definitions, while omitting
// runtime-only functions and theme metadata. It is never a validation source:
// the full checksum-verified catalog remains the only published contract.
func (s Snapshot) AuthoringJSON() ([]byte, error) {
	content, err := s.JSON()
	if err != nil {
		return nil, err
	}
	var catalog struct {
		CatalogID   string                     `json:"catalogId"`
		Components  map[string]json.RawMessage `json:"components"`
		Definitions map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(content, &catalog); err != nil {
		return nil, fmt.Errorf("renderer catalog: parse authoring view: %w", err)
	}
	if catalog.CatalogID == "" || len(catalog.Components) == 0 {
		return nil, errors.New("renderer catalog: authoring view requires catalogId and components")
	}
	definitions := referencedDefinitions(catalog.Components, catalog.Definitions)
	return json.Marshal(struct {
		CatalogID   string                     `json:"catalogId"`
		Components  map[string]json.RawMessage `json:"components"`
		Definitions map[string]json.RawMessage `json:"$defs,omitempty"`
	}{catalog.CatalogID, catalog.Components, definitions})
}

var localDefinitionRef = regexp.MustCompile(`#/\$defs/([^"/]+)`)

// referencedDefinitions computes the transitive closure of local $defs used
// by component schemas. External schema references stay as references: they
// are protocol details that rule interpretation does not need to duplicate.
func referencedDefinitions(components, definitions map[string]json.RawMessage) map[string]json.RawMessage {
	pending := make([]string, 0)
	for _, component := range components {
		pending = append(pending, definitionRefs(component)...)
	}
	selected := make(map[string]json.RawMessage)
	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]
		definition, exists := definitions[name]
		if !exists {
			continue
		}
		if _, seen := selected[name]; seen {
			continue
		}
		selected[name] = definition
		pending = append(pending, definitionRefs(definition)...)
	}
	return selected
}

func definitionRefs(raw json.RawMessage) []string {
	matches := localDefinitionRef.FindAllSubmatch(raw, -1)
	refs := make([]string, 0, len(matches))
	for _, match := range matches {
		refs = append(refs, string(match[1]))
	}
	return refs
}

func LoadCurrent(root string) (Snapshot, error) {
	if strings.TrimSpace(root) == "" {
		root = DefaultRoot
	}
	var current Current
	if err := decode(filepath.Join(root, "current.json"), &current); err != nil {
		return Snapshot{}, fmt.Errorf("renderer catalog: current pointer: %w", err)
	}
	if current.CatalogVersion == "" || current.CatalogID == "" || current.CatalogHash == "" {
		return Snapshot{}, errors.New("renderer catalog: current pointer is incomplete")
	}
	var index Index
	if err := decode(filepath.Join(root, "index.json"), &index); err != nil {
		return Snapshot{}, fmt.Errorf("renderer catalog: index: %w", err)
	}
	matched := false
	for _, item := range index.Catalogs {
		if item.CatalogVersion == current.CatalogVersion && item.CatalogID == current.CatalogID &&
			item.CatalogHash == current.CatalogHash && item.Status == "published" {
			matched = true
			break
		}
	}
	if !matched {
		return Snapshot{}, fmt.Errorf("renderer catalog: current version %q is not a published indexed catalog", current.CatalogVersion)
	}
	path := filepath.Join(root, current.CatalogVersion, "catalog.json")
	content, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("renderer catalog: read %s: %w", current.CatalogVersion, err)
	}
	hash := sha256.Sum256(content)
	if actual := hex.EncodeToString(hash[:]); actual != current.CatalogHash {
		return Snapshot{}, fmt.Errorf("renderer catalog: catalog checksum mismatch for version %s", current.CatalogVersion)
	}
	var raw map[string]any
	if err := json.Unmarshal(content, &raw); err != nil {
		return Snapshot{}, fmt.Errorf("renderer catalog: parse selected catalog: %w", err)
	}
	if id, _ := raw[schema.CatalogIDKey].(string); id != current.CatalogID {
		return Snapshot{}, fmt.Errorf("renderer catalog: catalogId mismatch for version %s", current.CatalogVersion)
	}
	return Snapshot{Version: current.CatalogVersion, CatalogID: current.CatalogID, CatalogPath: path, CatalogHash: current.CatalogHash}, nil
}

func (s Snapshot) CatalogConfig() (*schema.CatalogConfig, error) {
	if s.CatalogPath == "" || s.Version == "" {
		return nil, errors.New("renderer catalog: empty snapshot")
	}
	return schema.NewCatalogConfigFromPath("renderer-catalog-"+s.Version, s.CatalogPath, "")
}

func decode(path string, output any) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("multiple JSON values")
	}
	return nil
}
