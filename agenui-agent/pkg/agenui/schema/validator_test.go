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
	"strings"
	"testing"
)

// newCatalog09 builds a mock v0.9 catalog for testing.
func newCatalog09() *AGenUICatalog {
	s2cSchema := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id":     "https://agenui.org/specification/v0_9/server_to_client.json",
		"title":   "AGenUI Message Schema",
		"oneOf": []any{
			map[string]any{"$ref": "#/$defs/CreateSurfaceMessage"},
			map[string]any{"$ref": "#/$defs/UpdateComponentsMessage"},
			map[string]any{"$ref": "#/$defs/UpdateDataModelMessage"},
			map[string]any{"$ref": "#/$defs/DeleteSurfaceMessage"},
		},
		"$defs": map[string]any{
			"CreateSurfaceMessage": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"version": map[string]any{"const": "v0.9"},
					"createSurface": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"surfaceId": map[string]any{"type": "string"},
							"catalogId": map[string]any{"type": "string"},
							"theme":     map[string]any{"type": "object", "additionalProperties": true},
						},
						"required":             []any{"surfaceId", "catalogId"},
						"additionalProperties": false,
					},
				},
				"required":             []any{"version", "createSurface"},
				"additionalProperties": false,
			},
			"UpdateComponentsMessage": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"version": map[string]any{"const": "v0.9"},
					"updateComponents": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"surfaceId": map[string]any{"type": "string"},
							"components": map[string]any{
								"type":     "array",
								"minItems": 1,
								"items":    map[string]any{"$ref": "catalog.json#/$defs/anyComponent"},
							},
						},
						"required":             []any{"surfaceId", "components"},
						"additionalProperties": false,
					},
				},
				"required":             []any{"version", "updateComponents"},
				"additionalProperties": false,
			},
			"UpdateDataModelMessage": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"version": map[string]any{"const": "v0.9"},
					"updateDataModel": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"surfaceId": map[string]any{"type": "string"},
							"path":      map[string]any{"type": "string"},
							"value":     map[string]any{"additionalProperties": true},
						},
						"required":             []any{"surfaceId"},
						"additionalProperties": false,
					},
				},
				"required":             []any{"version", "updateDataModel"},
				"additionalProperties": false,
			},
			"DeleteSurfaceMessage": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"version": map[string]any{"const": "v0.9"},
					"deleteSurface": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"surfaceId": map[string]any{"type": "string"},
						},
						"required":             []any{"surfaceId"},
						"additionalProperties": false,
					},
				},
				"required":             []any{"deleteSurface", "version"},
				"additionalProperties": false,
			},
		},
	}

	catalogSchema := map[string]any{
		"$schema":   "https://json-schema.org/draft/2020-12/schema",
		"$id":       "https://agenui.org/specification/v0_9/basic_catalog.json",
		"title":     "AGenUI Basic Catalog",
		"catalogId": "https://agenui.dev/specification/v0_9/basic_catalog.json",
		"components": map[string]any{
			"Text": map[string]any{
				"type": "object",
				"allOf": []any{
					map[string]any{"$ref": "common_types.json#/$defs/ComponentCommon"},
					map[string]any{"$ref": "#/$defs/CatalogComponentCommon"},
				},
				"properties": map[string]any{
					"component": map[string]any{"const": "Text"},
					"text":      map[string]any{"$ref": "common_types.json#/$defs/DynamicString"},
				},
				"required":              []any{"component", "text"},
				"unevaluatedProperties": false,
			},
			"Image": map[string]any{
				"type": "object",
				"allOf": []any{
					map[string]any{"$ref": "common_types.json#/$defs/ComponentCommon"},
					map[string]any{"$ref": "#/$defs/CatalogComponentCommon"},
				},
				"properties": map[string]any{
					"component": map[string]any{"const": "Image"},
					"url":       map[string]any{"type": "string"},
				},
				"required":              []any{"component", "url"},
				"unevaluatedProperties": false,
			},
			"Column": map[string]any{
				"type": "object",
				"allOf": []any{
					map[string]any{"$ref": "common_types.json#/$defs/ComponentCommon"},
					map[string]any{"$ref": "#/$defs/CatalogComponentCommon"},
				},
				"properties": map[string]any{
					"component": map[string]any{"const": "Column"},
					"children":  map[string]any{"$ref": "common_types.json#/$defs/ChildList"},
				},
				"required":              []any{"component", "children"},
				"unevaluatedProperties": false,
			},
			"Card": map[string]any{
				"type": "object",
				"allOf": []any{
					map[string]any{"$ref": "common_types.json#/$defs/ComponentCommon"},
					map[string]any{"$ref": "#/$defs/CatalogComponentCommon"},
				},
				"properties": map[string]any{
					"component": map[string]any{"const": "Card"},
					"child":     map[string]any{"$ref": "common_types.json#/$defs/ComponentId"},
				},
				"required":              []any{"component", "child"},
				"unevaluatedProperties": false,
			},
			"Button": map[string]any{
				"type": "object",
				"allOf": []any{
					map[string]any{"$ref": "common_types.json#/$defs/ComponentCommon"},
					map[string]any{"$ref": "#/$defs/CatalogComponentCommon"},
				},
				"properties": map[string]any{
					"component": map[string]any{"const": "Button"},
					"text":      map[string]any{"type": "string"},
					"action":    map[string]any{"$ref": "common_types.json#/$defs/Action"},
				},
				"required":              []any{"component", "text", "action"},
				"unevaluatedProperties": false,
			},
		},
		"$defs": map[string]any{
			"CatalogComponentCommon": map[string]any{
				"type":       "object",
				"properties": map[string]any{"weight": map[string]any{"type": "number"}},
			},
			"anyComponent": map[string]any{
				"oneOf": []any{
					map[string]any{"$ref": "#/components/Text"},
					map[string]any{"$ref": "#/components/Image"},
					map[string]any{"$ref": "#/components/Column"},
					map[string]any{"$ref": "#/components/Card"},
					map[string]any{"$ref": "#/components/Button"},
				},
				"discriminator": map[string]any{"propertyName": "component"},
			},
		},
	}

	commonTypesSchema := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id":     "https://agenui.org/specification/v0_9/common_types.json",
		"title":   "AGenUI Common Types",
		"$defs": map[string]any{
			"ComponentId": map[string]any{"type": "string"},
			"Action":      map[string]any{"type": "object", "additionalProperties": true},
			"ComponentCommon": map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"$ref": "#/$defs/ComponentId"}},
				"required":   []any{"id"},
			},
			"DataBinding": map[string]any{"type": "object"},
			"DynamicString": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"$ref": "#/$defs/DataBinding"},
				},
			},
			"DynamicValue": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "object"},
					map[string]any{"type": "array"},
					map[string]any{"$ref": "#/$defs/DataBinding"},
				},
			},
			"DynamicNumber": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "number"},
					map[string]any{"$ref": "#/$defs/DataBinding"},
				},
			},
			"ChildList": map[string]any{
				"oneOf": []any{
					map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/ComponentId"}},
					map[string]any{
						"type": "object",
						"properties": map[string]any{
							"componentId": map[string]any{"$ref": "#/$defs/ComponentId"},
							"path":        map[string]any{"type": "string"},
						},
						"required":             []any{"componentId", "path"},
						"additionalProperties": false,
					},
				},
			},
		},
	}

	return &AGenUICatalog{
		Version:           Version09,
		Name:              "standard",
		CatalogSchema:     catalogSchema,
		S2CSchema:         s2cSchema,
		CommonTypesSchema: commonTypesSchema,
	}
}

