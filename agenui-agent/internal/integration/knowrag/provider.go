package knowrag

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/search"
)

// The static MCP mirror in tools.yaml lets the SDK apply a larger result
// policy before ToolCallInterceptor runs. The host-only field projection keeps
// Binder/Template schema guidance bounded, while the model-visible selection
// receipt contains metadata and provenance only.
const (
	maxBindingProjectionBytes = 3 << 10
	maxSelectionReceiptBytes  = 3 << 10
	// syntheticPrimaryAPIVersion and syntheticPrimaryRevisionPrefix are the
	// deterministic identities governSearchResult synthesizes when the
	// knowledge source omits a version or revision.
	syntheticPrimaryAPIVersion     = "unversioned"
	syntheticPrimaryRevisionPrefix = "snapshot-"
	// binderProjectionFieldLimit and binderProjectionBytes size the
	// Binder-facing field projection. The Binder needs the same evidence
	// breadth needed for realistic binding decisions (180 fields), not the 3KiB
	// Search-model projection that exists to keep the Search prompt small.
	binderProjectionFieldLimit = 180
	binderProjectionBytes      = 48 << 10
	maxProjectedEnumValues     = 256
	maxProjectedEnumRunes      = 512
	maxMalformedSchemaFields   = 2048
	// searchProjectionFieldLimit keeps the frozen Search-model projection
	// behavior: at most 24 ranked fields inside 3KiB.
	searchProjectionFieldLimit = 24
)

// ProcessToolResult validates and enriches the full result returned by the
// governed MCP tool, then splits it into the full host-side payload and the
// compact result returned to the model.
func ProcessToolResult(
	value string,
	query string,
	tenantID string,
	userID string,
	topK int,
) (full string, compact string, err error) {
	return processToolResult(value, query, tenantID, userID, topK, false)
}

// ProcessToolResultWithSelectionReceipt exposes a compact provenance receipt
// while retaining the same governed Host payload.
func ProcessToolResultWithSelectionReceipt(
	value string,
	query string,
	tenantID string,
	userID string,
	topK int,
) (full string, receipt string, err error) {
	return processToolResult(value, query, tenantID, userID, topK, true)
}

// ProcessToolResultForBinder governs the Binder primary recall result and
// returns a model-visible projection sized for binding decisions: up to 180
// ranked typed fields inside a 48KiB budget. The same projection is frozen in the host-side
// Search artifact: Materializer admission must see exactly the evidence the Binder
// used, including Host-completed fields from partially declared schemas.
func ProcessToolResultForBinder(
	value string,
	query string,
	tenantID string,
	userID string,
	topK int,
) (full string, compact string, err error) {
	raw := json.RawMessage(normalizedJSONText(value))
	if !json.Valid(raw) {
		return "", "", fmt.Errorf("knowrag: tool result is not valid JSON")
	}
	raw, err = canonicalizeEmbeddedResponseModels(raw)
	if err != nil {
		return "", "", err
	}
	governed, err := governSearchResult(raw, query, tenantID, userID, topK)
	if err != nil {
		return "", "", err
	}
	fullOutput, err := buildToolOutputBudget(
		governed, query, binderProjectionFieldLimit, binderProjectionBytes,
	)
	if err != nil {
		return "", "", err
	}
	compactOutput, err := bindingProjectionBudget(
		governed, query, binderProjectionFieldLimit, binderProjectionBytes,
	)
	if err != nil {
		return "", "", err
	}
	return string(fullOutput), string(compactOutput), nil
}

func processToolResult(
	value string,
	query string,
	tenantID string,
	userID string,
	topK int,
	selectionReceiptEnabled bool,
) (full string, compact string, err error) {
	raw := json.RawMessage(normalizedJSONText(value))
	if !json.Valid(raw) {
		return "", "", fmt.Errorf("knowrag: tool result is not valid JSON")
	}
	raw, err = canonicalizeEmbeddedResponseModels(raw)
	if err != nil {
		return "", "", err
	}
	governed, err := governSearchResult(raw, query, tenantID, userID, topK)
	if err != nil {
		return "", "", err
	}
	fullOutput, err := buildToolOutput(governed, query)
	if err != nil {
		return "", "", err
	}
	compactOutput, err := bindingProjection(governed, query)
	if selectionReceiptEnabled {
		compactOutput, err = selectionReceipt(governed)
	}
	if err != nil {
		return "", "", err
	}
	return string(fullOutput), string(compactOutput), nil
}

