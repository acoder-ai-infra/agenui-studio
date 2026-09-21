// Package workspace owns the mutable, versioned AGenUI document manipulated by
// model-facing tools. It contains no intent recognition: the model chooses what
// to change, while this package applies exact mutations and enforces boundaries.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	SchemaVersion  = "agenui.workspace/v1"
	ProtocolRootID = "root"
)

var (
	ErrRevisionConflict  = errors.New("agenui workspace: base revision does not match")
	ErrProtectedMutation = errors.New("agenui workspace: mutation is outside the editable set")
)

// SlotIdentityConflictError identifies the exact Host-owned semantic identity
// that was declared with two different payloads. Tool adapters may project
// these facts directly into a model-correctable response without parsing text.
type SlotIdentityConflictError struct {
	Kind         string
	ComponentID  string
	ContractID   string
	PropertyPath string
}

func (e *SlotIdentityConflictError) Error() string {
	return fmt.Sprintf(
		"agenui workspace: conflicting %s slot identity component=%q contract=%q property=%q",
		e.Kind, e.ComponentID, e.ContractID, e.PropertyPath,
	)
}

// Document is the authoring representation of one surface. Protocol envelopes
// are derived from it, so the model never has to maintain message wrappers.
type Document struct {
	SchemaVersion          string           `json:"schema_version"`
	SurfaceID              string           `json:"surface_id"`
	CatalogID              string           `json:"catalog_id"`
	RootID                 string           `json:"root_id"`
	Components             []map[string]any `json:"components"`
	DataModel              map[string]any   `json:"data_model"`
	FieldSlots             []map[string]any `json:"field_slots,omitempty"`
	ActionSlots            []map[string]any `json:"action_slots,omitempty"`
	DesignKnowledgeReceipt map[string]any   `json:"design_knowledge_receipt,omitempty"`
}

// NormalizeRuntimeDataModel converts the common JSON-Schema-shaped authoring
// mistake into the runtime value tree required by AGenUI updateDataModel. The
// conversion is generic and deterministic; it never adds business fields that
// were not declared by the caller.
func (d *Document) NormalizeRuntimeDataModel() bool {
	if d == nil || !looksLikeJSONSchema(d.DataModel) {
		return false
	}
	normalized, _ := schemaZeroValue(d.DataModel).(map[string]any)
	d.DataModel = normalized
	return true
}

// CanonicalizeSlotIDs turns component-local slot names into stable document
// identities. Models may naturally use "text" on several components; the Host
// owns the global identity by prefixing the real component id.
func (d *Document) CanonicalizeSlotIDs() {
	if d == nil {
		return
	}
	canonicalizeSlots(d.FieldSlots)
	canonicalizeSlots(d.ActionSlots)
}

// NormalizeSlotIdentities makes slot identity a Host-owned structural fact.
// Models describe the component and frozen Contract identity; they never need
// to coordinate globally unique IDs. Exact duplicate declarations collapse,
// while conflicting declarations for the same semantic identity are rejected.
func (d *Document) NormalizeSlotIdentities() error {
	if d == nil {
		return nil
	}
	fields, err := normalizeSlotIdentities("field", d.FieldSlots)
	if err != nil {
		return err
	}
	actions, err := normalizeSlotIdentities("action", d.ActionSlots)
	if err != nil {
		return err
	}
	d.FieldSlots, d.ActionSlots = fields, actions
	return nil
}

