// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package schema

import (
	"fmt"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/agenui"
)

// SchemaModifier is a function that transforms a schema map.
type SchemaModifier func(map[string]any) map[string]any

// AGenUISchemaManager manages AGenUI schema levels and prompt injection.
// It implements the agenui.InferenceStrategy interface.
type AGenUISchemaManager struct {
	version               string
	acceptsInlineCatalogs bool
	serverToClientSchema  map[string]any
	commonTypesSchema     map[string]any
	supportedCatalogs     []*AGenUICatalog
	catalogExamplePaths   map[string]string
	schemaModifiers       []SchemaModifier
}

// AGenUISchemaManagerConfig holds configuration for creating an AGenUISchemaManager.
type AGenUISchemaManagerConfig struct {
	Version               string
	Catalogs              []*CatalogConfig
	AcceptsInlineCatalogs bool
	SchemaModifiers       []SchemaModifier
}

// NewAGenUISchemaManager creates a new AGenUISchemaManager.
func NewAGenUISchemaManager(cfg AGenUISchemaManagerConfig) (*AGenUISchemaManager, error) {
	m := &AGenUISchemaManager{
		version:               cfg.Version,
		acceptsInlineCatalogs: cfg.AcceptsInlineCatalogs,
		catalogExamplePaths:   make(map[string]string),
		schemaModifiers:       cfg.SchemaModifiers,
	}

	if err := m.loadSchemas(cfg.Version, cfg.Catalogs); err != nil {
		return nil, err
	}

	return m, nil
}

// AcceptsInlineCatalogs returns whether the manager accepts inline catalogs.
func (m *AGenUISchemaManager) AcceptsInlineCatalogs() bool {
	return m.acceptsInlineCatalogs
}

