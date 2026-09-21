package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentregistry"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

const (
	agentChatProfileSchemaVersion    = "harness.agent_chat_profile.v1"
	agentChatTranscriptSchemaVersion = "harness.agent_chat_transcript.v1"
)

type agentChatTranscriptSession struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agentId"`
	Title     string    `json:"title,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// resolveAgentCard resolves an Agent's capability card in a tenant-aware way:
// tenant-published (console) agents are served from their released version's
// frozen Prepared.Card, falling back to the static base registry for
// bootstrap-YAML agents. This is what lets console-created-and-published agents
// (which only live in AgentConfigControl, not the static registry) be
// discovered and chatted with (audit I-09).
func (d *Deps) resolveAgentCard(r *http.Request, agentID string) (agentregistry.CapabilityCard, error) {
	if d.AgentConfigControl != nil {
		if tc, ok := observability.TraceContextFrom(r.Context()); ok && tc.TenantID != "" {
			release, err := d.AgentConfigControl.GetRelease(r.Context(), tc.TenantID, d.ConfigEnvironment, agentID)
			if err == nil {
				if version, verr := d.AgentConfigControl.GetVersion(r.Context(), tc.TenantID, agentID, release.Version); verr == nil && version.Prepared != nil {
					return version.Prepared.Card, nil
				}
			} else if !errors.Is(err, agentregistry.ErrAgentConfigControlNotFound) {
				return agentregistry.CapabilityCard{}, err
			}
		}
	}
	if d.AgentConfigSource != nil {
		return d.AgentConfigSource.GetCapabilityCard(r.Context(), agentID, "")
	}
	return agentregistry.CapabilityCard{}, agentregistry.ErrAgentNotFound
}

func (d *Deps) authorizeAgentChatAgent(w http.ResponseWriter, r *http.Request, agentID string) bool {
	if d.AgentConfigSource == nil && d.AgentConfigControl == nil {
		return true
	}
	card, err := d.resolveAgentCard(r, agentID)
	if err != nil {
		if errors.Is(err, agentregistry.ErrAgentNotFound) || errors.Is(err, agentregistry.ErrAgentConfigControlNotFound) {
			writeError(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
		} else {
			writeError(w, http.StatusServiceUnavailable, "AGENT_PROFILE_UNAVAILABLE", err.Error())
		}
		return false
	}
	if card.Status != agentregistry.AgentStatusEnabled {
		writeError(w, http.StatusConflict, "AGENT_NOT_AVAILABLE", "agent is not enabled")
		return false
	}
	return true
}

type agentChatTranscriptMessage struct {
	ID        string    `json:"id"`
	TurnID    string    `json:"turnId,omitempty"`
	RunID     string    `json:"runId,omitempty"`
	Role      string    `json:"role"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"createdAt"`
}

type agentChatTranscriptRun struct {
	RunID       string            `json:"runId"`
	TurnID      string            `json:"turnId,omitempty"`
	ParentRunID string            `json:"parentRunId,omitempty"`
	AgentID     string            `json:"agentId,omitempty"`
	Status      storage.RunStatus `json:"status"`
	ErrorCode   string            `json:"errorCode,omitempty"`
	StartedAt   time.Time         `json:"startedAt,omitempty"`
	EndedAt     time.Time         `json:"endedAt,omitempty"`
}

type agentChatTranscriptAttachment struct {
	ArtifactRef string    `json:"artifactRef"`
	RunID       string    `json:"runId"`
	MessageID   string    `json:"messageId,omitempty"`
	Name        string    `json:"name"`
	MimeType    string    `json:"mimeType"`
	SizeBytes   int64     `json:"sizeBytes"`
	Type        string    `json:"type"`
	Source      string    `json:"source"`
	OwnerModule string    `json:"ownerModule,omitempty"`
	OwnerID     string    `json:"ownerId,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// authorizeAgentChatSession preserves the normal session ownership checks and
// additionally prevents a conversation created for one agent from being sent
// to another agent through a caller-supplied session id.
func (d *Deps) authorizeAgentChatSession(w http.ResponseWriter, r *http.Request, sessionID, agentID string) bool {
	if sessionID == "" {
		return true
	}
	if !d.authorizeSessionForOpen(w, r, sessionID) {
		return false
	}
	session, err := d.Stores.Sessions.Get(r.Context(), sessionID)
	if err != nil {
		if storage.IsErrorCode(err, storage.ErrNotFound) {
			return true
		}
		writeError(w, statusForStorageErr(err), "SESSION_GET_FAILED", err.Error())
		return false
	}
	if session.AgentID != agentID {
		writeError(w, http.StatusForbidden, "SESSION_AGENT_MISMATCH", "session belongs to another agent")
		return false
	}
	return true
}

