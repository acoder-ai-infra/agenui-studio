package toolgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

const (
	defaultToolEventMaxPayloadBytes = 4096
	defaultToolEventMaxPreviewBytes = 1024
	defaultToolEventDedupeEntries   = 10000
)

type ToolEventSink interface {
	Emit(ctx context.Context, event ToolEvent) (*ToolEventEmitResult, error)
}

type ToolEventEmitResult struct {
	PersistedEvent *observability.AgentEvent `json:"persisted_event,omitempty"`
	Coalesced      bool                      `json:"coalesced,omitempty"`
	Dropped        bool                      `json:"dropped,omitempty"`
	Reason         string                    `json:"reason,omitempty"`
}

type ToolEventPolicy struct {
	MaxPayloadBytes int `json:"max_payload_bytes,omitempty"`
	MaxPreviewBytes int `json:"max_preview_bytes,omitempty"`
}

type ToolEventSinkConfig struct {
	EventStore       EventStore
	ArtifactStore    artifact.ArtifactStore
	IDGenerator      observability.IDGenerator
	Clock            Clock
	Logger           observability.StructuredLogger
	Policy           ToolEventPolicy
	MaxDedupeEntries int
}

type toolEventDedupeEntry struct {
	done   chan struct{}
	result *ToolEventEmitResult
	err    error
}

type DefaultToolEventSink struct {
	eventStore    EventStore
	artifactStore artifact.ArtifactStore
	ids           observability.IDGenerator
	clock         Clock
	logger        observability.StructuredLogger
	policy        ToolEventPolicy

	dedupeMu         sync.Mutex
	deduped          map[string]*toolEventDedupeEntry
	dedupeOrder      []string
	maxDedupeEntries int
}

func NewDefaultToolEventSink(config ToolEventSinkConfig) *DefaultToolEventSink {
	ids := config.IDGenerator
	if ids == nil {
		ids = observability.NewULIDGenerator("")
	}
	clock := config.Clock
	if clock == nil {
		clock = realClock{}
	}
	logger := config.Logger
	if logger == nil {
		logger = observability.NoopLogger{}
	}
	policy := config.Policy
	if policy.MaxPayloadBytes <= 0 {
		policy.MaxPayloadBytes = defaultToolEventMaxPayloadBytes
	}
	if policy.MaxPreviewBytes <= 0 {
		policy.MaxPreviewBytes = defaultToolEventMaxPreviewBytes
	}
	maxDedupeEntries := config.MaxDedupeEntries
	if maxDedupeEntries <= 0 {
		maxDedupeEntries = defaultToolEventDedupeEntries
	}
	return &DefaultToolEventSink{
		eventStore: config.EventStore, artifactStore: config.ArtifactStore,
		ids: ids, clock: clock, logger: logger, policy: policy,
		deduped: make(map[string]*toolEventDedupeEntry), maxDedupeEntries: maxDedupeEntries,
	}
}

func (s *DefaultToolEventSink) Emit(ctx context.Context, event ToolEvent) (*ToolEventEmitResult, error) {
	if s == nil {
		return nil, NewToolError(ErrorTypeInternal, "tool event sink is required", false, nil)
	}
	event, err := reconcileToolEventIdentity(ctx, event)
	if err != nil {
		return nil, err
	}

	dedupeKey := toolEventDedupeKey(event)
	if dedupeKey == "" {
		result, emitErr := s.emit(ctx, event)
		return cloneToolEventEmitResult(result), emitErr
	}

	entry, owner := s.reserveDedupe(dedupeKey)
	if !owner {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-entry.done:
			return cloneToolEventEmitResult(entry.result), entry.err
		}
	}
	result, err := s.emit(ctx, event)
	s.finishDedupe(dedupeKey, entry, result, err)
	if err != nil {
		return nil, err
	}
	return cloneToolEventEmitResult(result), nil
}

func (s *DefaultToolEventSink) reserveDedupe(key string) (*toolEventDedupeEntry, bool) {
	s.dedupeMu.Lock()
	defer s.dedupeMu.Unlock()
	if existing, ok := s.deduped[key]; ok {
		return existing, false
	}
	entry := &toolEventDedupeEntry{done: make(chan struct{})}
	s.deduped[key] = entry
	return entry, true
}

