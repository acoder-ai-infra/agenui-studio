package toolgateway

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/processpresentation"
)

func buildToolStartedEvent(ids observability.IDGenerator, now time.Time, tc observability.TraceContext, req ToolCallRequest, def *ToolDefinition) (observability.AgentEvent, error) {
	return NormalizeToolEvent(ids, buildToolStartedToolEvent(ids, now, tc, req, def))
}

func buildToolStartedToolEvent(ids observability.IDGenerator, now time.Time, tc observability.TraceContext, req ToolCallRequest, def *ToolDefinition) ToolEvent {
	payload := map[string]any{
		"tool_call_id":         req.ToolCallID,
		"tool_name":            req.ToolName,
		"tool_version":         req.ToolVersion,
		"tool_type":            string(def.Type),
		"display_name":         def.DisplayName,
		"arguments_preview":    buildArgumentsPreview(req.ArgumentsPreview, def.ResultPolicy.MaxSSEPreviewBytes),
		"risk_level":           string(def.RiskLevel),
		"process_presentation": toolProcessPresentation(req, def),
	}
	preview := mustMarshalJSON(payload)
	event := baseToolEvent(ids, now, tc, req, def, toolEventStarted, preview, nil)
	event.DebugRef = req.ArgumentsRef
	return event
}

func buildToolCompletedEvent(ids observability.IDGenerator, now time.Time, tc observability.TraceContext, req ToolCallRequest, def *ToolDefinition, normalized normalizedResult, duration time.Duration, retryCount int) (observability.AgentEvent, error) {
	return NormalizeToolEvent(ids, buildToolCompletedToolEvent(ids, now, tc, req, def, normalized, duration, retryCount))
}

func buildToolCompletedToolEvent(ids observability.IDGenerator, now time.Time, tc observability.TraceContext, req ToolCallRequest, def *ToolDefinition, normalized normalizedResult, duration time.Duration, retryCount int) ToolEvent {
	payload := map[string]any{
		"tool_call_id":         req.ToolCallID,
		"tool_name":            req.ToolName,
		"tool_version":         req.ToolVersion,
		"tool_type":            string(def.Type),
		"display_name":         def.DisplayName,
		"success":              true,
		"duration_ms":          duration.Milliseconds(),
		"retry_count":          retryCount,
		"result_preview":       jsonRawToAny(normalized.preview),
		"result_ref":           normalized.resultRef,
		"truncated":            normalized.truncated,
		"output_size_bytes":    normalized.outputBytes,
		"process_presentation": toolProcessPresentation(req, def),
	}
	if normalized.presentation != nil {
		payload["presentation"] = normalized.presentation
	}
	if normalized.truncated {
		payload["truncate_reason"] = normalized.truncateReason
		payload["original_size"] = normalized.originalSize
	}
	if normalized.resultArtifact != nil {
		payload["hash"] = normalized.resultArtifact.hash
		payload["size_bytes"] = normalized.resultArtifact.sizeBytes
		payload["mime_type"] = normalized.resultArtifact.mimeType
	}
	preview := mustMarshalJSON(payload)
	event := baseToolEvent(ids, now, tc, req, def, toolEventCompleted, preview, nil)
	event.PayloadRef = normalized.resultRef
	event.DebugRef = normalized.debugRef
	return event
}

func buildToolArtifactCreatedEvent(ids observability.IDGenerator, now time.Time, tc observability.TraceContext, req ToolCallRequest, def *ToolDefinition, item normalizedArtifact) (observability.AgentEvent, error) {
	return NormalizeToolEvent(ids, buildToolArtifactCreatedToolEvent(ids, now, tc, req, def, item))
}

func buildToolArtifactCreatedToolEvent(ids observability.IDGenerator, now time.Time, tc observability.TraceContext, req ToolCallRequest, def *ToolDefinition, item normalizedArtifact) ToolEvent {
	payload := map[string]any{
		"tool_call_id":         req.ToolCallID,
		"tool_name":            req.ToolName,
		"tool_version":         req.ToolVersion,
		"tool_type":            string(def.Type),
		"artifact_ref":         item.ref,
		"artifact_type":        string(item.typeName),
		"mime_type":            item.mimeType,
		"size_bytes":           item.sizeBytes,
		"hash":                 item.hash,
		"visibility":           string(item.visibility),
		"process_presentation": toolProcessPresentation(req, def),
	}
	preview := mustMarshalJSON(payload)
	event := baseToolEvent(ids, now, tc, req, def, ToolEventArtifactCreated, preview, nil)
	event.IdempotencyKey = req.RunID + ":" + req.ToolCallID + ":artifact:" + hashString(item.ref)[:24]
	event.Visibility = item.visibility
	event.PayloadRef = item.ref
	return event
}

func buildToolArgumentArtifactCreatedToolEvent(
	ids observability.IDGenerator,
	now time.Time,
	tc observability.TraceContext,
	req ToolCallRequest,
	def *ToolDefinition,
	meta *artifact.ArtifactMeta,
) ToolEvent {
	payload := map[string]any{
		"tool_call_id":         req.ToolCallID,
		"tool_name":            req.ToolName,
		"tool_version":         req.ToolVersion,
		"tool_type":            string(def.Type),
		"artifact_ref":         meta.ArtifactRef,
		"artifact_type":        string(meta.ArtifactType),
		"mime_type":            meta.MimeType,
		"size_bytes":           meta.SizeBytes,
		"input_size_bytes":     meta.SizeBytes,
		"hash":                 meta.Hash,
		"visibility":           string(meta.Visibility),
		"process_presentation": toolProcessPresentation(req, def),
	}
	event := baseToolEvent(ids, now, tc, req, def, ToolEventArtifactCreated, mustMarshalJSON(payload), nil)
	event.IdempotencyKey = req.RunID + ":" + req.ToolCallID + ":artifact:" + hashString(meta.ArtifactRef)[:24]
	event.Visibility = observability.VisibilityDebug
	event.PayloadRef = meta.ArtifactRef
	event.DebugRef = meta.ArtifactRef
	return event
}