// makePayload09 creates a v0.9 test payload with components.
func makePayload09(components []map[string]any) []any {
	return []any{
		map[string]any{
			"version":       "v0.9",
			"createSurface": map[string]any{"surfaceId": "test-surface", "catalogId": "std"},
		},
		map[string]any{
			"version": "v0.9",
			"updateComponents": map[string]any{
				"surfaceId":  "test-surface",
				"components": mapsToAnySlice(components),
			},
		},
	}
}

func mapsToAnySlice(m []map[string]any) []any {
	s := make([]any, len(m))
	for i, v := range m {
		s[i] = v
	}
	return s
}

func TestValidator09CreateSurface(t *testing.T) {
	catalog := newCatalog09()
	v := NewAGenUIValidator(catalog)

	message := []any{
		map[string]any{
			"version": "v0.9",
			"createSurface": map[string]any{
				"surfaceId": "test-id",
				"catalogId": "standard",
				"theme":     map[string]any{"primaryColor": "blue"},
			},
		},
	}
	if err := v.Validate(message, "root", false); err != nil {
		t.Fatalf("expected valid message, got: %v", err)
	}
}

func TestValidator09MissingVersion(t *testing.T) {
	catalog := newCatalog09()
	v := NewAGenUIValidator(catalog)

	invalid := []any{
		map[string]any{
			"createSurface": map[string]any{"surfaceId": "123", "catalogId": "standard"},
		},
	}
	err := v.Validate(invalid, "root", false)
	if err == nil {
		t.Fatal("expected validation error for missing version")
	}
	if !strings.Contains(err.Error(), "'version' is a required property") &&
		!strings.Contains(err.Error(), "version") {
		t.Errorf("expected version-related error, got: %v", err)
	}
}