func (s *DefaultToolEventSink) finishDedupe(key string, entry *toolEventDedupeEntry, result *ToolEventEmitResult, emitErr error) {
	s.dedupeMu.Lock()
	defer s.dedupeMu.Unlock()
	if emitErr != nil {
		entry.err = emitErr
		delete(s.deduped, key)
	} else {
		entry.result = cloneToolEventEmitResult(result)
		s.dedupeOrder = append(s.dedupeOrder, key)
		for len(s.dedupeOrder) > s.maxDedupeEntries {
			oldest := s.dedupeOrder[0]
			s.dedupeOrder = s.dedupeOrder[1:]
			delete(s.deduped, oldest)
		}
	}
	close(entry.done)
}

func (s *DefaultToolEventSink) emit(ctx context.Context, event ToolEvent) (*ToolEventEmitResult, error) {
	switch event.EventType {
	case ToolEventProgress, ToolEventWarning, ToolEventArtifactCreated, ToolEventDebug:
	default:
		return nil, NewToolError(ErrorTypeSchemaValidationFailed, "tool event type is not tool-emittable", false, nil)
	}
	if event.SchemaVersion == "" {
		event.SchemaVersion = ToolEventSchemaVersion
	}
	if event.SchemaVersion != ToolEventSchemaVersion {
		return nil, NewToolError(ErrorTypeSchemaValidationFailed, "unsupported tool event schema version", false, nil)
	}
	event = stripToolEmittablePrivateFields(event)
	if event.ToolEventID == "" {
		event.ToolEventID = s.ids.NewEventID()
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = s.clock.Now()
	}

	var payload map[string]any
	if event.EventType == ToolEventDebug && len(event.Payload) == 0 && strings.TrimSpace(event.DebugRef) != "" {
		payload = make(map[string]any)
	} else {
		var err error
		payload, err = decodeToolEventPayload(event.Payload)
		if err != nil {
			return nil, err
		}
	}
	if err := validateToolEventPayload(event, payload); err != nil {
		return nil, err
	}
	if event.Visibility != "" && !isCanonicalEventVisibility(event.Visibility) {
		return nil, NewToolError(ErrorTypeSchemaValidationFailed, "invalid tool event visibility", false, nil)
	}
	if event.EventType == ToolEventArtifactCreated {
		return s.emitArtifact(ctx, event, payload)
	}

	visibility := event.Visibility
	if visibility == "" {
		visibility = observability.VisibilityDebug
	}
	if event.EventType == ToolEventDebug && visibility != observability.VisibilityDebug {
		return nil, NewToolError(ErrorTypeSchemaValidationFailed, "debug tool event visibility must be debug", false, nil)
	}
	event.Visibility = visibility

	if event.EventType == ToolEventWarning {
		payload["kind"] = "warning"
	}
	overlayCanonicalToolIdentity(payload, event)
	sanitized, err := sanitizeToolEventPayload(payload)
	if err != nil {
		return nil, err
	}
	if event.EventType == ToolEventDebug {
		return s.emitDebug(ctx, event, payload, sanitized)
	}
	return s.emitDurable(ctx, event, sanitized)
}