func normalizeSlotIdentities(kind string, source []map[string]any) ([]map[string]any, error) {
	result := make([]map[string]any, 0, len(source))
	seen := make(map[string][]byte, len(source))
	for index, original := range source {
		slot := cloneMap(original)
		componentID := strings.TrimSpace(mapStringValue(slot, "componentId", "component_id"))
		contractKey := "contractItemId"
		contractID := strings.TrimSpace(mapStringValue(slot, contractKey, "contract_item_id"))
		if kind == "action" {
			contractKey = "contractActionId"
			contractID = strings.TrimSpace(mapStringValue(slot, contractKey, "contract_action_id"))
		}
		if componentID == "" || contractID == "" {
			return nil, fmt.Errorf(
				"agenui workspace: %s slot %d requires componentId and %s",
				kind, index, contractKey,
			)
		}
		role := strings.TrimSpace(mapStringValue(slot, "role"))
		property := strings.TrimSpace(mapStringValue(slot, "refKey", "ref_key"))
		semanticKey := strings.Join([]string{componentID, contractID, role, property}, "\x00")
		if kind == "action" {
			semanticKey = strings.Join([]string{componentID, contractID}, "\x00")
		}
		delete(slot, "slotId")
		delete(slot, "id")
		slot["slotId"] = stableSlotID(kind, componentID, contractID, semanticKey)
		canonical, err := json.Marshal(slot)
		if err != nil {
			return nil, fmt.Errorf("agenui workspace: encode %s slot identity: %w", kind, err)
		}
		if previous, exists := seen[semanticKey]; exists {
			if string(previous) == string(canonical) {
				continue
			}
			return nil, &SlotIdentityConflictError{
				Kind: kind, ComponentID: componentID, ContractID: contractID, PropertyPath: property,
			}
		}
		seen[semanticKey] = canonical
		result = append(result, slot)
	}
	sort.Slice(result, func(i, j int) bool { return slotID(result[i]) < slotID(result[j]) })
	return result, nil
}

func stableSlotID(kind, componentID, contractID, semanticKey string) string {
	sum := sha256.Sum256([]byte(semanticKey))
	return componentID + "." + kind + "." + contractID + "." + hex.EncodeToString(sum[:4])
}

func mapStringValue(source map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := source[key].(string); ok {
			return value
		}
	}
	return ""
}

func canonicalizeSlots(slots []map[string]any) {
	for _, slot := range slots {
		componentID, _ := slot["componentId"].(string)
		if componentID == "" {
			componentID, _ = slot["component_id"].(string)
		}
		localID := slotID(slot)
		if componentID == "" || localID == "" || localID == componentID ||
			strings.HasPrefix(localID, componentID+".") {
			continue
		}
		slot["slotId"] = componentID + "." + localID
		delete(slot, "id")
	}
}

func looksLikeJSONSchema(value map[string]any) bool {
	typeName, _ := value["type"].(string)
	properties, ok := value["properties"].(map[string]any)
	return typeName == "object" && ok && properties != nil
}