func TestValidator09WrongVersion(t *testing.T) {
	catalog := newCatalog09()
	v := NewAGenUIValidator(catalog)

	invalid := []any{
		map[string]any{
			"version":       "0.8",
			"createSurface": map[string]any{"surfaceId": "123", "catalogId": "standard"},
		},
	}
	err := v.Validate(invalid, "root", false)
	if err == nil {
		t.Fatal("expected validation error for wrong version")
	}
	if !strings.Contains(err.Error(), "v0.9") {
		t.Errorf("expected v0.9-related error, got: %v", err)
	}
}

func TestValidator09SurfaceIdMustBeString(t *testing.T) {
	catalog := newCatalog09()
	v := NewAGenUIValidator(catalog)

	invalid := []any{
		map[string]any{
			"version":       "v0.9",
			"createSurface": map[string]any{"surfaceId": 123, "catalogId": "standard"},
		},
	}
	err := v.Validate(invalid, "root", false)
	if err == nil {
		t.Fatal("expected validation error for non-string surfaceId")
	}
	if !strings.Contains(err.Error(), "string") {
		t.Errorf("expected string type error, got: %v", err)
	}
}

func TestValidator09MissingCatalogId(t *testing.T) {
	catalog := newCatalog09()
	v := NewAGenUIValidator(catalog)

	invalid := []any{
		map[string]any{
			"version":       "v0.9",
			"createSurface": map[string]any{"surfaceId": "123"},
		},
	}
	err := v.Validate(invalid, "root", false)
	if err == nil {
		t.Fatal("expected validation error for missing catalogId")
	}
	if !strings.Contains(err.Error(), "catalogId") {
		t.Errorf("expected catalogId-related error, got: %v", err)
	}
}

func TestValidator09UnknownMessageType(t *testing.T) {
	catalog := newCatalog09()
	v := NewAGenUIValidator(catalog)

	invalid := []any{
		map[string]any{"unknownMessage": map[string]any{}},
	}
	err := v.Validate(invalid, "root", false)
	if err == nil {
		t.Fatal("expected validation error for unknown message type")
	}
	if !strings.Contains(err.Error(), "Unknown message type") {
		t.Errorf("expected 'Unknown message type' error, got: %v", err)
	}
}

func TestValidateDuplicateIDs(t *testing.T) {
	catalog := newCatalog09()
	v := NewAGenUIValidator(catalog)

	components := []map[string]any{
		{"id": "root", "component": "Text", "text": "Root"},
		{"id": "c1", "component": "Text", "text": "Hello"},
		{"id": "c1", "component": "Text", "text": "World"},
	}
	payload := makePayload09(components)
	err := v.Validate(payload, "root", true)
	if err == nil {
		t.Fatal("expected error for duplicate IDs")
	}
	if !strings.Contains(err.Error(), "duplicate component ID") {
		t.Errorf("expected duplicate ID error, got: %v", err)
	}
}

