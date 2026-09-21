// Package runtime is the runtime core for executing Card Execution Packages:
// it parses and validates a package, calls data sources concurrently, merges
// same-entity supplementation, executes published JS operators and projects
// the AGenUI DataModel. It provides capabilities only; orchestration (caching,
// traffic policy, fallback chains) belongs to the host application.
package runtime

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// PackageVersion is the schema version implemented by this runtime.
const PackageVersion = "2.0"

const maxBindingWildcardDepth = 2

// DataSource roles.
const (
	RolePrimary    = "primary"
	RoleSupplement = "supplement"
)

// Missing policies carried from the content contract.
const (
	MissingBlock    = "block"
	MissingHide     = "hide"
	MissingFallback = "fallback"
)

// Param is one data source input. Template may reference execution params as
// {{name}}.
type Param struct {
	Name     string `json:"name"`
	In       string `json:"in"` // query | body
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Template string `json:"template,omitempty"`
	Value    any    `json:"value,omitempty"`
}

// DataSource is a fully expanded, self-contained API definition. Export from
// the studio replaces internal API IDs with these definitions so the package
// runs without studio access.
type DataSource struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Endpoint    string            `json:"endpoint"`
	Method      string            `json:"method"`
	Headers     map[string]string `json:"headers,omitempty"`
	Role        string            `json:"role"`
	ItemsPath   string            `json:"itemsPath,omitempty"`
	EntityKey   string            `json:"entityKey,omitempty"`
	Params      []Param           `json:"params,omitempty"`
}

// Invocation is one operator application inside a binding transform chain.
type Invocation struct {
	OperatorVersionID uint64         `json:"operatorVersionId"`
	Params            map[string]any `json:"params,omitempty"`
}

// Binding maps one semantic slot to an expanded data source field plus an
// optional transform chain.
type Binding struct {
	SlotID        string       `json:"slotId"`
	RequirementID string       `json:"requirementId"`
	DataSourceID  string       `json:"dataSourceId"`
	FieldPath     string       `json:"fieldPath"`
	RefKey        string       `json:"refKey"`
	MissingPolicy string       `json:"missingPolicy"`
	FallbackValue any          `json:"fallbackValue,omitempty"`
	Transforms    []Invocation `json:"transforms,omitempty"`
}

// Operator is the published Operator Detail data returned by the backend. A
// package embeds it unchanged, so runtime execution needs no studio access and
// no field-name translation layer.
type Operator struct {
	OperatorVersionID uint64          `json:"operatorVersionId"`
	OperatorKey       string          `json:"operatorKey"`
	Version           int             `json:"version"`
	InputSchema       json.RawMessage `json:"inputSchema"`
	ParamsSchema      json.RawMessage `json:"paramsSchema"`
	OutputSchema      json.RawMessage `json:"outputSchema"`
	SourceHash        string          `json:"sourceHash"`
	Language          string          `json:"language"`
	LanguageVersion   string          `json:"languageVersion,omitempty"`
	SourceCode        string          `json:"sourceCode"`
	Entry             string          `json:"entry"`
}

// Action is one executable interaction definition.
type Action struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Meta records provenance for traceability and replay.
type Meta struct {
	ContractHash     string    `json:"contractHash,omitempty"`
	DesignHash       string    `json:"designHash,omitempty"`
	RequirementsHash string    `json:"requirementsHash,omitempty"`
	GeneratedAt      time.Time `json:"generatedAt"`
	Generator        string    `json:"generator,omitempty"`
}

// Package is the Card Execution Package: the self-contained, runtime-ready
// artifact produced by the studio. See docs/card-package.md for the schema.
type Package struct {
	Version     string            `json:"version"`
	CardID      string            `json:"cardId"`
	Name        string            `json:"name"`
	Contract    json.RawMessage   `json:"contract,omitempty"`
	Protocol    []json.RawMessage `json:"protocol"`
	DataSources []DataSource      `json:"dataSources"`
	Bindings    []Binding         `json:"bindings"`
	Operators   []Operator        `json:"operators,omitempty"`
	Actions     []Action          `json:"actions,omitempty"`
	Meta        Meta              `json:"meta"`
}