func (s *DefaultToolEventSink) emitArtifact(ctx context.Context, event ToolEvent, payload map[string]any) (*ToolEventEmitResult, error) {
	ref, _ := payload["artifact_ref"].(string)
	ref = strings.TrimSpace(ref)
	if event.PayloadRef != "" && ref != event.PayloadRef {
		return nil, NewToolError(ErrorTypeArtifactError, "tool artifact reference mismatch", false, nil)
	}
	if ref == "" {
		ref = event.PayloadRef
	}
	if ref == "" {
		return nil, NewToolError(ErrorTypeArtifactError, "tool artifact reference is required", false, nil)
	}
	if s.artifactStore == nil {
		return nil, NewToolError(ErrorTypeArtifactError, "artifact store is required to verify tool event artifact", false, nil)
	}
	meta, err := s.artifactStore.Head(ctx, ref)
	if err != nil {
		return nil, NewToolError(ErrorTypeArtifactError, "verify tool event artifact", false, err)
	}
	normalized, err := normalizeArtifactMeta(toolEventRequest(event), ref, meta)
	if err != nil {
		return nil, err
	}
	if event.Visibility != "" && event.Visibility != normalized.visibility {
		return nil, NewToolError(ErrorTypeArtifactError, "tool artifact visibility does not match authoritative metadata", false, nil)
	}
	event.Visibility = normalized.visibility
	event.PayloadRef = ref
	event.IdempotencyKey = event.RunID + ":" + event.ToolCallID + ":artifact:" + hashString(ref)[:24]

	authoritative := map[string]any{
		"artifact_ref":  meta.ArtifactRef,
		"artifact_type": string(meta.ArtifactType),
		"mime_type":     meta.MimeType,
		"size_bytes":    meta.SizeBytes,
		"hash":          meta.Hash,
		"visibility":    string(meta.Visibility),
		"step_id":       meta.StepID,
		"owner_module":  string(meta.OwnerModule),
		"owner_id":      meta.OwnerID,
	}
	overlayCanonicalToolIdentity(authoritative, event)
	sanitized, err := sanitizeToolEventPayload(authoritative)
	if err != nil {
		return nil, err
	}
	preview, err := buildToolEventPreview(sanitized, s.policy.MaxPreviewBytes)
	if err != nil {
		return nil, err
	}
	return s.appendDurable(ctx, event, preview)
}

func (s *DefaultToolEventSink) emitDebug(ctx context.Context, event ToolEvent, payload map[string]any, sanitized json.RawMessage) (*ToolEventEmitResult, error) {
	if s.artifactStore == nil {
		return nil, NewToolError(ErrorTypeArtifactError, "artifact store is required for tool debug event", false, nil)
	}
	rawRef := strings.TrimSpace(event.DebugRef)
	if payloadRef, ok := payload["raw_ref"].(string); ok && strings.TrimSpace(payloadRef) != "" {
		payloadRef = strings.TrimSpace(payloadRef)
		if rawRef != "" && rawRef != payloadRef {
			return nil, NewToolError(ErrorTypeArtifactError, "tool debug artifact reference mismatch", false, nil)
		}
		rawRef = payloadRef
	}
	if rawRef != "" {
		object, err := s.artifactStore.Get(ctx, rawRef, artifact.GetOptions{Purpose: artifact.PurposeDebug})
		if err != nil {
			return nil, NewToolError(ErrorTypeArtifactError, "verify tool debug artifact", false, err)
		}
		if object == nil || object.Content == nil {
			return nil, NewToolError(ErrorTypeArtifactError, "tool debug artifact has no content", false, nil)
		}
		meta := object.Meta
		if err := object.Content.Close(); err != nil {
			return nil, NewToolError(ErrorTypeArtifactError, "close tool debug artifact", false, err)
		}
		normalized, err := normalizeArtifactMeta(toolEventRequest(event), rawRef, &meta)
		if err != nil {
			return nil, err
		}
		if meta.ArtifactType != artifact.ArtifactTypeDebugPayload || normalized.visibility != observability.VisibilityDebug {
			return nil, NewToolError(ErrorTypeArtifactError, "tool debug artifact metadata does not match debug policy", false, nil)
		}
	} else {
		meta, err := s.putToolEventPayloadArtifact(ctx, event, sanitized, artifact.VisibilityDebug, "tool-event-debug")
		if err != nil {
			return nil, err
		}
		rawRef = meta.ArtifactRef
	}

	logger := observability.LoggerFrom(ctx, s.logger)
	logger.Debug(ctx, "tool debug artifact stored",
		observability.String("tool_call_id", event.ToolCallID),
		observability.String("tool_name", event.ToolName),
		observability.String("artifact_ref", rawRef),
	)
	return &ToolEventEmitResult{Reason: "debug_artifact_only"}, nil
}

func (s *DefaultToolEventSink) emitDurable(ctx context.Context, event ToolEvent, sanitized json.RawMessage) (*ToolEventEmitResult, error) {
	preview, err := buildToolEventPreview(sanitized, s.policy.MaxPreviewBytes)
	if err != nil {
		return nil, err
	}
	if len(sanitized) > s.policy.MaxPayloadBytes {
		meta, err := s.putToolEventPayloadArtifact(ctx, event, sanitized, artifact.VisibilityInternal, "tool-event-payload")
		if err != nil {
			return nil, err
		}
		event.PayloadRef = meta.ArtifactRef
	}
	return s.appendDurable(ctx, event, preview)
}