func TestValidateMissingRoot(t *testing.T) {
	catalog := newCatalog09()
	v := NewAGenUIValidator(catalog)

	payload := []any{
		map[string]any{
			"version":       "v0.9",
			"createSurface": map[string]any{"surfaceId": "test", "catalogId": "std"},
		},
		map[string]any{
			"version": "v0.9",
			"updateComponents": map[string]any{
				"surfaceId": "test",
				"components": []any{
					map[string]any{"id": "c1", "component": "Text", "text": "hi"},
				},
			},
		},
	}
	err := v.Validate(payload, "root", true)
	if err == nil {
		t.Fatal("expected error for missing root component")
	}
	if !strings.Contains(err.Error(), "missing root component") {
		t.Errorf("expected missing root error, got: %v", err)
	}
}

func TestValidateDanglingChildrenRef(t *testing.T) {
	catalog := newCatalog09()
	v := NewAGenUIValidator(catalog)

	components := []map[string]any{
		{"id": "root", "component": "Column", "children": []any{"missing"}},
	}
	payload := makePayload09(components)
	err := v.Validate(payload, "root", true)
	if err == nil {
		t.Fatal("expected error for dangling reference")
	}
	if !strings.Contains(err.Error(), "references non-existent component") {
		t.Errorf("expected dangling ref error, got: %v", err)
	}
}

func TestValidateDanglingChildRef(t *testing.T) {
	catalog := newCatalog09()
	v := NewAGenUIValidator(catalog)

	components := []map[string]any{
		{"id": "root", "component": "Card", "child": "missing"},
	}
	payload := makePayload09(components)
	err := v.Validate(payload, "root", true)
	if err == nil {
		t.Fatal("expected error for dangling child reference")
	}
	if !strings.Contains(err.Error(), "references non-existent component") {
		t.Errorf("expected dangling ref error, got: %v", err)
	}
}

func TestFindRootIDV09(t *testing.T) {
	// createSurface with explicit root
	messages := []map[string]any{
		{"createSurface": map[string]any{"surfaceId": "s1", "root": "custom-root"}},
		{"updateComponents": map[string]any{"surfaceId": "s1", "components": []any{}}},
	}
	rootID := findRootID(messages, "")
	if rootID != "custom-root" {
		t.Errorf("expected custom-root, got %q", rootID)
	}

	// createSurface without explicit root defaults to "root"
	messages = []map[string]any{
		{"createSurface": map[string]any{"surfaceId": "s1"}},
		{"updateComponents": map[string]any{"surfaceId": "s1", "components": []any{}}},
	}
	rootID = findRootID(messages, "")
	if rootID != "root" {
		t.Errorf("expected 'root', got %q", rootID)
	}

	// Incremental update without createSurface returns empty
	messages = []map[string]any{
		{"updateComponents": map[string]any{"surfaceId": "s1", "components": []any{}}},
	}
	rootID = findRootID(messages, "")
	if rootID != "" {
		t.Errorf("expected empty string, got %q", rootID)
	}
}

func TestAnalyzeTopologyCircular(t *testing.T) {
	refMap := RefFieldsMap{
		"Node": {SingleRefs: map[string]struct{}{"next": {}}, ListRefs: map[string]struct{}{}},
	}
	components := []map[string]any{
		{"id": "c1", "component": "Node", "next": "c2"},
		{"id": "c2", "component": "Node", "next": "c1"},
	}
	_, err := AnalyzeTopology("c1", components, refMap, true)
	if err == nil {
		t.Fatal("expected circular reference error")
	}
	if !strings.Contains(err.Error(), "circular reference") {
		t.Errorf("expected circular reference error, got: %v", err)
	}
}

func TestAnalyzeTopologySelfRef(t *testing.T) {
	refMap := RefFieldsMap{
		"Node": {SingleRefs: map[string]struct{}{"next": {}}, ListRefs: map[string]struct{}{}},
	}
	components := []map[string]any{
		{"id": "c1", "component": "Node", "next": "c1"},
	}
	_, err := AnalyzeTopology("c1", components, refMap, true)
	if err == nil {
		t.Fatal("expected self-reference error")
	}
	if !strings.Contains(err.Error(), "self-reference") {
		t.Errorf("expected self-reference error, got: %v", err)
	}
}