// Load parses and validates a package. It rejects packages that reference
// unknown data sources or operators, which is the runtime-side half of the
// ID-expansion guarantee.
func Load(raw []byte) (*Package, error) {
	var pkg Package
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return nil, fmt.Errorf("runtime: decode package: %w", err)
	}
	if err := pkg.Validate(); err != nil {
		return nil, err
	}
	return &pkg, nil
}

// Validate enforces self-containment: every binding references a declared
// data source, every transform references a declared operator, and the
// package carries the protocol it renders.
func (p *Package) Validate() error {
	if p == nil {
		return fmt.Errorf("runtime: package is required")
	}
	if p.Version != PackageVersion {
		return fmt.Errorf("runtime: unsupported package version %q (want %q)", p.Version, PackageVersion)
	}
	if p.CardID == "" {
		return fmt.Errorf("runtime: package cardId is required")
	}
	if len(p.Protocol) == 0 {
		return fmt.Errorf("runtime: package protocol is required")
	}
	sources := make(map[string]DataSource)
	primaries := 0
	for _, ds := range p.DataSources {
		if ds.ID == "" || ds.Endpoint == "" {
			return fmt.Errorf("runtime: data source requires id and endpoint")
		}
		if _, exists := sources[ds.ID]; exists {
			return fmt.Errorf("runtime: duplicate data source id %q", ds.ID)
		}
		if ds.Role != RolePrimary && ds.Role != RoleSupplement {
			return fmt.Errorf("runtime: data source %q has invalid role %q", ds.ID, ds.Role)
		}
		if strings.TrimSpace(ds.ItemsPath) != "" {
			itemsTokens, err := parseFieldPath(ds.ItemsPath)
			if err != nil || wildcardCount(itemsTokens) != 0 {
				return fmt.Errorf("runtime: data source %q has invalid itemsPath %q", ds.ID, ds.ItemsPath)
			}
		}
		sources[ds.ID] = ds
		if ds.Role == RolePrimary {
			primaries++
		}
	}
	if primaries != 1 {
		return fmt.Errorf("runtime: package must declare exactly one primary data source, got %d", primaries)
	}
	primary, _ := p.PrimaryDataSource()
	operators := make(map[uint64]Operator)
	for _, op := range p.Operators {
		if op.OperatorVersionID == 0 || strings.TrimSpace(op.OperatorKey) == "" || op.Version < 1 ||
			strings.TrimSpace(op.SourceCode) == "" || strings.TrimSpace(op.Entry) == "" || !validContentHash(op.SourceHash) {
			return executionError(CodeOperatorNotPublished, "operator definition is incomplete", "", op.OperatorVersionID, false, nil)
		}
		language := strings.ToLower(strings.TrimSpace(op.Language))
		if language != "javascript" && language != "js" && language != "typescript" && language != "ts" {
			return executionError(CodeOperatorNotPublished, "operator language is not executable", "", op.OperatorVersionID, false, nil)
		}
		if op.SourceHash != hashContent(op.SourceCode) {
			return executionError(CodeOperatorSourceHashMismatch, "operator source hash does not match sourceCode", "", op.OperatorVersionID, false, nil)
		}
		if _, duplicate := operators[op.OperatorVersionID]; duplicate {
			return fmt.Errorf("runtime: duplicate operatorVersionId %d", op.OperatorVersionID)
		}
		for name, schema := range map[string]json.RawMessage{
			"inputSchema": op.InputSchema, "paramsSchema": op.ParamsSchema, "outputSchema": op.OutputSchema,
		} {
			if err := validateSchemaDefinition(schema); err != nil {
				return fmt.Errorf("runtime: operator %d %s: %w", op.OperatorVersionID, name, err)
			}
		}
		operators[op.OperatorVersionID] = op
	}
	for _, binding := range p.Bindings {
		if strings.TrimSpace(binding.SlotID) == "" || strings.TrimSpace(binding.FieldPath) == "" || strings.TrimSpace(binding.RefKey) == "" {
			return fmt.Errorf("runtime: binding requires slotId, fieldPath and refKey")
		}
		dataSource, exists := sources[binding.DataSourceID]
		if !exists {
			return fmt.Errorf("runtime: binding %q references unknown data source %q", binding.SlotID, binding.DataSourceID)
		}
		fieldTokens, err := parseFieldPath(binding.FieldPath)
		if err != nil {
			return executionError(CodeBindingSourcePathInvalid, "binding fieldPath is invalid", binding.SlotID, 0, false, err)
		}
		refTokens, err := parseRefKey(binding.RefKey)
		if err != nil {
			return executionError(CodeBindingTargetPathInvalid, "binding refKey is invalid", binding.SlotID, 0, false, err)
		}
		fieldWildcards, refWildcards := wildcardCount(fieldTokens), wildcardCount(refTokens)
		if fieldWildcards > maxBindingWildcardDepth || refWildcards > maxBindingWildcardDepth {
			return executionError(CodeBindingWildcardMismatch, "binding exceeds wildcard depth limit", binding.SlotID, 0, false,
				fmt.Errorf("fieldPath=%d refKey=%d max=%d", fieldWildcards, refWildcards, maxBindingWildcardDepth))
		}
		if refWildcards > 0 && fieldWildcards != refWildcards {
			return executionError(CodeBindingWildcardMismatch, "fieldPath and refKey wildcard coordinates do not match", binding.SlotID, 0, false,
				fmt.Errorf("fieldPath=%d refKey=%d", fieldWildcards, refWildcards))
		}
		if dataSource.Role == RoleSupplement && (refWildcards == 0 || dataSource.EntityKey == "" ||
			primary.EntityKey == "" || dataSource.EntityKey != primary.EntityKey) {
			return executionError(CodeSourceJoinNotProven, "supplement binding requires a proven entityKey coordinate join", binding.SlotID, 0, false, nil)
		}
		if binding.MissingPolicy != MissingBlock && binding.MissingPolicy != MissingHide && binding.MissingPolicy != MissingFallback {
			return fmt.Errorf("runtime: binding %q has invalid missingPolicy %q", binding.SlotID, binding.MissingPolicy)
		}
		for _, inv := range binding.Transforms {
			op, exists := operators[inv.OperatorVersionID]
			if !exists {
				return executionError(CodeOperatorNotPublished, "binding references an unpublished operator", binding.SlotID, inv.OperatorVersionID, false, nil)
			}
			if err := validateSchemaValue(op.ParamsSchema, normalizedParams(inv.Params)); err != nil {
				return executionError(CodeOperatorParamsValidationFailed, "operator params do not match paramsSchema", binding.SlotID, inv.OperatorVersionID, false, err)
			}
		}
	}
	for _, action := range p.Actions {
		if action.ID == "" || action.Type == "" || len(action.Payload) == 0 {
			return fmt.Errorf("runtime: action requires id, type and payload")
		}
		var payload struct {
			DataSourceID string `json:"dataSourceId"`
			Path         string `json:"path"`
			ComponentID  string `json:"componentId"`
		}
		if err := json.Unmarshal(action.Payload, &payload); err != nil {
			return fmt.Errorf("runtime: action %q payload is invalid: %w", action.ID, err)
		}
		if _, exists := sources[payload.DataSourceID]; !exists || payload.Path == "" || payload.ComponentID == "" {
			return fmt.Errorf("runtime: action %q requires a known data source, path and componentId", action.ID)
		}
	}
	if !validContentHash(p.Meta.ContractHash) || !validContentHash(p.Meta.DesignHash) ||
		!validContentHash(p.Meta.RequirementsHash) {
		return fmt.Errorf("runtime: meta contractHash, designHash and requirementsHash must be sha256 hashes")
	}
	return nil
}

func normalizedParams(params map[string]any) map[string]any {
	if params == nil {
		return map[string]any{}
	}
	return params
}

func validContentHash(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	for _, char := range value[len("sha256:"):] {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func hashContent(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("sha256:%x", sum[:])
}

// FindOperator returns the operator with the given published version ID.
func (p *Package) FindOperator(operatorVersionID uint64) (Operator, bool) {
	for _, op := range p.Operators {
		if op.OperatorVersionID == operatorVersionID {
			return op, true
		}
	}
	return Operator{}, false
}

// FindDataSource returns the data source with the given ID.
func (p *Package) FindDataSource(id string) (DataSource, bool) {
	for _, ds := range p.DataSources {
		if ds.ID == id {
			return ds, true
		}
	}
	return DataSource{}, false
}

// PrimaryDataSource returns the single primary data source.
func (p *Package) PrimaryDataSource() (DataSource, bool) {
	for _, ds := range p.DataSources {
		if ds.Role == RolePrimary {
			return ds, true
		}
	}
	return DataSource{}, false
}