func schemaZeroValue(schema map[string]any) any {
	switch typeName, _ := schema["type"].(string); typeName {
	case "object":
		result := make(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		for key, raw := range properties {
			property, _ := raw.(map[string]any)
			result[key] = schemaZeroValue(property)
		}
		return result
	case "array":
		return []any{}
	case "boolean":
		return false
	case "integer", "number":
		return 0
	default:
		return ""
	}
}

// Revision is a content-derived optimistic concurrency token.
func (d Document) Revision() (string, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("agenui workspace: encode revision: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Messages renders the canonical AGenUI v0.9 server-to-client messages.
func (d Document) Messages() []map[string]any {
	return []map[string]any{
		{"version": "v0.9", "createSurface": map[string]any{
			"surfaceId": d.SurfaceID, "catalogId": d.CatalogID,
		}},
		{"version": "v0.9", "updateComponents": map[string]any{
			"surfaceId": d.SurfaceID, "components": cloneMaps(d.Components),
		}},
		{"version": "v0.9", "updateDataModel": map[string]any{
			"surfaceId": d.SurfaceID, "path": "/", "value": cloneMap(d.DataModel),
		}},
	}
}

func (d Document) MarshalMessages() (string, error) {
	raw, err := json.Marshal(d.Messages())
	if err != nil {
		return "", fmt.Errorf("agenui workspace: encode messages: %w", err)
	}
	return string(raw), nil
}

// ValidateStructural enforces only protocol-independent document facts. Full
// protocol and Catalog validation are supplied by the caller at commit time.
func (d Document) ValidateStructural() error {
	if d.SchemaVersion != SchemaVersion || strings.TrimSpace(d.SurfaceID) == "" ||
		strings.TrimSpace(d.CatalogID) == "" || strings.TrimSpace(d.RootID) == "" {
		return errors.New("agenui workspace: schema_version, surface_id, catalog_id and root_id are required")
	}
	if len(d.Components) == 0 || d.DataModel == nil {
		return errors.New("agenui workspace: components and data_model are required")
	}
	if d.RootID != ProtocolRootID {
		return fmt.Errorf("agenui workspace: A2UI v0.9 root_id must be %q", ProtocolRootID)
	}
	ids := make(map[string]struct{}, len(d.Components))
	for index, component := range d.Components {
		id, _ := component["id"].(string)
		kind, _ := component["component"].(string)
		if id == "" || kind == "" {
			return fmt.Errorf("agenui workspace: component %d requires id and component", index)
		}
		if _, exists := ids[id]; exists {
			return fmt.Errorf("agenui workspace: duplicate component id %q", id)
		}
		ids[id] = struct{}{}
	}
	if _, exists := ids[d.RootID]; !exists {
		return fmt.Errorf("agenui workspace: root component %q is missing", d.RootID)
	}
	if err := validatePreviewExpressions(d.Components, d.DataModel); err != nil {
		return err
	}
	if err := validateSlotIDs("field", d.FieldSlots); err != nil {
		return err
	}
	if err := validateSlotIDs("action", d.ActionSlots); err != nil {
		return err
	}
	if err := validateSlotComponentReferences("field", d.FieldSlots, ids); err != nil {
		return err
	}
	if err := validateSlotComponentReferences("action", d.ActionSlots, ids); err != nil {
		return err
	}
	return validateFieldSlotRefs(d.FieldSlots)
}

// validatePreviewExpressions makes the first Design commit an honest preview:
// a formatString interpolation must resolve against the document's preview
// data. This is protocol structure, not a business-field heuristic; it works
// for every component and for both card and repeated-item paths.
func validatePreviewExpressions(components []map[string]any, data map[string]any) error {
	for _, component := range components {
		componentID, _ := component["id"].(string)
		if err := walkPreviewExpression(component, data, componentID); err != nil {
			return err
		}
	}
	return nil
}

func walkPreviewExpression(value any, data map[string]any, componentID string) error {
	switch typed := value.(type) {
	case map[string]any:
		if call, _ := typed["call"].(string); call == "formatString" {
			args, _ := typed["args"].(map[string]any)
			template, _ := firstString(args, "value", "template")
			for _, reference := range formatStringReferences(template) {
				if !previewPathExists(data, reference) {
					return fmt.Errorf("agenui workspace: component %q formatString references missing preview path %q", componentID, reference)
				}
			}
		}
		for _, child := range typed {
			if err := walkPreviewExpression(child, data, componentID); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := walkPreviewExpression(child, data, componentID); err != nil {
				return err
			}
		}
	}
	return nil
}

func firstString(values map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		if value, ok := values[key].(string); ok {
			return value, true
		}
	}
	return "", false
}

func formatStringReferences(template string) []string {
	matches := formatStringReferencePattern.FindAllStringSubmatch(template, -1)
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) == 2 && strings.TrimSpace(match[1]) != "" {
			result = append(result, strings.TrimSpace(match[1]))
		}
	}
	return result
}

var formatStringReferencePattern = regexp.MustCompile(`\$\{([^{}]+)\}`)

func previewPathExists(data map[string]any, rawPath string) bool {
	path := strings.TrimPrefix(strings.TrimSpace(rawPath), "/")
	if path == "" {
		return false
	}
	segments := strings.FieldsFunc(path, func(r rune) bool { return r == '.' || r == '/' })
	if len(segments) == 0 {
		return false
	}
	if previewPathExistsAt(data, segments) {
		return true
	}
	// A repeated component resolves bare names from its list-item scope. The
	// Workspace does not assign business scopes, so it only verifies that the
	// named path is represented by at least one preview object; renderer list
	// scoping remains the protocol's responsibility.
	return previewPathExistsNested(data, segments)
}

func previewPathExistsAt(value any, segments []string) bool {
	if len(segments) == 0 {
		return true
	}
	switch typed := value.(type) {
	case map[string]any:
		next, ok := typed[segments[0]]
		return ok && previewPathExistsAt(next, segments[1:])
	case []any:
		for _, item := range typed {
			if previewPathExistsAt(item, segments) {
				return true
			}
		}
	}
	return false
}