func TestAnalyzeTopologyReachable(t *testing.T) {
	refMap := RefFieldsMap{
		"Node": {SingleRefs: map[string]struct{}{"next": {}}, ListRefs: map[string]struct{}{}},
	}
	components := []map[string]any{
		{"id": "root", "component": "Node", "next": "c1"},
		{"id": "c1", "component": "Node", "next": "c2"},
		{"id": "c2", "component": "Node"},
		{"id": "orphan", "component": "Node"},
	}
	reachable, err := AnalyzeTopology("root", components, refMap, false)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	for _, expected := range []string{"root", "c1", "c2"} {
		if _, ok := reachable[expected]; !ok {
			t.Errorf("expected %q to be reachable", expected)
		}
	}
	if _, ok := reachable["orphan"]; ok {
		t.Error("orphan should not be reachable")
	}
}

func TestAnalyzeTopologyOrphansRaise(t *testing.T) {
	refMap := RefFieldsMap{
		"Node": {SingleRefs: map[string]struct{}{"next": {}}, ListRefs: map[string]struct{}{}},
	}
	components := []map[string]any{
		{"id": "root", "component": "Node", "next": "c1"},
		{"id": "c1", "component": "Node"},
		{"id": "orphan", "component": "Node"},
	}
	_, err := AnalyzeTopology("root", components, refMap, true)
	if err == nil {
		t.Fatal("expected orphan error")
	}
	if !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("expected 'not reachable' error, got: %v", err)
	}
}

func TestExtractComponentRefFieldsMock(t *testing.T) {
	catalog := &AGenUICatalog{
		Version: Version09,
		CatalogSchema: map[string]any{
			"components": map[string]any{
				"Container": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"child":    map[string]any{"$ref": "common_types.json#/$defs/ComponentId"},
						"children": map[string]any{"$ref": "common_types.json#/$defs/ChildList"},
						"text":     map[string]any{"type": "string"},
					},
				},
				"Text": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"text": map[string]any{"type": "string"},
					},
				},
			},
		},
	}

	refMap := ExtractComponentRefFields(catalog)

	// Container should have child as single ref and children as list ref
	containerRefs, ok := refMap["Container"]
	if !ok {
		t.Fatal("expected Container in refMap")
	}
	if _, ok := containerRefs.SingleRefs["child"]; !ok {
		t.Error("expected 'child' in Container.SingleRefs")
	}
	if _, ok := containerRefs.ListRefs["children"]; !ok {
		t.Error("expected 'children' in Container.ListRefs")
	}
}

func TestGetComponentReferencesFlat(t *testing.T) {
	refMap := RefFieldsMap{
		"Container": {
			SingleRefs: map[string]struct{}{"child": {}},
			ListRefs:   map[string]struct{}{"children": {}},
		},
	}
	comp := map[string]any{
		"id":        "c1",
		"component": "Container",
		"child":     "c2",
		"children":  []any{"c3", "c4"},
	}
	refs := getComponentReferencesFlat(comp, refMap)

	if refs["c2"] != "child" {
		t.Errorf("expected c2 -> child, got %v", refs["c2"])
	}
	if refs["c3"] != "children" {
		t.Errorf("expected c3 -> children, got %v", refs["c3"])
	}
	if refs["c4"] != "children" {
		t.Errorf("expected c4 -> children, got %v", refs["c4"])
	}
}