// SupportedCatalogIDs returns the list of supported catalog IDs.
func (m *AGenUISchemaManager) SupportedCatalogIDs() []string {
	var ids []string
	for _, c := range m.supportedCatalogs {
		if id, err := c.CatalogID(); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func (m *AGenUISchemaManager) applyModifiers(schema map[string]any) map[string]any {
	for _, modifier := range m.schemaModifiers {
		schema = modifier(schema)
	}
	return schema
}

func (m *AGenUISchemaManager) loadSchemas(version string, catalogs []*CatalogConfig) error {
	if _, ok := SpecVersionMap[version]; !ok {
		return fmt.Errorf("unknown AGenUI specification version: %s. Supported: %v", version, supportedVersions())
	}

	// Load server-to-client schema
	s2c, err := LoadFromBundledResource(version, ServerToClientSchemaKey, SpecVersionMap)
	if err != nil {
		return fmt.Errorf("failed to load server-to-client schema: %w", err)
	}
	if s2c != nil {
		m.serverToClientSchema = m.applyModifiers(s2c)
	}

	// Load common types schema
	ct, err := LoadFromBundledResource(version, CommonTypesSchemaKey, SpecVersionMap)
	if err != nil {
		return fmt.Errorf("failed to load common types schema: %w", err)
	}
	if ct != nil {
		m.commonTypesSchema = m.applyModifiers(ct)
	}

	// Process catalogs
	for _, config := range catalogs {
		catalogSchema, err := config.Provider.Load()
		if err != nil {
			return fmt.Errorf("failed to load catalog '%s': %w", config.Name, err)
		}
		catalogSchema = m.applyModifiers(catalogSchema)

		catalog := &AGenUICatalog{
			Version:           version,
			Name:              config.Name,
			CatalogSchema:     catalogSchema,
			S2CSchema:         m.serverToClientSchema,
			CommonTypesSchema: m.commonTypesSchema,
		}
		m.supportedCatalogs = append(m.supportedCatalogs, catalog)

		catalogID, err := catalog.CatalogID()
		if err == nil {
			m.catalogExamplePaths[catalogID] = config.ExamplesPath
		}
	}

	return nil
}

// selectCatalog selects the component catalog based on client capabilities.
func (m *AGenUISchemaManager) selectCatalog(clientUICapabilities map[string]any) (*AGenUICatalog, error) {
	if len(m.supportedCatalogs) == 0 {
		return nil, fmt.Errorf("no supported catalogs found")
	}

	if len(clientUICapabilities) == 0 {
		return m.supportedCatalogs[0], nil
	}

	inlineCatalogs, _ := clientUICapabilities[InlineCatalogsKey].([]any)
	clientSupportedIDs, _ := clientUICapabilities[SupportedCatalogIDsKey].([]any)

	if !m.acceptsInlineCatalogs && len(inlineCatalogs) > 0 {
		return nil, fmt.Errorf("inline catalog '%s' is provided in client UI capabilities, but the agent does not accept inline catalogs", InlineCatalogsKey)
	}

	if len(inlineCatalogs) > 0 {
		baseCatalog := m.supportedCatalogs[0]
		if len(clientSupportedIDs) > 0 {
			agentCatalogs := m.catalogsByID()
			for _, cscid := range clientSupportedIDs {
				if id, ok := cscid.(string); ok {
					if c, exists := agentCatalogs[id]; exists {
						baseCatalog = c
						break
					}
				}
			}
		}

		mergedSchema := deepCopyMap(baseCatalog.CatalogSchema)
		for _, inlineCatalogRaw := range inlineCatalogs {
			inlineCatalogSchema, ok := inlineCatalogRaw.(map[string]any)
			if !ok {
				continue
			}
			inlineCatalogSchema = m.applyModifiers(inlineCatalogSchema)
			if inlineComps, ok := inlineCatalogSchema[CatalogComponentsKey].(map[string]any); ok {
				if mergedComps, ok := mergedSchema[CatalogComponentsKey].(map[string]any); ok {
					for k, v := range inlineComps {
						mergedComps[k] = v
					}
				}
			}
			// Merge $defs from inline catalog so that local $ref references remain valid.
			if inlineDefs, ok := inlineCatalogSchema["$defs"].(map[string]any); ok {
				if mergedDefs, ok := mergedSchema["$defs"].(map[string]any); ok {
					for k, v := range inlineDefs {
						mergedDefs[k] = v
					}
				} else {
					mergedSchema["$defs"] = deepCopyMap(inlineDefs)
				}
			}
		}

		return &AGenUICatalog{
			Version:           m.version,
			Name:              InlineCatalogName,
			CatalogSchema:     mergedSchema,
			S2CSchema:         m.serverToClientSchema,
			CommonTypesSchema: m.commonTypesSchema,
		}, nil
	}

	if len(clientSupportedIDs) == 0 {
		return m.supportedCatalogs[0], nil
	}

	agentCatalogs := m.catalogsByID()
	for _, cscid := range clientSupportedIDs {
		if id, ok := cscid.(string); ok {
			if c, exists := agentCatalogs[id]; exists {
				return c, nil
			}
		}
	}

	var agentIDs []string
	for _, c := range m.supportedCatalogs {
		if id, err := c.CatalogID(); err == nil {
			agentIDs = append(agentIDs, id)
		}
	}
	return nil, fmt.Errorf("no client-supported catalog found on the agent side. Agent-supported catalogs are: %v", agentIDs)
}

func (m *AGenUISchemaManager) catalogsByID() map[string]*AGenUICatalog {
	result := make(map[string]*AGenUICatalog, len(m.supportedCatalogs))
	for _, c := range m.supportedCatalogs {
		if id, err := c.CatalogID(); err == nil {
			result[id] = c
		}
	}
	return result
}

// GetSelectedCatalog gets the selected catalog after selection and component pruning.
func (m *AGenUISchemaManager) GetSelectedCatalog(clientUICapabilities map[string]any, allowedComponents, allowedMessages []string) (*AGenUICatalog, error) {
	catalog, err := m.selectCatalog(clientUICapabilities)
	if err != nil {
		return nil, err
	}
	return catalog.WithPruning(allowedComponents, allowedMessages), nil
}

// LoadExamples loads examples for a catalog.
func (m *AGenUISchemaManager) LoadExamples(catalog *AGenUICatalog, validate bool) (string, error) {
	catalogID, err := catalog.CatalogID()
	if err != nil {
		return "", nil
	}
	if path, ok := m.catalogExamplePaths[catalogID]; ok {
		return catalog.LoadExamples(path, validate)
	}
	return "", nil
}

// GenerateSystemPrompt assembles the final system instruction for the LLM.
// This method implements the agenui.InferenceStrategy interface.
func (m *AGenUISchemaManager) GenerateSystemPrompt(opts agenui.SystemPromptOptions) string {
	var parts []string
	parts = append(parts, opts.RoleDescription)

	workflow := DefaultWorkflowRules
	if opts.WorkflowDescription != "" {
		workflow += opts.WorkflowDescription
	}
	parts = append(parts, fmt.Sprintf("## Workflow Description:\n%s", workflow))

	if opts.UIDescription != "" {
		parts = append(parts, fmt.Sprintf("## UI Description:\n%s", opts.UIDescription))
	}

	selectedCatalog, err := m.GetSelectedCatalog(
		opts.ClientUICapabilities,
		opts.AllowedComponents,
		opts.AllowedMessages,
	)
	if err != nil {
		// If catalog selection fails, continue without schema
		parts = append(parts, fmt.Sprintf("<!-- Warning: catalog selection failed: %s -->", err.Error()))
	} else {
		if opts.IncludeSchema {
			parts = append(parts, selectedCatalog.RenderAsLLMInstructions())
		}

		if opts.IncludeExamples {
			examplesStr, err := m.LoadExamples(selectedCatalog, opts.ValidateExamples)
			if err == nil && examplesStr != "" {
				parts = append(parts, fmt.Sprintf("### Examples:\n%s", examplesStr))
			}
		}
	}

	return strings.Join(parts, "\n\n")
}

func supportedVersions() []string {
	versions := make([]string, 0, len(SpecVersionMap))
	for k := range SpecVersionMap {
		versions = append(versions, k)
	}
	return versions
}

// Ensure AGenUISchemaManager implements agenui.InferenceStrategy at compile time.
var _ agenui.InferenceStrategy = (*AGenUISchemaManager)(nil)
