package agentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	artifactstore "github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storagewrite"
)

var ErrFinalResponseTooLarge = errors.New("final response exceeds runtime capture limit")

const (
	DefaultFinalArtifactThreshold = 64 * 1024
	DefaultFinalResponseMaxBytes  = 4 * 1024 * 1024
	DefaultFinalResponsePreview   = 512
)

type FinalizationPolicy struct {
	ArtifactThreshold int
	MaxContentBytes   int
	PreviewRunes      int
}

func DefaultFinalizationPolicy() FinalizationPolicy {
	return FinalizationPolicy{
		ArtifactThreshold: DefaultFinalArtifactThreshold,
		MaxContentBytes:   DefaultFinalResponseMaxBytes,
		PreviewRunes:      DefaultFinalResponsePreview,
	}
}

type AssistantMessageWrite struct {
	MessageID   string                        `json:"message_id"`
	SessionID   string                        `json:"session_id"`
	RunID       string                        `json:"run_id"`
	Role        string                        `json:"role"`
	Content     string                        `json:"content,omitempty"`
	ContentRef  string                        `json:"content_ref"`
	Preview     string                        `json:"preview"`
	ContentHash string                        `json:"content_hash"`
	SizeBytes   int                           `json:"size_bytes"`
	ContentType string                        `json:"content_type"`
	CreatedAt   time.Time                     `json:"created_at"`
	Visibility  observability.EventVisibility `json:"visibility"`
}

type FinalArtifactWrite struct {
	ArtifactID   string                        `json:"artifact_id"`
	SessionID    string                        `json:"session_id"`
	RunID        string                        `json:"run_id"`
	ArtifactType string                        `json:"artifact_type"`
	Content      []byte                        `json:"content"`
	ContentHash  string                        `json:"content_hash"`
	SizeBytes    int                           `json:"size_bytes"`
	ContentType  string                        `json:"content_type"`
	CreatedAt    time.Time                     `json:"created_at"`
	Visibility   observability.EventVisibility `json:"visibility"`
}

type FinalResponseIndex struct {
	MessageID   string `json:"message_id"`
	ContentRef  string `json:"content_ref"`
	Preview     string `json:"preview"`
	ContentHash string `json:"content_hash"`
	SizeBytes   int    `json:"size_bytes"`
	ContentType string `json:"content_type"`
}

type finalResponseAccumulator struct {
	content  strings.Builder
	maxBytes int
	overflow bool
}

func newFinalResponseAccumulator(maxBytes int) *finalResponseAccumulator {
	if maxBytes <= 0 {
		maxBytes = DefaultFinalResponseMaxBytes
	}
	return &finalResponseAccumulator{maxBytes: maxBytes}
}

func (a *finalResponseAccumulator) Add(event observability.AgentEvent) {
	if a == nil || (event.EventType != EventAgentTextDelta && event.EventType != EventFinalResponse) {
		return
	}
	text := finalTextFromPayload(event.Payload)
	if text == "" {
		return
	}
	if event.EventType == EventFinalResponse {
		a.content.Reset()
		a.overflow = false
	}
	if a.overflow {
		return
	}
	if a.content.Len()+len(text) > a.maxBytes {
		a.overflow = true
		return
	}
	a.content.WriteString(text)
}

func (a *finalResponseAccumulator) Content() (string, error) {
	if a != nil && a.overflow {
		return "", NewRuntimeError(ErrorRuntime, "FINAL_RESPONSE_TOO_LARGE", "final response exceeds runtime limit").WithCause(ErrFinalResponseTooLarge)
	}
	if a == nil {
		return "", nil
	}
	return a.content.String(), nil
}