func previewPathExistsNested(value any, segments []string) bool {
	switch typed := value.(type) {
	case map[string]any:
		for _, child := range typed {
			if previewPathExistsAt(child, segments) || previewPathExistsNested(child, segments) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if previewPathExistsAt(child, segments) || previewPathExistsNested(child, segments) {
				return true
			}
		}
	}
	return false
}

// ValidateDraft checks the identity of an in-progress authoring document.
// Components and slots are intentionally allowed to be incomplete until
// commit so the model can build a large surface through bounded tool calls.
func (d Document) ValidateDraft() error {
	if d.SchemaVersion != SchemaVersion || strings.TrimSpace(d.SurfaceID) == "" ||
		strings.TrimSpace(d.CatalogID) == "" || strings.TrimSpace(d.RootID) == "" {
		return errors.New("agenui workspace: schema_version, surface_id, catalog_id and root_id are required")
	}
	if d.DataModel == nil {
		return errors.New("agenui workspace: data_model is required")
	}
	if d.RootID != ProtocolRootID {
		return fmt.Errorf("agenui workspace: A2UI v0.9 root_id must be %q", ProtocolRootID)
	}
	return nil
}

// EmptyDataPaths reports empty preview leaves without assigning business
// meaning to them. It is guidance for the Style model, not a publish gate.
func (d Document) EmptyDataPaths() []string {
	var result []string
	collectEmptyDataPaths(d.DataModel, "", &result)
	sort.Strings(result)
	return result
}

func collectEmptyDataPaths(value any, path string, output *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			collectEmptyDataPaths(typed[key], path+"/"+escapePointer(key), output)
		}
	case []any:
		if len(typed) == 0 {
			*output = append(*output, path)
			return
		}
		for index, child := range typed {
			collectEmptyDataPaths(child, fmt.Sprintf("%s/%d", path, index), output)
		}
	case string:
		if strings.TrimSpace(typed) == "" {
			*output = append(*output, path)
		}
	case nil:
		*output = append(*output, path)
	}
}

func escapePointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func validateSlotIDs(kind string, slots []map[string]any) error {
	seen := make(map[string]struct{}, len(slots))
	for index, slot := range slots {
		id := slotID(slot)
		if id == "" {
			return fmt.Errorf("agenui workspace: %s slot %d requires id", kind, index)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("agenui workspace: duplicate %s slot id %q", kind, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func validateSlotComponentReferences(kind string, slots []map[string]any, componentIDs map[string]struct{}) error {
	for index, slot := range slots {
		componentID, _ := slot["componentId"].(string)
		if strings.TrimSpace(componentID) == "" {
			return fmt.Errorf("agenui workspace: %s slot %d requires componentId", kind, index)
		}
		if _, exists := componentIDs[componentID]; !exists {
			return fmt.Errorf(
				"agenui workspace: %s slot %q references unknown component %q",
				kind, slotID(slot), componentID,
			)
		}
	}
	return nil
}

func validateFieldSlotRefs(slots []map[string]any) error {
	for _, slot := range slots {
		refKey := strings.TrimSpace(stringValue(slot["refKey"]))
		if refKey == "" || !strings.HasPrefix(refKey, "/") || strings.ContainsAny(refKey, "\r\n") {
			return fmt.Errorf(
				"agenui workspace: field slot %q requires an absolute data-model refKey",
				slotID(slot),
			)
		}
	}
	return nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func slotID(slot map[string]any) string {
	if id, _ := slot["slotId"].(string); id != "" {
		return id
	}
	id, _ := slot["id"].(string)
	return id
}

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	raw, _ := json.Marshal(input)
	var output map[string]any
	_ = json.Unmarshal(raw, &output)
	return output
}

func cloneMaps(input []map[string]any) []map[string]any {
	output := make([]map[string]any, 0, len(input))
	for _, item := range input {
		output = append(output, cloneMap(item))
	}
	return output
}

func sortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
