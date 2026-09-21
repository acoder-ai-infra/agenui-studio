package toolgateway

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/processpresentation"
)

type ToolProgressEvent struct {
	Stage      string                        `json:"stage,omitempty"`
	Message    string                        `json:"message,omitempty"`
	Percent    *int                          `json:"percent,omitempty"`
	Data       json.RawMessage               `json:"data,omitempty"`
	Visibility observability.EventVisibility `json:"visibility,omitempty"`
}

type ToolDebugEvent struct {
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	RawRef  string          `json:"raw_ref,omitempty"`
}

type ToolWarningEvent struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

type ToolArtifactEvent struct {
	ArtifactRef string          `json:"artifact_ref"`
	MimeType    string          `json:"mime_type,omitempty"`
	SizeBytes   int64           `json:"size_bytes,omitempty"`
	Hash        string          `json:"hash,omitempty"`
	Preview     json.RawMessage `json:"preview,omitempty"`
}

type ToolContext interface {
	EmitProgress(ctx context.Context, event ToolProgressEvent) error
	EmitDebug(ctx context.Context, event ToolDebugEvent) error
	EmitWarning(ctx context.Context, event ToolWarningEvent) error
	EmitArtifact(ctx context.Context, event ToolArtifactEvent) error
	IsCancelled() bool
	Trace() observability.TraceContext
}

type defaultToolContext struct {
	executionCtx        context.Context
	trace               observability.TraceContext
	req                 ToolCallRequest
	toolType            ToolType
	processPresentation processpresentation.Metadata
	sink                ToolEventSink
	logger              observability.StructuredLogger

	mu     sync.Mutex
	events []observability.AgentEvent
}

func newDefaultToolContext(
	executionCtx context.Context,
	tc observability.TraceContext,
	req ToolCallRequest,
	def *ToolDefinition,
	sink ToolEventSink,
	logger observability.StructuredLogger,
) *defaultToolContext {
	if executionCtx == nil {
		executionCtx = context.Background()
	}
	if logger == nil {
		logger = observability.NoopLogger{}
	}
	toolType := ToolType("")
	presentation := processpresentation.Metadata{Stage: req.ProcessStage}
	if def != nil {
		toolType = def.Type
		presentation = toolProcessPresentation(req, def)
		if req.ToolVersion == "" {
			req.ToolVersion = def.Version
		}
	}
	return &defaultToolContext{
		executionCtx:        executionCtx,
		trace:               cloneTraceContext(tc),
		req:                 req,
		toolType:            toolType,
		processPresentation: presentation,
		sink:                sink,
		logger:              logger,
	}
}

func (c *defaultToolContext) EmitProgress(_ context.Context, progress ToolProgressEvent) error {
	payload, err := marshalToolContextPayload(progress)
	if err != nil {
		return err
	}
	visibility := progress.Visibility
	if visibility == "" {
		visibility = observability.VisibilityUserVisible
	}
	event := c.baseEvent(ToolEventProgress, visibility, payload)
	return c.emit(event, true)
}

func (c *defaultToolContext) EmitDebug(_ context.Context, debug ToolDebugEvent) error {
	payload, err := marshalToolContextPayload(debug)
	if err != nil {
		return err
	}
	event := c.baseEvent(ToolEventDebug, observability.VisibilityDebug, payload)
	event.DebugRef = debug.RawRef
	return c.emit(event, false)
}

func (c *defaultToolContext) EmitWarning(_ context.Context, warning ToolWarningEvent) error {
	payload, err := marshalToolContextPayload(warning)
	if err != nil {
		return err
	}
	event := c.baseEvent(ToolEventWarning, observability.VisibilityUserVisible, payload)
	return c.emit(event, true)
}

func (c *defaultToolContext) EmitArtifact(_ context.Context, item ToolArtifactEvent) error {
	payload, err := marshalToolContextPayload(item)
	if err != nil {
		return err
	}
	event := c.baseEvent(ToolEventArtifactCreated, "", payload)
	event.PayloadRef = item.ArtifactRef
	return c.emit(event, false)
}

func (c *defaultToolContext) IsCancelled() bool {
	return c == nil || c.executionCtx.Err() != nil
}

func (c *defaultToolContext) Trace() observability.TraceContext {
	if c == nil {
		return observability.TraceContext{}
	}
	return cloneTraceContext(c.trace)
}

func (c *defaultToolContext) persistedEvents() []observability.AgentEvent {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	events := make([]observability.AgentEvent, len(c.events))
	for i := range c.events {
		events[i] = cloneAgentEvent(c.events[i])
	}
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Sequence < events[j].Sequence
	})
	return events
}

func (c *defaultToolContext) baseEvent(eventType ToolEventType, visibility observability.EventVisibility, payload json.RawMessage) ToolEvent {
	return ToolEvent{
		EventType: eventType,
		TenantID:  c.req.TenantID, UserID: c.req.UserID,
		TraceID: nonEmpty(c.req.TraceID, c.trace.TraceID), SpanID: nonEmpty(c.req.SpanID, c.trace.SpanID), ParentSpanID: c.trace.ParentSpanID,
		SessionID: c.req.SessionID, RunID: c.req.RunID, StepID: c.req.StepID,
		AgentID: c.req.AgentID, AgentType: c.trace.AgentType, Runtime: c.trace.Runtime,
		ToolCallID: c.req.ToolCallID, ToolName: c.req.ToolName, ToolVersion: c.req.ToolVersion, ToolType: c.toolType,
		ProcessPresentation: c.processPresentation,
		Visibility:          visibility,
		Payload:             append(json.RawMessage(nil), payload...),
	}
}

func (c *defaultToolContext) emit(event ToolEvent, degradable bool) error {
	if c.sink == nil {
		if degradable {
			c.logDegradation("sink_unavailable")
			return nil
		}
		return NewToolError(ErrorTypeInternal, "tool event sink is unavailable", false, nil)
	}
	result, err := c.sink.Emit(c.executionCtx, event)
	if err != nil {
		if degradable {
			c.logDegradation("sink_error")
			return nil
		}
		return err
	}
	if result != nil && result.PersistedEvent != nil {
		persisted := cloneAgentEvent(*result.PersistedEvent)
		c.mu.Lock()
		c.events = append(c.events, persisted)
		c.mu.Unlock()
	}
	return nil
}

func (c *defaultToolContext) logDegradation(reason string) {
	c.logger.Warn(c.executionCtx, "tool event sink degraded",
		observability.String("fallback_type", "tool_event_degraded"),
		observability.String("tool_call_id", c.req.ToolCallID),
		observability.String("from", "tool_event_sink"),
		observability.String("to", "structured_log"),
		observability.String("reason", reason),
		observability.String("impact", "process_event_replay_unavailable_p0_lifecycle_preserved"),
	)
}

func marshalToolContextPayload(value any) (json.RawMessage, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, NewToolError(ErrorTypeSchemaValidationFailed, "encode tool context event payload", false, err)
	}
	return payload, nil
}

func cloneTraceContext(tc observability.TraceContext) observability.TraceContext {
	if tc.Baggage != nil {
		baggage := make(map[string]string, len(tc.Baggage))
		for key, value := range tc.Baggage {
			baggage[key] = value
		}
		tc.Baggage = baggage
	}
	return tc
}