func TestValidateRecursionAndPathsValid(t *testing.T) {
	data := map[string]any{
		"path": "/valid/path",
		"nested": map[string]any{
			"path": "relative/path",
		},
	}
	if err := ValidateRecursionAndPaths(data); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidateRecursionAndPathsInvalid(t *testing.T) {
	data := map[string]any{
		"path": "invalid//~3path",
	}
	err := ValidateRecursionAndPaths(data)
	if err == nil {
		t.Fatal("expected error for invalid path syntax")
	}
	if !strings.Contains(err.Error(), "invalid path syntax") {
		t.Errorf("expected 'invalid path syntax' error, got: %v", err)
	}
}

func TestValidateRecursionAndPathsFuncCallDepth(t *testing.T) {
	// Create deeply nested function calls exceeding MaxFuncCallDepth
	innermost := map[string]any{
		"call": "f5",
		"args": map[string]any{"value": "end"},
	}
	current := innermost
	for i := 4; i >= 0; i-- {
		current = map[string]any{
			"call": "f",
			"args": current,
		}
	}
	err := ValidateRecursionAndPaths(current)
	if err == nil {
		t.Fatal("expected recursion limit error")
	}
	if !strings.Contains(err.Error(), "recursion limit exceeded") {
		t.Errorf("expected recursion limit error, got: %v", err)
	}
}

func TestValidateComponentIntegrityValid(t *testing.T) {
	refMap := RefFieldsMap{
		"Column": {SingleRefs: map[string]struct{}{}, ListRefs: map[string]struct{}{"children": {}}},
		"Text":   {},
	}
	components := []map[string]any{
		{"id": "root", "component": "Column", "children": []any{"c1"}},
		{"id": "c1", "component": "Text", "text": "Hello"},
	}
	err := ValidateComponentIntegrity("root", components, refMap, false)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidator09UpdateComponents(t *testing.T) {
	catalog := newCatalog09()
	v := NewAGenUIValidator(catalog)

	// Valid updateComponents message
	payload := []any{
		map[string]any{
			"version":       "v0.9",
			"createSurface": map[string]any{"surfaceId": "s1", "catalogId": "std"},
		},
		map[string]any{
			"version": "v0.9",
			"updateComponents": map[string]any{
				"surfaceId": "s1",
				"components": []any{
					map[string]any{"id": "root", "component": "Text", "text": "Hello"},
				},
			},
		},
	}
	if err := v.Validate(payload, "root", true); err != nil {
		t.Fatalf("expected valid payload, got: %v", err)
	}
}

func TestValidator09UpdateDataModel(t *testing.T) {
	catalog := newCatalog09()
	v := NewAGenUIValidator(catalog)

	payload := []any{
		map[string]any{
			"version": "v0.9",
			"updateDataModel": map[string]any{
				"surfaceId": "s1",
				"value":     map[string]any{"key": "val"},
			},
		},
	}
	if err := v.Validate(payload, "", false); err != nil {
		t.Fatalf("expected valid payload, got: %v", err)
	}
}

func TestValidator09DeleteSurface(t *testing.T) {
	catalog := newCatalog09()
	v := NewAGenUIValidator(catalog)

	payload := []any{
		map[string]any{
			"version":       "v0.9",
			"deleteSurface": map[string]any{"surfaceId": "s1"},
		},
	}
	if err := v.Validate(payload, "", false); err != nil {
		t.Fatalf("expected valid payload, got: %v", err)
	}
}

func TestValidator09DeleteSurfaceMissingSurfaceId(t *testing.T) {
	catalog := newCatalog09()
	v := NewAGenUIValidator(catalog)

	payload := []any{
		map[string]any{
			"version":       "v0.9",
			"deleteSurface": map[string]any{},
		},
	}
	err := v.Validate(payload, "", false)
	if err == nil {
		t.Fatal("expected validation error for missing surfaceId")
	}
	if !strings.Contains(err.Error(), "surfaceId") {
		t.Errorf("expected surfaceId-related error, got: %v", err)
	}
}

func TestValidatorNilCatalog(t *testing.T) {
	v := NewAGenUIValidator(nil)
	// With nil catalog, fallback path runs (no schema validation, only integrity)
	payload := map[string]any{"foo": "bar"}
	// Should not panic
	_ = v.Validate(payload, "", false)
}

func TestPrettyErrorMessages(t *testing.T) {
	catalog := newCatalog09()
	v := NewAGenUIValidator(catalog)

	payload := []any{
		map[string]any{
			"version": "v0.9",
			"createSurface": map[string]any{
				"surfaceId": "recipe-card",
				"catalogId": "https://agenui.org/specification/v0_9/basic_catalog.json",
			},
		},
		map[string]any{
			"version": "v0.9",
			"updateComponents": map[string]any{
				"surfaceId": "recipe-card",
				"components": []any{
					map[string]any{
						"id": "main-column", "component": "Column",
						"children": []any{"recipe-image"},
						"gap":      "small",
					},
					map[string]any{
						"id": "recipe-image", "component": "Image",
						"url":     map[string]any{"path": "/image"},
						"altText": map[string]any{"path": "/title"},
						"fit":     "cover",
					},
				},
			},
		},
		map[string]any{"unknownMessage": map[string]any{}},
	}

	err := v.Validate(payload, "root", true)
	if err == nil {
		t.Fatal("expected validation errors")
	}

	errText := err.Error()
	// Check that we get meaningful error messages
	if !strings.Contains(errText, "Unknown message type") {
		t.Errorf("expected 'Unknown message type' in error, got: %v", errText)
	}
}

func TestValidateSingleMessage(t *testing.T) {
	catalog := newCatalog09()
	v := NewAGenUIValidator(catalog)

	// Single message (not wrapped in array)
	message := map[string]any{
		"version": "v0.9",
		"createSurface": map[string]any{
			"surfaceId": "test-id",
			"catalogId": "standard",
		},
	}
	if err := v.Validate(message, "root", false); err != nil {
		t.Fatalf("expected valid single message, got: %v", err)
	}
}

func TestValidateInvalidType(t *testing.T) {
	catalog := newCatalog09()
	v := NewAGenUIValidator(catalog)

	err := v.Validate("not a valid type", "root", false)
	if err == nil {
		t.Fatal("expected error for invalid type")
	}
	if !strings.Contains(err.Error(), "must be a dict or list") {
		t.Errorf("expected type error, got: %v", err)
	}
}

// newCatalog09WithStylesAndAlign builds a v0.9 catalog whose Text component
// declares a strict styles sub-schema AND a strict align enum. It exists for
// two regression suites in this file:
//
//  1. TestValidator09_StylesErrorsDropped — locks in the contract that every
//     validation leaf rooted under "styles" is suppressed (catalog vocabulary
//     intentionally permissive for styling). The strict styles sub-schema
//     here is the test fixture — the production policy is "model can write
//     anything under styles", and these tests prove the suppression works
//     even when the catalog itself would have rejected the input.
//
//  2. TestValidator09_EnumErrorIncludesAllowedList — locks in the
//     human-readable enum error format. Uses the align field (NOT styles)
//     because styles errors are dropped by the policy in (1), so an enum
//     under styles would never bubble up to assert against.
//
// Kept separate from newCatalog09() so the existing test suite's expectations
// (no styles, no enum) stay untouched.
func newCatalog09WithStylesAndAlign() *AGenUICatalog {
	catalog := newCatalog09()
	components, _ := catalog.CatalogSchema["components"].(map[string]any)
	text, _ := components["Text"].(map[string]any)
	props, _ := text["properties"].(map[string]any)
	props["styles"] = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"color":    map[string]any{"type": "string", "enum": []any{"red", "green", "blue"}},
			"fontSize": map[string]any{"type": "number"},
		},
		"additionalProperties": false,
	}
	props["align"] = map[string]any{
		"type": "string",
		"enum": []any{"start", "center", "end"},
	}
	return catalog
}

