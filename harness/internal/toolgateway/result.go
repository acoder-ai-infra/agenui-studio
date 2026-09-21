package toolgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type normalizedArtifact struct {
	ref        string
	typeName   artifact.ArtifactType
	mimeType   string
	sizeBytes  int64
	hash       string
	visibility observability.EventVisibility
}

type normalizedResult struct {
	preview            json.RawMessage
	modelContextResult json.RawMessage
	presentation       *ResultPresentation
	resultRef          string
	resultArtifact     *normalizedArtifact
	debugRef           string
	outputBytes        int64
	truncated          bool
	truncateReason     string
	originalSize       int64
	artifacts          []normalizedArtifact
}

func normalizeResult(ctx context.Context, artifactStore artifact.ArtifactStore, req ToolCallRequest, raw *ToolRawResult, policy ToolOutputPolicy) (normalizedResult, error) {
	if raw == nil {
		raw = &ToolRawResult{}
	}
	binaryResult := len(raw.Data) == 0 && raw.Text == "" && len(raw.Bytes) > 0
	artifactContent := append([]byte(nil), raw.Bytes...)
	data := append(json.RawMessage(nil), raw.Data...)
	if len(data) == 0 && raw.Text != "" {
		data, _ = json.Marshal(raw.Text)
	}
	if binaryResult {
		data, _ = json.Marshal(map[string]any{
			"binary":     true,
			"mime_type":  raw.MimeType,
			"size_bytes": len(raw.Bytes),
		})
	}
	if len(data) == 0 {
		data = json.RawMessage(`null`)
	}
	redactedData, err := redactSensitiveJSON(data)
	if err != nil {
		return normalizedResult{}, NewToolError(ErrorTypeNormalizationFailed, "tool result cannot form safe layers", false, err)
	}
	data = redactedData
	presentation := normalizeResultPresentation(raw.Presentation)
	if !binaryResult {
		artifactContent = append([]byte(nil), data...)
	}
	mimeType := raw.MimeType
	if mimeType == "" {
		mimeType = "application/json"
	}
	threshold := policy.ArtifactThresholdBytes
	if threshold <= 0 {
		threshold = 4096
	}
	maxPreviewBytes := policy.MaxSSEPreviewBytes
	if maxPreviewBytes <= 0 {
		maxPreviewBytes = 1024
	}
	preview, previewTruncated := truncateSafeResult(data, maxPreviewBytes)
	maxModelBytes := policy.MaxModelContextBytes
	if maxModelBytes <= 0 {
		maxModelBytes = 12000
	}
	modelContext, modelTruncated := truncateSafeResult(data, maxModelBytes)
	truncated := previewTruncated || modelTruncated
	if policy.SummarizeWhenTruncated && modelTruncated {
		modelContext = append(json.RawMessage(nil), preview...)
		if len(modelContext) > maxModelBytes {
			modelContext, _ = truncateSafeResult(modelContext, maxModelBytes)
		}
	}
	truncateReason := ""
	if truncated {
		truncateReason = "size_limit"
	}
	artifacts := make([]normalizedArtifact, 0, len(raw.ArtifactRefs)+2)
	seenRefs := make(map[string]struct{}, len(raw.ArtifactRefs)+2)
	if req.ArgumentsRef != "" {
		seenRefs[req.ArgumentsRef] = struct{}{}
	}
	appendArtifact := func(item normalizedArtifact) {
		if _, exists := seenRefs[item.ref]; exists {
			return
		}
		seenRefs[item.ref] = struct{}{}
		artifacts = append(artifacts, item)
	}
	verifyArtifactRefs := func() error {
		if len(raw.ArtifactRefs) > 0 && artifactStore == nil {
			return NewToolError(ErrorTypeArtifactError, "artifact store is required to verify tool artifact references", false, nil)
		}
		for _, ref := range raw.ArtifactRefs {
			if _, exists := seenRefs[ref]; exists {
				continue
			}
			meta, err := artifactStore.Head(ctx, ref)
			if err != nil {
				return NewToolError(ErrorTypeArtifactError, "verify tool artifact reference", false, err)
			}
			item, err := normalizeArtifactMeta(req, ref, meta)
			if err != nil {
				return err
			}
			appendArtifact(item)
		}
		return nil
	}
	if !raw.Partial {
		if err := verifyArtifactRefs(); err != nil {
			return normalizedResult{}, err
		}
	}
	resultRef := ""
	var resultArtifact *normalizedArtifact
	if len(artifactContent) > threshold || truncated || raw.Partial || binaryResult {
		if artifactStore == nil {
			return normalizedResult{}, NewToolError(ErrorTypeArtifactError, "artifact store is required for protected tool result", false, nil)
		}
		metadata := map[string]string{
			"tool_name": req.ToolName,
		}
		if raw.Partial {
			metadata["partial"] = "true"
		}
		artifactName := req.ToolName + "-result.json"
		if binaryResult {
			artifactName = req.ToolName + "-result.bin"
		}
		meta, err := artifactStore.Put(ctx, artifact.PutArtifactRequest{
			TenantID:        req.TenantID,
			UserID:          req.UserID,
			SessionID:       req.SessionID,
			RunID:           req.RunID,
			StepID:          req.StepID,
			OwnerModule:     artifact.OwnerModuleToolGateway,
			OwnerID:         req.ToolCallID,
			ArtifactType:    artifact.ArtifactTypeToolResult,
			MimeType:        mimeType,
			Name:            artifactName,
			Visibility:      artifact.VisibilityInternal,
			Content:         bytes.NewReader(artifactContent),
			RetentionPolicy: artifact.RetentionRunTTL,
			CreatedBy:       "tool:" + req.ToolName,
			IdempotencyKey:  req.RunID + ":" + req.ToolCallID + ":tool-result",
			Metadata:        metadata,
		})
		if err != nil {
			return normalizedResult{}, NewToolError(ErrorTypeArtifactError, "write tool result artifact", false, err)
		}
		item, err := normalizeArtifactMeta(req, artifactRef(meta), meta)
		if err != nil {
			return normalizedResult{}, err
		}
		if meta.ArtifactType != artifact.ArtifactTypeToolResult || meta.Visibility != artifact.VisibilityInternal || meta.RetentionPolicy != artifact.RetentionRunTTL {
			return normalizedResult{}, NewToolError(ErrorTypeArtifactError, "tool result artifact metadata does not match result policy", false, nil)
		}
		if raw.Partial && meta.Metadata["partial"] != "true" {
			return normalizedResult{}, NewToolError(ErrorTypeArtifactError, "partial tool result artifact metadata is missing", false, nil)
		}
		resultRef = item.ref
		itemCopy := item
		resultArtifact = &itemCopy
		appendArtifact(item)
	}

	debugRef := ""
	currentResult := func() normalizedResult {
		return normalizedResult{
			preview:            preview,
			modelContextResult: modelContext,
			presentation:       presentation,
			resultRef:          resultRef,
			resultArtifact:     resultArtifact,
			debugRef:           debugRef,
			outputBytes:        int64(len(artifactContent)),
			truncated:          truncated,
			truncateReason:     truncateReason,
			originalSize:       int64(len(artifactContent)),
			artifacts:          artifacts,
		}
	}
	returnNormalizationError := func(err error) (normalizedResult, error) {
		if raw.Partial && resultRef != "" {
			return currentResult(), err
		}
		return normalizedResult{}, err
	}
	if raw.Partial {
		if err := verifyArtifactRefs(); err != nil {
			return returnNormalizationError(err)
		}
	}

	if len(raw.Debug) > 0 {
		if artifactStore == nil {
			return returnNormalizationError(NewToolError(ErrorTypeArtifactError, "artifact store is required for raw tool debug", false, nil))
		}
		meta, err := artifactStore.Put(ctx, artifact.PutArtifactRequest{
			TenantID:        req.TenantID,
			UserID:          req.UserID,
			SessionID:       req.SessionID,
			RunID:           req.RunID,
			StepID:          req.StepID,
			OwnerModule:     artifact.OwnerModuleToolGateway,
			OwnerID:         req.ToolCallID,
			ArtifactType:    artifact.ArtifactTypeDebugPayload,
			MimeType:        "application/json",
			Name:            req.ToolName + "-debug.json",
			Visibility:      artifact.VisibilityDebug,
			Content:         bytes.NewReader(raw.Debug),
			RetentionPolicy: artifact.RetentionDebugShortTTL,
			CreatedBy:       "tool:" + req.ToolName,
			IdempotencyKey:  req.RunID + ":" + req.ToolCallID + ":tool-debug",
			Metadata: map[string]string{
				"tool_name":    req.ToolName,
				"payload_kind": "debug",
			},
		})
		if err != nil {
			return returnNormalizationError(NewToolError(ErrorTypeArtifactError, "write raw tool debug artifact", false, err))
		}
		item, err := normalizeArtifactMeta(req, artifactRef(meta), meta)
		if err != nil {
			return returnNormalizationError(err)
		}
		if meta.ArtifactType != artifact.ArtifactTypeDebugPayload || meta.Visibility != artifact.VisibilityDebug || meta.RetentionPolicy != artifact.RetentionDebugShortTTL {
			return returnNormalizationError(NewToolError(ErrorTypeArtifactError, "tool debug artifact metadata does not match debug policy", false, nil))
		}
		debugRef = item.ref
		appendArtifact(item)
	}
	return currentResult(), nil
}