// canonicalizeEmbeddedResponseModels deliberately preserves the source schema.
// Studio does not repair or reinterpret schemas for one upstream API: providers
// must return valid, portable JSON Schema before it enters the shared pipeline.
func canonicalizeEmbeddedResponseModels(
	raw json.RawMessage,
) (json.RawMessage, error) {
	return raw, nil
}

func normalizedJSONText(value string) string {
	normalized := strings.TrimSpace(value)
	for range 8 {
		if len(normalized) == 0 {
			break
		}
		if normalized[0] == '"' {
			var unwrapped string
			if json.Unmarshal([]byte(normalized), &unwrapped) != nil {
				break
			}
			normalized = strings.TrimSpace(unwrapped)
			continue
		}
		if text, ok := mcpTextContent(normalized); ok {
			normalized = strings.TrimSpace(text)
			continue
		}
		break
	}
	return normalized
}

// mcpTextContent unwraps the MCP tool-result content envelope forms
// {"type":"text","text":"..."} and [{"type":"text","text":"..."}] that some
// SDK paths hand to interceptors instead of the extracted text payload. A
// knowledge-base search envelope (query/results/total) never carries
// type=text, so business payloads are not affected.
func mcpTextContent(value string) (string, bool) {
	type textContent struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	switch value[0] {
	case '{':
		var single textContent
		if json.Unmarshal([]byte(value), &single) == nil &&
			single.Type == "text" && single.Text != "" {
			return single.Text, true
		}
	case '[':
		var many []textContent
		if json.Unmarshal([]byte(value), &many) == nil && len(many) == 1 &&
			many[0].Type == "text" && many[0].Text != "" {
			return many[0].Text, true
		}
	}
	return "", false
}

type apiSearchResponse struct {
	Query   string           `json:"query"`
	Results []map[string]any `json:"results"`
	Total   int              `json:"total"`
}