func (d *Deps) handleAgentChatProfile(w http.ResponseWriter, r *http.Request) {
	agentID := strings.TrimSpace(r.PathValue("agentId"))
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "MISSING_AGENT_ID", "agent id required")
		return
	}
	if d.AgentConfigSource == nil && d.AgentConfigControl == nil {
		writeError(w, http.StatusServiceUnavailable, "AGENT_CATALOG_UNAVAILABLE", "agent catalog not configured")
		return
	}
	card, err := d.resolveAgentCard(r, agentID)
	if err != nil {
		if errors.Is(err, agentregistry.ErrAgentNotFound) || errors.Is(err, agentregistry.ErrAgentConfigControlNotFound) {
			writeError(w, http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "AGENT_PROFILE_UNAVAILABLE", err.Error())
		return
	}
	if card.Status != agentregistry.AgentStatusEnabled {
		writeError(w, http.StatusConflict, "AGENT_NOT_AVAILABLE", "agent is not enabled")
		return
	}
	name := card.AgentID
	if configs, listErr := d.AgentConfigSource.ListAgentConfigs(r.Context()); listErr == nil {
		for _, config := range configs {
			if config.AgentID == card.AgentID && config.Version == card.Version && strings.TrimSpace(config.Name) != "" {
				name = config.Name
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schemaVersion": agentChatProfileSchemaVersion,
		"agentId":       card.AgentID,
		"name":          name,
		"description":   card.Description,
		"agentType":     card.AgentType,
		"version":       card.Version,
		"status":        card.Status,
		"availability":  "available",
		"outputFormat":  card.OutputFormat,
		"features": map[string]bool{
			"attachments": d.Artifacts != nil,
			"cancel":      d.Canceller != nil,
			"multiTurn":   true,
			"processView": true,
		},
		"attachmentPolicy": map[string]any{
			"maxFiles":        agentChatMaxFiles,
			"maxFileBytes":    agentChatMaxFileBytes,
			"maxRequestBytes": agentChatMaxBodyBytes,
		},
	})
}

func (d *Deps) handleAgentChatTranscript(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.PathValue("sid"))
	session, ok := d.authorizeSession(w, r, sessionID)
	if !ok {
		return
	}
	detail := strings.TrimSpace(r.URL.Query().Get("detail"))
	if detail == "" {
		detail = "normal"
	}
	if detail != "summary" && detail != "normal" && detail != "verbose" {
		writeError(w, http.StatusBadRequest, "INVALID_TRANSCRIPT_DETAIL", "detail must be summary, normal, or verbose")
		return
	}
	limit := parseLimit(r, 50)
	if limit > 100 {
		limit = 100
	}
	messagePage, err := d.Stores.Messages.List(r.Context(), storage.MessageListQuery{
		SessionID:    sessionID,
		Limit:        limit,
		Visibilities: protocol.VisibilitiesForTarget(targetFor(r)),
	})
	if err != nil {
		writeError(w, statusForStorageErr(err), "MESSAGE_LIST_FAILED", err.Error())
		return
	}
	runs, err := d.Stores.Runs.ListBySession(r.Context(), sessionID)
	if err != nil {
		writeError(w, statusForStorageErr(err), "RUN_LIST_FAILED", err.Error())
		return
	}

	messages := make([]agentChatTranscriptMessage, 0, len(messagePage.Items))
	visibleRunIDs := make(map[string]bool, len(messagePage.Items))
	visibleRunOrder := make([]string, 0, len(messagePage.Items))
	for _, item := range messagePage.Items {
		messages = append(messages, agentChatTranscriptMessage{
			ID: item.ID, TurnID: item.TurnID, RunID: item.RunID, Role: item.Role,
			Text: agentChatDisplayMessageText(item.ContentPreview), CreatedAt: item.CreatedAt,
		})
		if item.RunID != "" {
			if !visibleRunIDs[item.RunID] {
				visibleRunOrder = append(visibleRunOrder, item.RunID)
			}
			visibleRunIDs[item.RunID] = true
		}
	}
	runsByID := make(map[string]*storage.Run, len(runs))
	for _, run := range runs {
		runsByID[run.RunID] = run
	}
	visibleRunOrder = agentChatRunTreeOrder(runs, visibleRunOrder)
	runViews := make([]agentChatTranscriptRun, 0, len(visibleRunOrder))
	attachments := make([]agentChatTranscriptAttachment, 0)
	files := make([]agentChatTranscriptAttachment, 0)
	commentaryByRun := make(map[string][]map[string]any, len(runs))
	processByRun := make(map[string][]map[string]any, len(runs))
	contextByRun := make(map[string][]map[string]any, len(runs))
	controlsByRun := make(map[string][]map[string]any, len(runs))
	processProjector := newAgentChatProcessProjector()
	reasoningBoundaries := make(map[string][]time.Time)
	for _, run := range runs {
		if run == nil || run.ParentRunID == "" {
			continue
		}
		if !run.StartedAt.IsZero() {
			reasoningBoundaries[run.ParentRunID] = append(reasoningBoundaries[run.ParentRunID], run.StartedAt)
		}
		if !run.EndedAt.IsZero() {
			reasoningBoundaries[run.ParentRunID] = append(reasoningBoundaries[run.ParentRunID], run.EndedAt)
		}
	}
	for runID := range reasoningBoundaries {
		sort.Slice(reasoningBoundaries[runID], func(i, j int) bool {
			return reasoningBoundaries[runID][i].Before(reasoningBoundaries[runID][j])
		})
	}
	boundaryIndex := make(map[string]int, len(reasoningBoundaries))
	tc := observability.MustTraceContext(r.Context())
	for _, runID := range visibleRunOrder {
		run := runsByID[runID]
		if run == nil {
			continue
		}
		runViews = append(runViews, agentChatTranscriptRun{
			RunID: run.RunID, TurnID: run.TurnID, ParentRunID: run.ParentRunID, AgentID: run.AgentID, Status: run.Status,
			ErrorCode: run.ErrorCode, StartedAt: run.StartedAt, EndedAt: run.EndedAt,
		})
		events, queryErr := d.Stores.Events.Query(r.Context(), storage.EventQuery{
			RunID: run.RunID, Limit: 1000,
			Visibilities: protocol.VisibilitiesForTarget(targetFor(r)),
		})
		if queryErr != nil {
			writeError(w, statusForStorageErr(queryErr), "EVENT_LIST_FAILED", queryErr.Error())
			return
		}
		for _, event := range events {
			boundaries := reasoningBoundaries[run.RunID]
			for boundaryIndex[run.RunID] < len(boundaries) && !event.CreatedAt.Before(boundaries[boundaryIndex[run.RunID]]) {
				processProjector.BreakReasoning(run.RunID)
				boundaryIndex[run.RunID]++
			}
			if process, visible := processProjector.Project(event, tc.DebugEnabled); visible {
				decorateAgentChatProcess(process, run)
				processByRun[run.RunID] = append(processByRun[run.RunID], process)
			}
			if run.ParentRunID == "" {
				if commentary, visible := agentChatCommentaryProjection(event); visible {
					if len(commentaryByRun[run.RunID]) == 0 {
						commentaryByRun[run.RunID] = append(commentaryByRun[run.RunID], commentary)
					}
				}
			}
			if contextView, visible := agentChatContextProjection(event, tc.DebugEnabled); visible {
				decorateAgentChatContext(contextView, run)
				contextByRun[run.RunID] = append(contextByRun[run.RunID], contextView)
			}
			if controlView, visible := agentChatControlProjection(event); visible {
				controlsByRun[run.RunID] = append(controlsByRun[run.RunID], controlView)
			}
		}
		if d.Stores.Controls != nil && len(controlsByRun[run.RunID]) > 0 {
			controls, controlErr := d.Stores.Controls.ListByRun(r.Context(), run.RunID)
			if controlErr != nil {
				writeError(w, statusForStorageErr(controlErr), "CONTROL_LIST_FAILED", controlErr.Error())
				return
			}
			statusByRequest := make(map[string]string, len(controls))
			responseRefByRequest := make(map[string]string, len(controls))
			for _, control := range controls {
				statusByRequest[control.RequestID] = control.Status
				responseRefByRequest[control.RequestID] = control.ResponseRef
			}
			for _, controlView := range controlsByRun[run.RunID] {
				requestID, _ := controlView["requestId"].(string)
				if status := statusByRequest[requestID]; status != "" {
					controlView["status"] = status
				}
				if ref := responseRefByRequest[requestID]; ref != "" {
					d.decorateAgentChatControlAnswer(r, run, controlView, ref)
				}
			}
		}
		controlViews := controlsByRun[run.RunID]
		for index, controlView := range controlViews {
			// A waiting Run with no pending request means only its latest answered
			// request failed to resume. Older answered requests already resumed far
			// enough to create a successor and must stay ordinary history.
			d.finalizeAgentChatControlView(r, run, controlView, index == len(controlViews)-1)
		}
		if d.Artifacts == nil {
			continue
		}
		actorCtx := artifact.ContextWithActor(r.Context(), artifact.Actor{
			TenantID: tc.TenantID, UserID: tc.UserID, SessionID: sessionID,
			RunID: run.RunID, AgentID: run.AgentID, Role: artifact.ActorUser,
		})
		metas, listErr := d.Artifacts.List(actorCtx, artifact.ListQuery{
			TenantID: tc.TenantID, SessionID: sessionID, RunID: run.RunID,
			Visibility: artifact.VisibilityUserVisible,
		})
		if listErr != nil {
			writeError(w, http.StatusInternalServerError, "ARTIFACT_LIST_FAILED", listErr.Error())
			return
		}
		for _, meta := range metas {
			if !agentChatManageableArtifact(meta) {
				continue
			}
			file := agentChatFileViewFromMeta(meta)
			files = append(files, file)
			if agentChatUploadedAttachment(meta) {
				attachments = append(attachments, file)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schemaVersion": agentChatTranscriptSchemaVersion,
		"detail":        detail,
		"session": agentChatTranscriptSession{
			ID: session.ID, AgentID: session.AgentID, Title: session.Title, Status: session.Status,
			CreatedAt: session.CreatedAt, UpdatedAt: session.UpdatedAt,
		},
		"messages":        messages,
		"runs":            runViews,
		"attachments":     attachments,
		"files":           files,
		"commentaryByRun": commentaryByRun,
		"processByRun":    processByRun,
		"contextByRun":    contextByRun,
		"controlsByRun":   controlsByRun,
		"hasMore":         messagePage.HasMore,
		"nextCursor":      messagePage.NextBeforeMessageID,
	})
}

func agentChatFileViewFromMeta(meta artifact.ArtifactMeta) agentChatTranscriptAttachment {
	name := strings.TrimSpace(meta.Name)
	if name == "" {
		name = strings.TrimSpace(meta.ArtifactID)
	}
	if name == "" {
		name = "artifact"
	}
	return agentChatTranscriptAttachment{
		ArtifactRef: meta.ArtifactRef,
		RunID:       meta.RunID,
		MessageID:   meta.OwnerID,
		Name:        name,
		MimeType:    meta.MimeType,
		SizeBytes:   meta.SizeBytes,
		Type:        string(meta.ArtifactType),
		Source:      agentChatArtifactSource(meta),
		OwnerModule: string(meta.OwnerModule),
		OwnerID:     meta.OwnerID,
		CreatedAt:   meta.CreatedAt,
	}
}

func agentChatUploadedAttachment(meta artifact.ArtifactMeta) bool {
	return meta.OwnerModule == artifact.OwnerModuleProtocol && meta.Metadata["source"] == "agent_chat"
}

func agentChatArtifactSource(meta artifact.ArtifactMeta) string {
	if agentChatUploadedAttachment(meta) {
		return "uploaded"
	}
	return "generated"
}

func agentChatManageableArtifact(meta artifact.ArtifactMeta) bool {
	if meta.Visibility != artifact.VisibilityUserVisible || meta.Status != artifact.ArtifactStatusReady {
		return false
	}
	switch meta.ArtifactType {
	case artifact.ArtifactTypeFile, artifact.ArtifactTypeImage, artifact.ArtifactTypeToolResult, artifact.ArtifactTypeFinalResult, artifact.ArtifactTypeSchema:
		return true
	default:
		return false
	}
}

// agentChatDisplayMessageText keeps the runtime's attachment-enriched prompt
// intact in storage while presenting only the user's original text in history.
// Attachment metadata is rendered separately as file chips.
func agentChatDisplayMessageText(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.Index(value, "\n\n[Attachment:"); index >= 0 {
		value = strings.TrimSpace(value[:index])
	} else if strings.HasPrefix(value, "[Attachment:") {
		value = ""
	}
	if value == "" {
		return "已上传附件"
	}
	return value
}

// agentChatRunTreeOrder expands only roots present in the visible message page.
// This keeps pagination boundaries intact while restoring every descendant Run
// that belongs to those visible turns.
func agentChatRunTreeOrder(runs []*storage.Run, rootOrder []string) []string {
	children := make(map[string][]string)
	for _, run := range runs {
		if run != nil && run.ParentRunID != "" {
			children[run.ParentRunID] = append(children[run.ParentRunID], run.RunID)
		}
	}
	result := make([]string, 0, len(rootOrder))
	seen := make(map[string]bool, len(rootOrder))
	var appendTree func(string)
	appendTree = func(runID string) {
		if runID == "" || seen[runID] {
			return
		}
		seen[runID] = true
		result = append(result, runID)
		for _, childID := range children[runID] {
			appendTree(childID)
		}
	}
	for _, runID := range rootOrder {
		appendTree(runID)
	}
	return result
}

// finalizeAgentChatControlView prevents a historical ticket from remaining
// actionable after expiry or a local deployment-key change. Non-pending views
// never need to retain a bearer credential.
func (d *Deps) finalizeAgentChatControlView(r *http.Request, run *storage.Run, view map[string]any, latest bool) {
	status, _ := view["status"].(string)
	if status != "pending" {
		delete(view, "controlTicket")
		if status == "answered" && latest && run != nil && run.Status == storage.RunStatusWaitingControl {
			view["status"] = "resume_failed"
		}
		return
	}
	if d.ControlTickets == nil {
		return
	}
	requestID, _ := view["requestId"].(string)
	ticket, _ := view["controlTicket"].(string)
	if requestID == "" || ticket == "" {
		view["status"] = "unavailable"
		delete(view, "controlTicket")
		return
	}
	if _, err := d.resolveControlResumeToken(r, run, requestID, controlResponseRequest{ControlTicket: ticket}); err != nil {
		view["status"] = "unavailable"
		delete(view, "controlTicket")
	}
}

func (d *Deps) decorateAgentChatControlAnswer(r *http.Request, run *storage.Run, view map[string]any, responseRef string) {
	if d.Artifacts == nil || run == nil || responseRef == "" {
		return
	}
	status, _ := view["status"].(string)
	if status != "answered" {
		return
	}
	tc := observability.MustTraceContext(r.Context())
	ctx := artifact.ContextWithActor(r.Context(), artifact.Actor{
		TenantID: tc.TenantID, UserID: tc.UserID, SessionID: run.SessionID,
		RunID: run.RunID, AgentID: run.AgentID, Role: artifact.ActorAudit,
	})
	object, err := d.Artifacts.Get(ctx, responseRef, artifact.GetOptions{Purpose: artifact.PurposeReplay})
	if err != nil {
		return
	}
	defer object.Content.Close()
	data, err := io.ReadAll(io.LimitReader(object.Content, 64<<10))
	if err != nil {
		return
	}
	text, answers := agentChatControlAnswerProjection(data)
	if text != "" {
		view["answerText"] = text
	}
	if len(answers) > 0 {
		view["answers"] = answers
	}
}

type agentChatControlAnswerView struct {
	QuestionID string `json:"questionId"`
	Text       string `json:"text"`
}

func agentChatControlAnswerProjection(data []byte) (string, []agentChatControlAnswerView) {
	var payload struct {
		Text    string `json:"response_text"`
		Targets map[string]struct {
			Answers []struct {
				QuestionID string `json:"question_id"`
				Answer     string `json:"answer"`
				Text       string `json:"text"`
				Selected   *struct {
					Label string `json:"label"`
				} `json:"selected_option"`
			} `json:"answers"`
		} `json:"targets"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return "", nil
	}
	contextIDs := make([]string, 0, len(payload.Targets))
	for contextID := range payload.Targets {
		contextIDs = append(contextIDs, contextID)
	}
	sort.Strings(contextIDs)
	answerTexts := make([]string, 0)
	answers := make([]agentChatControlAnswerView, 0)
	seen := make(map[string]struct{})
	for _, contextID := range contextIDs {
		for _, item := range payload.Targets[contextID].Answers {
			answer := strings.TrimSpace(item.Answer)
			if answer == "" {
				answer = strings.TrimSpace(item.Text)
			}
			if answer == "" && item.Selected != nil {
				answer = strings.TrimSpace(item.Selected.Label)
			}
			questionID := strings.TrimSpace(item.QuestionID)
			key := questionID + "\x00" + answer
			if answer == "" {
				continue
			}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			answer = boundedAgentChatText(answer, 2000)
			answerTexts = append(answerTexts, answer)
			if questionID != "" {
				answers = append(answers, agentChatControlAnswerView{
					QuestionID: boundedAgentChatText(questionID, 120),
					Text:       answer,
				})
			}
		}
	}
	text := boundedAgentChatText(payload.Text, 4000)
	if text == "" {
		text = boundedAgentChatText(strings.Join(answerTexts, "\n"), 4000)
	}
	return text, answers
}

func agentChatControlAnswerText(data []byte) string {
	text, _ := agentChatControlAnswerProjection(data)
	return text
}