const (
	maxResultPresentationTitleRunes   = 160
	maxResultPresentationSummaryRunes = 1000
	maxResultPresentationDetails      = 12
	maxResultPresentationLabelRunes   = 80
	maxResultPresentationValueRunes   = 1000
)

func normalizeResultPresentation(source *ResultPresentation) *ResultPresentation {
	if source == nil {
		return nil
	}
	title := boundedResultPresentationText(source.Title, maxResultPresentationTitleRunes)
	summary := boundedResultPresentationText(source.Summary, maxResultPresentationSummaryRunes)
	details := make([]ResultPresentationDetail, 0, min(len(source.Details), maxResultPresentationDetails))
	for _, detail := range source.Details {
		if len(details) >= maxResultPresentationDetails {
			break
		}
		label := boundedResultPresentationText(detail.Label, maxResultPresentationLabelRunes)
		value := boundedResultPresentationText(detail.Value, maxResultPresentationValueRunes)
		if label == "" || value == "" {
			continue
		}
		if isSensitiveField(label) {
			value = "[REDACTED]"
		}
		details = append(details, ResultPresentationDetail{Label: label, Value: value})
	}
	if title == "" && summary == "" && len(details) == 0 {
		return nil
	}
	return &ResultPresentation{Title: title, Summary: summary, Details: details}
}