// TestValidator09_StylesErrorsDropped verifies that every validation leaf
// rooted under a component's "styles" object is suppressed, regardless of
// the underlying schema-keyword failure (enum, type, additionalProperties).
//
// The fixture catalog declares strict styles sub-schemas just to *generate*
// failures on demand; the production catalog's styles vocabulary is
// effectively open. The whole point is that even a strict declaration must
// not surface to the model — it would block legal styling output.
//
// The cases collectively cover the three concrete leaf error kinds the
// jsonschema library can produce inside styles:
//   - enum (color="purple")
//   - type mismatch (fontSize="big")
//   - additionalProperties (unknown="oops")
//
// Plus a negative control on the same Text component to prove the
// suppression is scoped to "styles" and doesn't accidentally hide
// non-styles enum failures (align="weird" still reports).
func TestValidator09_StylesErrorsDropped(t *testing.T) {
	catalog := newCatalog09WithStylesAndAlign()
	v := NewAGenUIValidator(catalog)

	cases := []struct {
		name       string
		styles     map[string]any
		shouldPass bool
	}{
		{
			name:       "enum_violation_dropped",
			styles:     map[string]any{"color": "purple"},
			shouldPass: true,
		},
		{
			name:       "type_violation_dropped",
			styles:     map[string]any{"fontSize": "big"},
			shouldPass: true,
		},
		{
			name:       "additional_properties_dropped",
			styles:     map[string]any{"unknown": "oops"},
			shouldPass: true,
		},
		{
			name:       "valid_styles_passes",
			styles:     map[string]any{"color": "red", "fontSize": float64(12)},
			shouldPass: true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			payload := makePayload09([]map[string]any{
				{
					"id":        "root",
					"component": "Text",
					"text":      "hello",
					"styles":    tc.styles,
				},
			})
			err := v.Validate(payload, "root", true)
			if tc.shouldPass && err != nil {
				t.Errorf("expected styles-only error to be dropped, got: %v", err)
			}
			if !tc.shouldPass && err == nil {
				t.Errorf("expected validation failure, got nil")
			}
		})
	}
}