func (s *DefaultToolEventSink) appendDurable(ctx context.Context, event ToolEvent, preview json.RawMessage) (*ToolEventEmitResult, error) {
	if s.eventStore == nil {
		return nil, NewToolError(ErrorTypeInternal, "event store is required for durable tool event", false, nil)
	}
	event.Payload = nil
	event.PayloadPreview = preview
	normalized, err := NormalizeToolEvent(s.ids, event)
	if err != nil {
		return nil, err
	}
	appendResult, err := s.eventStore.AppendEvent(ctx, normalized)
	if err != nil {
		return nil, err
	}
	if appendResult == nil {
		return nil, NewToolError(ErrorTypeInternal, "event store returned no persisted event", false, nil)
	}
	if err := validatePersistedEvent(normalized, appendResult.Event); err != nil {
		return nil, err
	}
	persisted := cloneAgentEvent(appendResult.Event)
	if err := emitPersistedEvent(ctx, persisted); err != nil {
		return nil, err
	}
	return &ToolEventEmitResult{PersistedEvent: &persisted}, nil
}

func buildToolEventPreview(sanitized json.RawMessage, maxBytes int) (json.RawMessage, error) {
	if maxBytes <= 0 {
		return nil, NewToolError(ErrorTypeInternal, "tool event preview bound is required", false, nil)
	}
	if len(sanitized) <= maxBytes {
		return append(json.RawMessage(nil), sanitized...), nil
	}
	var payload map[string]any
	if err := json.Unmarshal(sanitized, &payload); err != nil || payload == nil {
		return nil, NewToolError(ErrorTypeSchemaValidationFailed, "tool event payload must be a sanitized JSON object", false, err)
	}
	preview := map[string]any{
		"tool_call_id": payload["tool_call_id"],
		"tool_name":    payload["tool_name"],
		"tool_version": payload["tool_version"],
		"tool_type":    payload["tool_type"],
		"truncated":    true,
	}
	encoded, err := json.Marshal(preview)
	if err != nil {
		return nil, NewToolError(ErrorTypeSchemaValidationFailed, "encode tool event preview", false, err)
	}
	if len(encoded) > maxBytes {
		return nil, NewToolError(ErrorTypeInternal, "tool event preview bound cannot preserve canonical identity", false, nil)
	}

	priority := []string{
		"kind", "code", "stage", "percent", "message", "retryable",
		"artifact_ref", "artifact_type", "mime_type", "size_bytes", "hash", "visibility",
		"step_id", "owner_module", "owner_id",
	}
	seen := map[string]struct{}{
		"tool_call_id": {}, "tool_name": {}, "tool_version": {}, "tool_type": {}, "truncated": {},
	}
	remaining := make([]string, 0, len(payload))
	for key := range payload {
		if _, ok := seen[key]; !ok {
			remaining = append(remaining, key)
		}
	}
	sort.Strings(remaining)
	keys := make([]string, 0, len(priority)+len(remaining))
	keys = append(keys, priority...)
	keys = append(keys, remaining...)
	for _, key := range keys {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		value, ok := payload[key]
		if !ok {
			continue
		}
		preview[key] = value
		candidate, marshalErr := json.Marshal(preview)
		if marshalErr != nil {
			delete(preview, key)
			continue
		}
		if len(candidate) > maxBytes {
			delete(preview, key)
			continue
		}
		encoded = candidate
	}
	return encoded, nil
}