func boundedResultPresentationText(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if limit > 0 && len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}

func cloneResultPresentation(source *ResultPresentation) *ResultPresentation {
	if source == nil {
		return nil
	}
	copy := *source
	copy.Details = append([]ResultPresentationDetail(nil), source.Details...)
	return &copy
}

func truncateSafeResult(data json.RawMessage, maxBytes int) (json.RawMessage, bool) {
	if maxBytes <= 0 || len(data) <= maxBytes {
		return append(json.RawMessage(nil), data...), false
	}
	end := maxBytes
	for end > 0 {
		if utf8.Valid(data[:end]) {
			truncated, _ := json.Marshal(string(data[:end]) + "...")
			if len(truncated) <= maxBytes {
				return truncated, true
			}
		}
		end--
	}
	if maxBytes >= len(`"..."`) {
		return json.RawMessage(`"..."`), true
	}
	if maxBytes >= len(`""`) {
		return json.RawMessage(`""`), true
	}
	return json.RawMessage(`0`), true
}

func normalizeArtifactMeta(req ToolCallRequest, requestedRef string, meta *artifact.ArtifactMeta) (normalizedArtifact, error) {
	invalid := func(reason string) (normalizedArtifact, error) {
		return normalizedArtifact{}, NewToolError(ErrorTypeArtifactError, "invalid tool artifact metadata: "+reason, false, nil)
	}
	if meta == nil {
		return invalid("missing metadata")
	}
	if requestedRef == "" || meta.ArtifactRef == "" || meta.ArtifactRef != requestedRef {
		return invalid("reference mismatch")
	}
	parts, err := artifact.ParseRef(meta.ArtifactRef)
	if err != nil {
		return normalizedArtifact{}, NewToolError(ErrorTypeArtifactError, "invalid tool artifact reference", false, err)
	}
	if meta.TenantID != req.TenantID || meta.SessionID != req.SessionID || meta.RunID != req.RunID ||
		parts.TenantID != req.TenantID || parts.SessionID != req.SessionID || parts.RunID != req.RunID {
		return invalid("cross-scope reference")
	}
	if !isCanonicalArtifactType(meta.ArtifactType) {
		return invalid("unknown artifact type")
	}
	visibility, ok := artifactEventVisibility(meta.Visibility)
	if !ok {
		return invalid("unknown visibility")
	}
	if meta.MimeType == "" || meta.Hash == "" || meta.SizeBytes < 0 {
		return invalid("incomplete content metadata")
	}
	return normalizedArtifact{
		ref:        meta.ArtifactRef,
		typeName:   meta.ArtifactType,
		mimeType:   meta.MimeType,
		sizeBytes:  meta.SizeBytes,
		hash:       meta.Hash,
		visibility: visibility,
	}, nil
}