// TestValidator09_StylesDropDoesNotMaskNonStylesErrors is the negative
// control for the styles-suppression policy. If isStylesLocation ever
// regressed to "skip everything" or "skip the whole component", these
// non-styles failures would silently disappear and the model would
// stop getting actionable retry feedback.
//
// align="weird" hits the strict align enum on Text, which lives at the
// component root (NOT under styles), so it MUST still surface. Combining
// it with a styles violation on the same component proves the two
// channels are independent: styles drops, align reports.
func TestValidator09_StylesDropDoesNotMaskNonStylesErrors(t *testing.T) {
	catalog := newCatalog09WithStylesAndAlign()
	v := NewAGenUIValidator(catalog)

	payload := makePayload09([]map[string]any{
		{
			"id":        "root",
			"component": "Text",
			"text":      "hello",
			"styles":    map[string]any{"color": "purple"}, // would fail, but dropped
			"align":     "weird",                           // must surface
		},
	})
	err := v.Validate(payload, "root", true)
	if err == nil {
		t.Fatal("expected align enum error to surface")
	}
	if !strings.Contains(err.Error(), "align") {
		t.Errorf("expected error to mention 'align', got: %v", err.Error())
	}
	// Belt-and-suspenders: also assert no styles leak occurred. If the
	// suppression broke and styles errors started showing up alongside,
	// this catches it without depending on exact wording.
	if strings.Contains(err.Error(), "styles") {
		t.Errorf("styles-rooted error leaked into output: %v", err.Error())
	}
}

// TestValidator09_EnumErrorIncludesAllowedList locks in the human/LLM
// readable format introduced for kind.Enum failures: the message must
// quote the offending value AND list every allowed value in a stable,
// sorted "(allowed: 'a', 'b', 'c')" suffix.
//
// Uses the align field (catalog root), NOT styles, because styles errors
// are dropped — see newCatalog09WithStylesAndAlign's block comment.
//
// Sorted-output check: the catalog declares enum order start, center,
// end (declaration order), but formatEnumError sorts alphabetically so
// log diffs stay stable across catalog refreshes that reorder enums.
func TestValidator09_EnumErrorIncludesAllowedList(t *testing.T) {
	catalog := newCatalog09WithStylesAndAlign()
	v := NewAGenUIValidator(catalog)

	payload := makePayload09([]map[string]any{
		{
			"id":        "root",
			"component": "Text",
			"text":      "hello",
			"align":     "weird",
		},
	})
	err := v.Validate(payload, "root", true)
	if err == nil {
		t.Fatal("expected enum validation error")
	}
	got := err.Error()

	// 1. Must report the offending value, single-quoted.
	if !strings.Contains(got, "'weird'") {
		t.Errorf("expected error to quote bad value 'weird', got: %v", got)
	}
	// 2. Must include the canonical "is not one of the allowed values"
	//    leader so callers (sanitizeSchemaError, retry feedback) can
	//    keyword-match the same as before.
	if !strings.Contains(got, "is not one of the allowed values") {
		t.Errorf("expected canonical enum leader, got: %v", got)
	}
	// 3. Must include the allowed-list suffix in alphabetical order.
	//    Whole substring asserted so a regression in sort order trips
	//    the test (e.g. accidentally returning declaration order again).
	const wantSuffix = "(allowed: 'center', 'end', 'start')"
	if !strings.Contains(got, wantSuffix) {
		t.Errorf("expected sorted allowed-list suffix %q, got: %v", wantSuffix, got)
	}
}
