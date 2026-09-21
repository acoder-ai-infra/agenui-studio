// Package catalogadmission checks component availability against the current
// published Renderer Catalog.
package catalogadmission

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/agenui/renderercatalog"
)

// EditableStylePaths derives the authoring surface from the same verified
// catalog used by admission. It is a projection, not a second policy source.
func EditableStylePaths(root string) ([]string, error) {
	snapshot, err := renderercatalog.LoadCurrent(root)
	if err != nil {
		return nil, err
	}
	raw, err := snapshot.JSON()
	if err != nil {
		return nil, err
	}
	var catalog map[string]any
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return nil, fmt.Errorf("catalog editable styles: %w", err)
	}
	paths := map[string]struct{}{}
	definitions, _ := catalog["$defs"].(map[string]any)
	collectStyleProperties(catalog, definitions, paths)
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	if len(result) == 0 {
		return nil, errors.New("catalog editable styles: no style properties")
	}
	return result, nil
}

func collectStyleProperties(value any, definitions map[string]any, paths map[string]struct{}) {
	object, ok := value.(map[string]any)
	if !ok {
		if values, array := value.([]any); array {
			for _, child := range values {
				collectStyleProperties(child, definitions, paths)
			}
		}
		return
	}
	for key, child := range object {
		if key == "styles" {
			if schema, ok := child.(map[string]any); ok {
				if ref, _ := schema["$ref"].(string); strings.HasPrefix(ref, "#/$defs/") {
					schema, _ = definitions[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
				}
				if properties, ok := schema["properties"].(map[string]any); ok {
					for name := range properties {
						if strings.TrimSpace(name) != "" {
							paths["styles."+name] = struct{}{}
						}
					}
				}
			}
		}
		collectStyleProperties(child, definitions, paths)
	}
}

type Result struct {
	CatalogVersion string `json:"catalog_version,omitempty"`
	CatalogID      string `json:"catalog_id,omitempty"`
	Compatible     bool   `json:"compatible"`
	Status         string `json:"status"`
	// Message is a user-facing Chinese explanation for the management UI.
	// Reason remains the stable machine/debug detail for existing callers.
	Message string `json:"message"`
	Reason  string `json:"reason,omitempty"`
}

const (
	StatusSupported   = "supported"
	StatusUnsupported = "unsupported"
	StatusUnavailable = "unavailable"
)

// Analyze checks component availability against the current published catalog.
// The candidate catalogId is substituted only for validation; callers should
// use CatalogID to stamp the final createSurface message on success. A catalog
// pointer update therefore takes effect on the next finalization without a
// second, stale capability registry.
func Analyze(root, agenuiJSON string) Result {
	snapshot, err := renderercatalog.LoadCurrent(root)
	if err != nil {
		return unavailableResult(err.Error())
	}
	candidate, err := withCatalogID(agenuiJSON, snapshot.CatalogID)
	if err != nil {
		return unavailableResult(err.Error())
	}
	catalogJSON, err := snapshot.JSON()
	if err != nil {
		return unavailableResult(err.Error())
	}
	var catalog struct {
		Components map[string]json.RawMessage `json:"components"`
	}
	if err := json.Unmarshal(catalogJSON, &catalog); err != nil || len(catalog.Components) == 0 {
		return unavailableResult("invalid renderer catalog")
	}
	// Catalog admission owns only component availability. Component attributes, full
	// AGenUI validity, binding completeness, topology, paths and actions are
	// validated by their own stages or clients and must never be translated
	// into a renderer-version mismatch here.
	components, err := componentNames(candidate)
	if err != nil {
		return unavailableResult(err.Error())
	}
	unsupported := make([]string, 0)
	for name := range components {
		if _, exists := catalog.Components[name]; !exists {
			unsupported = append(unsupported, name)
		}
	}
	if len(unsupported) == 0 {
		return Result{
			CatalogVersion: snapshot.Version,
			CatalogID:      snapshot.CatalogID,
			Compatible:     true,
			Status:         StatusSupported,
			Message:        "该卡片可在当前 Renderer Catalog " + snapshot.Version + " 中生效。",
		}
	}
	return Result{
		Status:  StatusUnsupported,
		Reason:  "final AGenUI uses components absent from the current renderer catalog",
		Message: "当前 Renderer Catalog 不支持该卡片所用组件，请调整设计方案，或更新并发布对应 catalog 后重试。",
	}
}

func unavailableResult(reason string) Result {
	return Result{
		Status:  StatusUnavailable,
		Reason:  reason,
		Message: "暂时无法校验卡片与客户端渲染能力的兼容性，请稍后重试或联系管理员。",
	}
}

func StampCatalogID(agenuiJSON, catalogID string) (string, error) {
	return withCatalogID(agenuiJSON, catalogID)
}

func withCatalogID(agenuiJSON, catalogID string) (string, error) {
	var messages []map[string]any
	if err := json.Unmarshal([]byte(agenuiJSON), &messages); err != nil {
		return "", errors.New("invalid final AGenUI JSON")
	}
	for _, message := range messages {
		create, ok := message["createSurface"].(map[string]any)
		if !ok {
			continue
		}
		create["catalogId"] = catalogID
		encoded, err := json.Marshal(messages)
		if err != nil {
			return "", fmt.Errorf("encode final AGenUI JSON: %w", err)
		}
		return string(encoded), nil
	}
	return "", errors.New("final AGenUI missing createSurface")
}

func componentNames(agenuiJSON string) (map[string]struct{}, error) {
	var messages []map[string]any
	if err := json.Unmarshal([]byte(agenuiJSON), &messages); err != nil {
		return nil, errors.New("invalid final AGenUI JSON")
	}
	result := make(map[string]struct{})
	for _, message := range messages {
		update, ok := message["updateComponents"].(map[string]any)
		if !ok {
			continue
		}
		components, ok := update["components"].([]any)
		if !ok {
			return nil, errors.New("final AGenUI updateComponents is invalid")
		}
		for _, raw := range components {
			component, ok := raw.(map[string]any)
			if !ok {
				return nil, errors.New("final AGenUI component is invalid")
			}
			name, _ := component["component"].(string)
			if name == "" {
				return nil, errors.New("final AGenUI component name is missing")
			}
			result[name] = struct{}{}
		}
	}
	if len(result) == 0 {
		return nil, errors.New("final AGenUI missing updateComponents")
	}
	return result, nil
}