func (s *DefaultToolEventSink) putToolEventPayloadArtifact(
	ctx context.Context,
	event ToolEvent,
	payload json.RawMessage,
	visibility artifact.Visibility,
	idempotencyPrefix string,
) (*artifact.ArtifactMeta, error) {
	if s.artifactStore == nil {
		return nil, NewToolError(ErrorTypeArtifactError, "artifact store is required for tool event payload", false, nil)
	}
	meta, err := s.artifactStore.Put(ctx, artifact.PutArtifactRequest{
		TenantID: event.TenantID, UserID: event.UserID, SessionID: event.SessionID, RunID: event.RunID, StepID: event.StepID,
		OwnerModule: artifact.OwnerModuleToolGateway, OwnerID: event.ToolCallID,
		ArtifactType: artifact.ArtifactTypeDebugPayload, MimeType: "application/json",
		Visibility: visibility, Content: bytes.NewReader(payload), RetentionPolicy: artifact.RetentionDebugShortTTL,
		CreatedBy:      "tool:" + event.ToolName,
		IdempotencyKey: idempotencyPrefix + ":" + event.TenantID + ":" + event.RunID + ":" + event.ToolCallID + ":" + event.ToolEventID,
	})
	if err != nil {
		return nil, NewToolError(ErrorTypeArtifactError, "write tool event payload artifact", false, err)
	}
	if meta == nil || meta.ArtifactRef == "" {
		return nil, NewToolError(ErrorTypeArtifactError, "tool event payload artifact has no reference", false, nil)
	}
	normalized, err := normalizeArtifactMeta(toolEventRequest(event), meta.ArtifactRef, meta)
	if err != nil {
		return nil, err
	}
	wantVisibility, _ := artifactEventVisibility(visibility)
	if meta.StepID != event.StepID || meta.OwnerModule != artifact.OwnerModuleToolGateway || meta.OwnerID != event.ToolCallID ||
		meta.ArtifactType != artifact.ArtifactTypeDebugPayload || meta.MimeType != "application/json" ||
		normalized.visibility != wantVisibility || meta.RetentionPolicy != artifact.RetentionDebugShortTTL {
		return nil, NewToolError(ErrorTypeArtifactError, "tool event payload artifact metadata does not match policy", false, nil)
	}
	return meta, nil
}

func reconcileToolEventIdentity(ctx context.Context, event ToolEvent) (ToolEvent, error) {
	if tc, ok := observability.TraceContextFrom(ctx); ok {
		identities := []struct {
			name    string
			value   *string
			trusted string
		}{
			{name: "trace id", value: &event.TraceID, trusted: tc.TraceID},
			{name: "tenant id", value: &event.TenantID, trusted: tc.TenantID},
			{name: "user id", value: &event.UserID, trusted: tc.UserID},
			{name: "session id", value: &event.SessionID, trusted: tc.SessionID},
			{name: "run id", value: &event.RunID, trusted: tc.RunID},
			{name: "agent id", value: &event.AgentID, trusted: tc.AgentID},
		}
		for _, identity := range identities {
			if *identity.value != "" && identity.trusted != "" && *identity.value != identity.trusted {
				return ToolEvent{}, NewToolError(ErrorTypePermissionDenied, "tool event "+identity.name+" does not match trusted context", false, nil)
			}
			if *identity.value == "" {
				*identity.value = identity.trusted
			}
		}
		if event.SpanID == "" {
			event.SpanID = tc.SpanID
		}
		if event.ParentSpanID == "" {
			event.ParentSpanID = tc.ParentSpanID
		}
		if event.AgentType == "" {
			event.AgentType = tc.AgentType
		}
		if event.Runtime == "" {
			event.Runtime = tc.Runtime
		}
	}
	for name, value := range map[string]string{
		"trace id": event.TraceID, "tenant id": event.TenantID, "session id": event.SessionID,
		"run id": event.RunID, "step id": event.StepID, "agent id": event.AgentID,
		"tool call id": event.ToolCallID, "tool name": event.ToolName, "tool version": event.ToolVersion,
	} {
		if strings.TrimSpace(value) == "" {
			return ToolEvent{}, NewToolError(ErrorTypeSchemaValidationFailed, "tool event "+name+" is required", false, nil)
		}
	}
	if !isSupportedToolType(event.ToolType) {
		return ToolEvent{}, NewToolError(ErrorTypeSchemaValidationFailed, "tool event tool type is required and must be canonical", false, nil)
	}
	return event, nil
}

func decodeToolEventPayload(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, NewToolError(ErrorTypeSchemaValidationFailed, "tool event payload object is required", false, nil)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		return nil, NewToolError(ErrorTypeSchemaValidationFailed, "tool event payload must be a JSON object", false, err)
	}
	return payload, nil
}