func finalTextFromPayload(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var value struct {
		Text    string `json:"text"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(payload, &value); err != nil {
		return ""
	}
	if value.Text != "" {
		return value.Text
	}
	return value.Content
}

func (s *RuntimeService) finalizeRun(ctx context.Context, req RunRequest, accumulator *finalResponseAccumulator, pendingRunCompleted *observability.AgentEvent) (observability.AgentEvent, observability.AgentEvent, error) {
	content, err := accumulator.Content()
	if err != nil {
		return observability.AgentEvent{}, observability.AgentEvent{}, err
	}
	policy := s.Finalization
	if policy.ArtifactThreshold <= 0 {
		policy.ArtifactThreshold = DefaultFinalArtifactThreshold
	}
	if policy.PreviewRunes <= 0 {
		policy.PreviewRunes = DefaultFinalResponsePreview
	}
	now := time.Now()
	visibility := finalResultVisibility(req.ResultVisibility)
	contentHash := sha256Hex(content)
	messageID := "msg_" + stableRuntimeFactID(req.RunID, "assistant_message")
	contentRef := "message://" + messageID + "/content"
	messageContent := content
	var artifact *FinalArtifactWrite
	if len(content) > policy.ArtifactThreshold || len([]rune(content)) > policy.PreviewRunes {
		artifactID := "artifact_" + stableRuntimeFactID(req.RunID, "final_result")
		contentRef = artifactstore.BuildRef(finalizationTenantID(ctx, req), req.SessionID, req.RunID, artifactID)
		messageContent = ""
		artifact = &FinalArtifactWrite{
			ArtifactID: artifactID, SessionID: req.SessionID, RunID: req.RunID,
			ArtifactType: "final_result", Content: []byte(content), ContentHash: contentHash,
			SizeBytes: len(content), ContentType: "text/plain; charset=utf-8", CreatedAt: now, Visibility: visibility,
		}
	}
	index := FinalResponseIndex{
		MessageID: messageID, ContentRef: contentRef, Preview: previewText(content, policy.PreviewRunes),
		ContentHash: contentHash, SizeBytes: len(content), ContentType: "text/plain; charset=utf-8",
	}
	message := AssistantMessageWrite{
		MessageID: messageID, SessionID: req.SessionID, RunID: req.RunID, Role: "assistant",
		Content: messageContent, ContentRef: contentRef, Preview: index.Preview, ContentHash: contentHash,
		SizeBytes: len(content), ContentType: index.ContentType, CreatedAt: now, Visibility: visibility,
	}
	finalEvent := s.normalizeEvent(ctx, req, observability.AgentEvent{
		EventID:        "evt_" + stableRuntimeFactID(req.RunID, "final_response"),
		IdempotencyKey: req.RunID + ":final_response",
		EventType:      EventFinalResponse,
		Visibility:     visibility,
		Payload:        JSONPayload(index),
		PayloadPreview: JSONPayload(map[string]string{"text": index.Preview}),
		PayloadRef:     contentRef,
	})
	completedEvent := observability.AgentEvent{EventType: EventRunCompleted, Visibility: visibility}
	if pendingRunCompleted != nil {
		completedEvent = *pendingRunCompleted
	}
	completedEvent.EventID = "evt_" + stableRuntimeFactID(req.RunID, "run_completed")
	completedEvent.IdempotencyKey = req.RunID + ":run_completed"
	completedEvent.EventType = EventRunCompleted
	completedEvent.Visibility = visibility
	completedEvent = s.normalizeEvent(ctx, req, completedEvent)
	required := make([]storagewrite.Write, 0, 5)
	if artifact != nil {
		required = append(required, storagewrite.Write{Store: storagewrite.StoreArtifact, Operation: storagewrite.OperationPut, Ref: contentRef, Payload: *artifact})
	}
	required = append(required,
		storagewrite.Write{Store: storagewrite.StoreMessage, Operation: storagewrite.OperationInsert, Ref: "message:" + messageID, Payload: message},
		storagewrite.Write{Store: storagewrite.StoreEvent, Operation: storagewrite.OperationAppend, Ref: "event:" + req.RunID + ":final_response", Payload: finalEvent},
		storagewrite.Write{Store: storagewrite.StoreEvent, Operation: storagewrite.OperationAppend, Ref: "event:" + req.RunID + ":run_completed", Payload: completedEvent},
		storagewrite.Write{Store: storagewrite.StoreRun, Operation: storagewrite.OperationUpdate, Ref: "run:" + req.RunID + ":completed", Payload: RunStoreWrite{Action: RunStoreActionComplete}},
	)
	result, err := s.Writer.Execute(ctx, s.plan(ctx, req, "runtime.finalize", required...))
	if err == nil {
		for _, write := range result.Required {
			event, ok := write.Write.Payload.(observability.AgentEvent)
			if !ok {
				continue
			}
			switch event.EventType {
			case EventFinalResponse:
				finalEvent.Sequence = write.Receipt.Sequence
			case EventRunCompleted:
				completedEvent.Sequence = write.Receipt.Sequence
			}
		}
	}
	return finalEvent, completedEvent, err
}

func finalResultVisibility(visibility observability.EventVisibility) observability.EventVisibility {
	switch visibility {
	case observability.VisibilityInternal, observability.VisibilityDebug, observability.VisibilityRestricted:
		return visibility
	default:
		return observability.VisibilityUserVisible
	}
}

func finalizationTenantID(ctx context.Context, req RunRequest) string {
	tenantID := observability.MustTraceContext(ctx).TenantID
	if tenantID == "" {
		tenantID = req.TenantID
	}
	if tenantID == "" {
		tenantID = "default"
	}
	return tenantID
}

func stableRuntimeFactID(runID, kind string) string {
	sum := sha256.Sum256([]byte(runID + ":" + kind))
	return hex.EncodeToString(sum[:16])
}

func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func previewText(content string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	var preview strings.Builder
	count := 0
	for _, r := range content {
		if count == maxRunes {
			break
		}
		preview.WriteRune(r)
		count++
	}
	return preview.String()
}

func (p FinalizationPolicy) Validate() error {
	if p.ArtifactThreshold < 0 || p.MaxContentBytes < 0 || p.PreviewRunes < 0 {
		return fmt.Errorf("invalid finalization policy")
	}
	return nil
}