type field struct {
	Path        string   `json:"path"`
	Type        string   `json:"type,omitempty"`
	Description string   `json:"description,omitempty"`
	Example     any      `json:"example,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Trust       string   `json:"source_trust,omitempty"`
	priority    int
}

// bindingProjection is a host-only field projection. It is embedded in the
// full Search handoff consumed by Template/Binder and must never be returned
// to the Search model.
func bindingProjection(raw json.RawMessage, query string) (json.RawMessage, error) {
	return bindingProjectionBudget(
		raw, query, searchProjectionFieldLimit, maxBindingProjectionBytes,
	)
}

// bindingProjectionBudget projects ranked typed fields for one consumer
// budget. The Search model keeps the frozen 24-field/3KiB projection; the
// Binder consumes a 180-field/48KiB projection through
// ProcessToolResultForBinder.
func bindingProjectionBudget(
	raw json.RawMessage,
	query string,
	fieldLimit int,
	byteBudget int,
) (json.RawMessage, error) {
	var source apiSearchResponse
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, fmt.Errorf("knowrag: decode search result: %w", err)
	}
	apis := make([]map[string]any, 0, len(source.Results))
	for _, result := range source.Results {
		fields := responseFieldsLimit(result["response_model"], query, fieldLimit)
		api := map[string]any{
			"path":               result["path"],
			"method":             result["method"],
			"description":        result["description"],
			"project":            result["project_name"],
			"score":              result["score"],
			"result_id":          result["result_id"],
			"data_source_id":     result["data_source_id"],
			"api_version":        result["api_version"],
			"knowledge_revision": result["knowledge_revision"],
			"content_hash":       result["content_hash"],
			"response_fields":    fields,
		}
		apis = append(apis, api)
	}
	compacted := map[string]any{
		"query": source.Query,
		"total": source.Total,
		"apis":  apis,
	}
	encoded, err := json.Marshal(compacted)
	if err != nil {
		return nil, fmt.Errorf("knowrag: encode compact result: %w", err)
	}
	for len(encoded) > byteBudget {
		longest := -1
		longestLength := 0
		for index := range apis {
			fields, _ := apis[index]["response_fields"].([]field)
			if len(fields) > longestLength {
				longest = index
				longestLength = len(fields)
			}
		}
		if longest < 0 || longestLength == 0 {
			break
		}
		fields := apis[longest]["response_fields"].([]field)
		apis[longest]["response_fields"] = fields[:len(fields)-1]
		encoded, err = json.Marshal(compacted)
		if err != nil {
			return nil, fmt.Errorf("knowrag: encode compact result: %w", err)
		}
	}
	if len(encoded) > byteBudget {
		return nil, fmt.Errorf(
			"knowrag: binding projection exceeds %d bytes",
			byteBudget,
		)
	}
	return encoded, nil
}

// selectionReceipt is the deterministic model-visible result for the selected
// API. Full response schemas/examples remain in the host-side Search handoff;
// the Search model only needs enough metadata and immutable provenance to echo
// the already-governed selection without copying binding fields.
func selectionReceipt(raw json.RawMessage) (json.RawMessage, error) {
	var source apiSearchResponse
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, fmt.Errorf("knowrag: decode selection receipt: %w", err)
	}
	apis := make([]map[string]any, 0, len(source.Results))
	for _, result := range source.Results {
		apis = append(apis, map[string]any{
			"path":               result["path"],
			"method":             result["method"],
			"description":        result["description"],
			"project":            result["project_name"],
			"score":              clampUnitScore(scoreValue(result["score"])),
			"result_id":          result["result_id"],
			"data_source_id":     result["data_source_id"],
			"api_version":        result["api_version"],
			"knowledge_revision": result["knowledge_revision"],
			"content_hash":       result["content_hash"],
		})
	}
	receipt := map[string]any{
		"query": source.Query,
		"total": source.Total,
		"apis":  apis,
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return nil, fmt.Errorf("knowrag: encode selection receipt: %w", err)
	}
	if len(encoded) > maxSelectionReceiptBytes {
		return nil, fmt.Errorf(
			"knowrag: selection receipt exceeds %d bytes",
			maxSelectionReceiptBytes,
		)
	}
	return encoded, nil
}

func governSearchResult(
	raw json.RawMessage,
	query string,
	tenantID string,
	userID string,
	topK int,
) (json.RawMessage, error) {
	if topK < 1 || topK > search.MaxResultsPerSearch {
		return nil, fmt.Errorf(
			"knowrag: requested top_k %d is outside [1,%d]",
			topK,
			search.MaxResultsPerSearch,
		)
	}
	var source apiSearchResponse
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, fmt.Errorf("knowrag: decode governed result: %w", err)
	}
	if len(source.Results) > topK {
		return nil, fmt.Errorf(
			"knowrag: result count %d exceeds requested top_k %d",
			len(source.Results),
			topK,
		)
	}
	response := search.Response{
		Results:           make([]search.Result, 0, len(source.Results)),
		SelectedResultIDs: make([]string, 0, len(source.Results)),
	}
	for index, result := range source.Results {
		canonical := cloneResultWithoutReceipt(result)
		encoded, err := json.Marshal(canonical)
		if err != nil {
			return nil, fmt.Errorf("knowrag: hash result %d: %w", index, err)
		}
		digest := sha256.Sum256(encoded)
		contentHash := fmt.Sprintf("sha256:%x", digest[:])
		dataSourceID := firstString(
			result["data_source_id"],
			result["id"],
			result["path"],
		)
		if dataSourceID == "" {
			return nil, fmt.Errorf("knowrag: result %d lacks stable identity", index)
		}
		apiVersion := firstString(result["api_version"], result["version"])
		if apiVersion == "" {
			apiVersion = syntheticPrimaryAPIVersion
		}
		knowledgeRevision := firstString(
			result["knowledge_revision"],
			result["revision"],
		)
		if knowledgeRevision == "" {
			knowledgeRevision = syntheticPrimaryRevisionPrefix + fmt.Sprintf("%x", digest[:8])
		}
		resultID := firstString(result["result_id"])
		if resultID == "" {
			resultID = "api-" + fmt.Sprintf("%x", digest[:8])
		}
		score := clampUnitScore(scoreValue(result["score"]))
		receipt := search.Result{
			ResultID:          resultID,
			DataSourceID:      dataSourceID,
			APIVersion:        apiVersion,
			KnowledgeRevision: knowledgeRevision,
			ContentHash:       contentHash,
			Score:             0.45 * score,
			ScoreComponents: search.ScoreComponents{
				Lexical: score,
			},
		}
		response.Results = append(response.Results, receipt)
		response.SelectedResultIDs = append(response.SelectedResultIDs, resultID)
	}
	validatedResults := make([]search.Result, 0, len(response.Results))
	seenResultIDs := make(map[string]struct{}, len(response.Results))
	seenRevisions := make(map[string]struct{}, len(response.Results))
	for index, receipt := range response.Results {
		validated, err := search.NormalizeAndValidateResponse(search.Request{
			TenantID: tenantID,
			UserID:   userID,
			Query:    query,
			Limit:    1,
		}, search.Response{
			Results:           []search.Result{receipt},
			SelectedResultIDs: []string{receipt.ResultID},
		})
		if err != nil {
			return nil, fmt.Errorf(
				"knowrag: governed result %d rejected: %w", index, err,
			)
		}
		receipt = validated.Results[0]
		if _, duplicate := seenResultIDs[receipt.ResultID]; duplicate {
			return nil, fmt.Errorf(
				"knowrag: governed result %d duplicates result_id %q",
				index,
				receipt.ResultID,
			)
		}
		seenResultIDs[receipt.ResultID] = struct{}{}
		revisionID := strings.Join([]string{
			receipt.DataSourceID,
			receipt.APIVersion,
			receipt.KnowledgeRevision,
		}, "\x00")
		if _, duplicate := seenRevisions[revisionID]; duplicate {
			return nil, fmt.Errorf(
				"knowrag: governed result %d duplicates knowledge revision",
				index,
			)
		}
		seenRevisions[revisionID] = struct{}{}
		validatedResults = append(validatedResults, receipt)
	}
	for index, receipt := range validatedResults {
		result := source.Results[index]
		result["result_id"] = receipt.ResultID
		result["data_source_id"] = receipt.DataSourceID
		result["api_version"] = receipt.APIVersion
		result["knowledge_revision"] = receipt.KnowledgeRevision
		result["content_hash"] = receipt.ContentHash
	}
	return json.Marshal(source)
}

func cloneResultWithoutReceipt(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		switch key {
		case "result_id", "data_source_id", "api_version",
			"knowledge_revision", "content_hash":
			continue
		default:
			result[key] = value
		}
	}
	return result
}

func firstString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func scoreValue(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case json.Number:
		result, _ := typed.Float64()
		return result
	default:
		return 0
	}
}

// clampUnitScore brings knowrag's raw relevance score into the [0,1] contract
// the search-governance validator enforces. knowrag returns un-normalized
// scores that can fall slightly outside the unit interval for strong matches
// (observed 1.108 for a lexical hit), which would otherwise be fail-closed as
// "invalid score" and sink the whole run. NaN/Inf collapse to 0; ordering
// inside [0,1] is preserved, only out-of-range extremes are clamped. The
// receipt keeps its aggregate and components deterministic.
func clampUnitScore(score float64) float64 {
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0
	}
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}

func buildToolOutput(raw json.RawMessage, query string) (json.RawMessage, error) {
	return buildToolOutputBudget(
		raw, query, searchProjectionFieldLimit, maxBindingProjectionBytes,
	)
}

func buildToolOutputBudget(
	raw json.RawMessage,
	query string,
	fieldLimit int,
	byteBudget int,
) (json.RawMessage, error) {
	compacted, err := bindingProjectionBudget(raw, query, fieldLimit, byteBudget)
	if err != nil {
		return nil, err
	}
	var compactedObject map[string]any
	if err := json.Unmarshal(compacted, &compactedObject); err != nil {
		return nil, fmt.Errorf("knowrag: decode compact result: %w", err)
	}
	var source apiSearchResponse
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, fmt.Errorf("knowrag: decode full search result: %w", err)
	}
	compactedObject["results"] = source.Results
	encoded, err := json.Marshal(compactedObject)
	if err != nil {
		return nil, fmt.Errorf("knowrag: encode full search output: %w", err)
	}
	return encoded, nil
}

func responseFields(value any, query string) []field {
	return responseFieldsLimit(value, query, searchProjectionFieldLimit)
}

func responseFieldsLimit(value any, query string, limit int) []field {
	text, ok := value.(string)
	if !ok || !json.Valid([]byte(text)) {
		return nil
	}
	var schema map[string]any
	if json.Unmarshal([]byte(text), &schema) != nil {
		return nil
	}
	var fields []field
	flattenSchema("$", schema, &fields)
	for index := range fields {
		fields[index].priority = fieldPriority(
			fields[index].Path, fields[index].Description, query,
		)
		fields[index].Trust = "schema_defined"
	}
	return prioritizedFieldsLimit(fields, limit)
}

func projectedStringEnum(node map[string]any) []string {
	if nodeType, _ := node["type"].(string); nodeType != "string" {
		return nil
	}
	raw, ok := node["enum"].([]any)
	if !ok || len(raw) == 0 || len(raw) > maxProjectedEnumValues {
		return nil
	}
	seen := make(map[string]struct{}, len(raw))
	result := make([]string, 0, len(raw))
	for _, value := range raw {
		text, ok := value.(string)
		if !ok || text == "" || strings.TrimSpace(text) != text ||
			len([]rune(text)) > maxProjectedEnumRunes {
			return nil
		}
		if _, duplicate := seen[text]; duplicate {
			return nil
		}
		seen[text] = struct{}{}
		result = append(result, text)
	}
	sort.Strings(result)
	return result
}

func prioritizedFields(fields []field) []field {
	return prioritizedFieldsLimit(fields, searchProjectionFieldLimit)
}

func prioritizedFieldsLimit(fields []field, limit int) []field {
	sort.Slice(fields, func(i, j int) bool {
		if fields[i].priority != fields[j].priority {
			return fields[i].priority > fields[j].priority
		}
		if fields[i].Trust != fields[j].Trust {
			return fields[i].Trust == "schema_defined"
		}
		return fields[i].Path < fields[j].Path
	})
	if limit > 0 && len(fields) > limit {
		fields = fields[:limit]
	}
	return fields
}

func flattenSchema(path string, node map[string]any, fields *[]field) {
	properties, _ := node["properties"].(map[string]any)
	if len(properties) > 0 {
		names := make([]string, 0, len(properties))
		for name := range properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			child, _ := properties[name].(map[string]any)
			if child != nil {
				flattenSchema(path+"."+name, child, fields)
			}
		}
		return
	}
	nodeType, _ := node["type"].(string)
	if nodeType == "array" {
		if items, ok := node["items"].(map[string]any); ok {
			flattenSchema(path+"[*]", items, fields)
			return
		}
	}
	description, _ := node["description"].(string)
	if len(description) > 160 {
		description = description[:160]
	}
	*fields = append(*fields, field{
		Path:        path,
		Type:        nodeType,
		Description: description,
		Example:     node["example"],
		Enum:        projectedStringEnum(node),
	})
}

// fieldPriority is intentionally neutral. Source fields are ordered by their
// schema traversal order and exposed with their real paths and descriptions;
// semantic selection is performed by the model, not a built-in business lexicon.
func fieldPriority(path, description, query string) int {
	return 0
}