func validateToolEventPayload(event ToolEvent, payload map[string]any) error {
	switch event.EventType {
	case ToolEventProgress:
		if percent, ok := payload["percent"]; ok {
			value, ok := percent.(float64)
			if !ok || math.Trunc(value) != value || value < 0 || value > 100 {
				return NewToolError(ErrorTypeSchemaValidationFailed, "tool progress percent must be an integer from 0 through 100", false, nil)
			}
		}
	case ToolEventWarning:
		code, codeOK := payload["code"].(string)
		message, messageOK := payload["message"].(string)
		if !codeOK || !messageOK || strings.TrimSpace(code) == "" || strings.TrimSpace(message) == "" {
			return NewToolError(ErrorTypeSchemaValidationFailed, "tool warning code and message are required", false, nil)
		}
	case ToolEventArtifactCreated:
		ref, ok := payload["artifact_ref"].(string)
		if (!ok || strings.TrimSpace(ref) == "") && strings.TrimSpace(ref) == "" {
			return NewToolError(ErrorTypeArtifactError, "tool artifact reference is required", false, nil)
		}
	case ToolEventDebug:
		message, messageOK := payload["message"].(string)
		_, hasData := payload["data"]
		rawRef, rawRefOK := payload["raw_ref"].(string)
		if (!messageOK || strings.TrimSpace(message) == "") && !hasData && (!rawRefOK || strings.TrimSpace(rawRef) == "") && strings.TrimSpace(event.DebugRef) == "" {
			return NewToolError(ErrorTypeSchemaValidationFailed, "tool debug message, data, or raw_ref is required", false, nil)
		}
	}
	return nil
}

func stripToolEmittablePrivateFields(event ToolEvent) ToolEvent {
	event.PayloadPreview = nil
	event.IdempotencyKey = ""
	event.Error = nil
	switch event.EventType {
	case ToolEventProgress, ToolEventWarning:
		event.PayloadRef = ""
		event.DebugRef = ""
	case ToolEventArtifactCreated:
		event.DebugRef = ""
	case ToolEventDebug:
		event.PayloadRef = ""
	}
	return event
}

func overlayCanonicalToolIdentity(payload map[string]any, event ToolEvent) {
	payload["tool_call_id"] = event.ToolCallID
	payload["tool_name"] = event.ToolName
	payload["tool_version"] = event.ToolVersion
	payload["tool_type"] = string(event.ToolType)
	if !event.ProcessPresentation.Empty() {
		payload["process_presentation"] = event.ProcessPresentation
	}
}

func sanitizeToolEventPayload(payload map[string]any) (json.RawMessage, error) {
	data, err := json.Marshal(redactSensitiveValue(payload))
	if err != nil {
		return nil, NewToolError(ErrorTypeSchemaValidationFailed, "sanitize tool event payload", false, err)
	}
	return data, nil
}

func toolEventRequest(event ToolEvent) ToolCallRequest {
	return ToolCallRequest{
		ToolCallID: event.ToolCallID, TraceID: event.TraceID, SpanID: event.SpanID,
		TenantID: event.TenantID, UserID: event.UserID, SessionID: event.SessionID,
		RunID: event.RunID, StepID: event.StepID, AgentID: event.AgentID,
		ToolName: event.ToolName, ToolVersion: event.ToolVersion,
		ProcessStage: event.ProcessPresentation.Stage,
	}
}

func toolEventDedupeKey(event ToolEvent) string {
	if strings.TrimSpace(event.ExternalEventID) == "" {
		return ""
	}
	return event.TenantID + "\x00" + event.RunID + "\x00" + event.ToolCallID + "\x00" + event.ExternalEventID
}

func cloneToolEventEmitResult(result *ToolEventEmitResult) *ToolEventEmitResult {
	if result == nil {
		return nil
	}
	cloned := *result
	if result.PersistedEvent != nil {
		event := cloneAgentEvent(*result.PersistedEvent)
		cloned.PersistedEvent = &event
	}
	return &cloned
}

func isSupportedToolType(toolType ToolType) bool {
	switch toolType {
	case ToolTypeFunction, ToolTypeHTTP, ToolTypeMCP:
		return true
	default:
		return false
	}
}