func artifactRef(meta *artifact.ArtifactMeta) string {
	if meta == nil {
		return ""
	}
	return meta.ArtifactRef
}

func isCanonicalArtifactType(value artifact.ArtifactType) bool {
	switch value {
	case artifact.ArtifactTypeToolResult,
		artifact.ArtifactTypeContextSnapshot,
		artifact.ArtifactTypeCheckpointState,
		artifact.ArtifactTypeDebugPayload,
		artifact.ArtifactTypeFinalResult,
		artifact.ArtifactTypeFile,
		artifact.ArtifactTypeImage,
		artifact.ArtifactTypeSchema,
		artifact.ArtifactTypePrompt:
		return true
	default:
		return false
	}
}

func artifactEventVisibility(value artifact.Visibility) (observability.EventVisibility, bool) {
	switch value {
	case artifact.VisibilityUserVisible:
		return observability.VisibilityUserVisible, true
	case artifact.VisibilityInternal:
		return observability.VisibilityInternal, true
	case artifact.VisibilityDebug:
		return observability.VisibilityDebug, true
	case artifact.VisibilityRestricted:
		return observability.VisibilityRestricted, true
	default:
		return "", false
	}
}

func rawResultData(raw *ToolRawResult) json.RawMessage {
	if raw == nil {
		return json.RawMessage(`null`)
	}
	data := append(json.RawMessage(nil), raw.Data...)
	if len(data) == 0 && raw.Text != "" {
		data, _ = json.Marshal(raw.Text)
	}
	if len(data) == 0 && len(raw.Bytes) > 0 {
		data, _ = json.Marshal(string(raw.Bytes))
	}
	if len(data) == 0 {
		data = json.RawMessage(`null`)
	}
	return data
}

func redactSensitiveJSON(data json.RawMessage) (json.RawMessage, error) {
	if !json.Valid(data) {
		return nil, errors.New("tool result is not valid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if !containsSensitiveField(value) {
		return append(json.RawMessage(nil), data...), nil
	}
	redacted := redactSensitiveValue(value)
	out, err := json.Marshal(redacted)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func containsSensitiveField(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, fieldValue := range typed {
			if isSensitiveField(key) || containsSensitiveField(fieldValue) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if containsSensitiveField(item) {
				return true
			}
		}
	}
	return false
}

func redactSensitiveValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, fieldValue := range typed {
			if isSensitiveField(key) {
				out[key] = "[REDACTED]"
				continue
			}
			out[key] = redactSensitiveValue(fieldValue)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = redactSensitiveValue(item)
		}
		return out
	default:
		return value
	}
}

func isSensitiveField(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("-", "_", " ", "_", ".", "_").Replace(key))
	for _, marker := range []string{"password", "passwd", "secret", "token", "api_key", "apikey", "authorization", "credential"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}