func buildToolFailedEvent(ids observability.IDGenerator, now time.Time, tc observability.TraceContext, req ToolCallRequest, def *ToolDefinition, failure *ToolFailure, duration time.Duration, retryCount int) (observability.AgentEvent, error) {
	return NormalizeToolEvent(ids, buildToolFailedToolEvent(ids, now, tc, req, def, failure, duration, retryCount))
}

func buildToolFailedToolEvent(ids observability.IDGenerator, now time.Time, tc observability.TraceContext, req ToolCallRequest, def *ToolDefinition, failure *ToolFailure, duration time.Duration, retryCount int) ToolEvent {
	payload := map[string]any{
		"tool_call_id":         req.ToolCallID,
		"tool_name":            req.ToolName,
		"tool_version":         req.ToolVersion,
		"tool_type":            string(def.Type),
		"display_name":         def.DisplayName,
		"tool_failure":         failure,
		"duration_ms":          duration.Milliseconds(),
		"retry_count":          retryCount,
		"process_presentation": toolProcessPresentation(req, def),
	}
	preview := mustMarshalJSON(payload)
	return baseToolEvent(ids, now, tc, req, def, toolEventFailed, preview, &observability.EventError{
		Code:      eventErrorCode(failure.ErrorType),
		Type:      canonicalEventErrorType(ErrorType(failure.ErrorType)),
		Message:   failure.SafeUserMessage,
		Retryable: failure.Retryable,
	})
}

func canonicalEventErrorType(errorType ErrorType) observability.EventErrorType {
	switch errorType {
	case ErrorTypeTimeout:
		return observability.EventErrorTimeout
	case ErrorTypeCancelled:
		return observability.EventErrorCancelled
	case ErrorTypePermissionDenied, ErrorTypeControlRequired:
		return observability.EventErrorPermissionDenied
	case ErrorTypeSchemaValidationFailed, ErrorTypeInvalidArgument:
		return observability.EventErrorSchemaValidation
	case ErrorTypeRateLimited:
		return observability.EventErrorRateLimited
	case ErrorTypeUpstreamError:
		return observability.EventErrorUpstream
	default:
		return observability.EventErrorInternal
	}
}

func baseToolEvent(ids observability.IDGenerator, now time.Time, tc observability.TraceContext, req ToolCallRequest, def *ToolDefinition, eventType ToolEventType, payload json.RawMessage, eventErr *observability.EventError) ToolEvent {
	visibility := def.Visibility
	if visibility == "" {
		visibility = observability.VisibilityDebug
	}
	eventID := ""
	if ids != nil {
		eventID = ids.NewEventID()
	}
	canonicalType, _ := canonicalToolEventType(eventType)
	return ToolEvent{
		ToolEventID:         eventID,
		SchemaVersion:       ToolEventSchemaVersion,
		IdempotencyKey:      toolLifecycleIdempotencyKey(req.RunID, req.ToolCallID, canonicalType),
		TraceID:             nonEmpty(req.TraceID, tc.TraceID),
		SpanID:              nonEmpty(req.SpanID, tc.SpanID),
		ParentSpanID:        tc.ParentSpanID,
		SessionID:           req.SessionID,
		RunID:               req.RunID,
		StepID:              req.StepID,
		AgentID:             req.AgentID,
		AgentType:           tc.AgentType,
		Runtime:             tc.Runtime,
		EventType:           eventType,
		ToolCallID:          req.ToolCallID,
		ToolName:            req.ToolName,
		ToolVersion:         req.ToolVersion,
		ToolType:            def.Type,
		ProcessPresentation: toolProcessPresentation(req, def),
		Visibility:          visibility,
		PayloadPreview:      payload,
		Error:               eventErr,
		CreatedAt:           now,
	}
}

func toolProcessPresentation(req ToolCallRequest, def *ToolDefinition) processpresentation.Metadata {
	stage := req.ProcessStage
	if processpresentation.ValidateStage(stage) != nil || stage.Empty() {
		stage = processpresentation.DefaultStage()
	}
	key := strings.TrimSpace(req.ToolName)
	label := "执行工具"
	if def != nil {
		if strings.TrimSpace(def.Name) != "" {
			key = strings.TrimSpace(def.Name)
		}
		if strings.TrimSpace(def.DisplayName) != "" {
			label = strings.TrimSpace(def.DisplayName)
		}
	}
	return processpresentation.Metadata{
		Stage: stage,
		Activity: processpresentation.Activity{
			Key: key, Label: label, DetailLevel: processpresentation.DetailLevelSecondary,
		},
	}
}

func mustMarshalJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return data
}

func buildArgumentsPreview(preview map[string]any, maxBytes int) any {
	if preview == nil {
		return nil
	}
	redacted := redactSensitiveValue(preview)
	data, err := json.Marshal(redacted)
	if err != nil {
		return nil
	}
	if maxBytes <= 0 {
		maxBytes = 1024
	}
	if len(data) > maxBytes {
		end := maxBytes
		for end > 0 && !utf8.Valid(data[:end]) {
			end--
		}
		return string(data[:end]) + "..."
	}
	return json.RawMessage(data)
}

func jsonRawToAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw)
	}
	return value
}

func nonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func eventErrorCode(errorType string) string {
	if errorType == "" {
		return "TOOL_INTERNAL_ERROR"
	}
	return "TOOL_" + strings.ToUpper(strings.ReplaceAll(errorType, "-", "_"))
}
