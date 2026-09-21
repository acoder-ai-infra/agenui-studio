package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/identifiercontract"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/processpresentation"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

const (
	agentChatMaxBodyBytes                      = 32 << 20
	agentChatMaxFileBytes                      = 10 << 20
	agentChatMaxFiles                          = 8
	agentChatMaxInlineTextBytes                = 256 << 10
	agentChatReasoningScopeTaskAcknowledgement = "task_acknowledgement"
	agentChatCommentarySchemaVersion           = "harness.agent_chat_commentary.v1"
	agentChatCursorSchemaVersion               = "harness.agent_chat_cursor.v1"
	agentChatMaxCursorRuns                     = 256
)

type agentChatCursor struct {
	SchemaVersion string           `json:"schemaVersion"`
	Sequences     map[string]int64 `json:"sequences"`
}

type agentChatRequest struct {
	SessionID string          `json:"sessionId,omitempty"`
	Prompt    string          `json:"prompt,omitempty"`
	Messages  []aiChatMessage `json:"messages,omitempty"`
}

type agentChatAttachment struct {
	Name     string
	MimeType string
	Content  []byte
}

type agentChatAttachmentView struct {
	ArtifactRef string `json:"artifactRef"`
	Name        string `json:"name"`
	MimeType    string `json:"mimeType"`
	SizeBytes   int64  `json:"sizeBytes"`
	Type        string `json:"type"`
}

// handleAgentChat is an additive, agent-addressed chat endpoint. It deliberately
// owns a separate request and stream projection so /api/v1/ai/chat keeps its
// existing contract unchanged.
func (d *Deps) handleAgentChat(w http.ResponseWriter, r *http.Request) {
	agentID := strings.TrimSpace(r.PathValue("agentId"))
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "MISSING_AGENT_ID", "agent id required")
		return
	}
	if !d.authorizeAgentChatAgent(w, r, agentID) {
		return
	}
	if d.RunService == nil {
		writeError(w, http.StatusInternalServerError, "RUN_SERVICE_UNAVAILABLE", "run service not configured")
		return
	}
	body, attachments, err := decodeAgentChatRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_CHAT_REQUEST", err.Error())
		return
	}
	if len(attachments) > 0 && d.Artifacts == nil {
		writeError(w, http.StatusNotImplemented, "ATTACHMENT_STORE_UNAVAILABLE", "attachment store not configured")
		return
	}
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" {
		prompt = strings.TrimSpace(lastUserText(body.Messages))
	}
	prompt = composeAgentChatPrompt(prompt, attachments)
	if prompt == "" {
		writeError(w, http.StatusBadRequest, "EMPTY_CHAT_MESSAGE", "prompt or attachment required")
		return
	}
	if !d.authorizeAgentChatSession(w, r, body.SessionID, agentID) {
		return
	}

	tc := observability.MustTraceContext(r.Context())
	tc.SessionID = body.SessionID
	tc.AgentID = agentID
	ctx := observability.WithTraceContext(r.Context(), tc)

	// 入口层预回合扩展管线（与 SDK Start 共享 kernel.TurnPipeline）：未配置
	// 时直通；失败 fail-closed，不得开 Turn。
	prepared, err := d.prepareTurnInput(ctx, &tc, agentID, prompt)
	if err != nil {
		writeTurnPipelineError(w, err)
		return
	}
	prompt = prepared.Preview
	ctx = observability.WithTraceContext(r.Context(), tc)

	turn, err := d.RunService.OpenTurn(ctx, storage.OpenTurnRequest{
		SessionID: tc.SessionID, TenantID: tc.TenantID, UserID: tc.UserID,
		AgentID: agentID, UserContentPreview: prompt,
		IdempotencyKey:       r.Header.Get("Idempotency-Key"),
		DeferContextSnapshot: len(attachments) > 0,
		ContextAttachments:   contextAttachmentsFromAgentChatItems(attachments),
		ContextFragments:     prepared.Fragments,
	})
	if err != nil {
		writeError(w, statusForStorageErr(err), "OPEN_TURN_FAILED", err.Error())
		return
	}
	tc.SessionID = turn.Session.ID
	tc.RunID = turn.Run.RunID
	ctx = observability.WithTraceContext(ctx, tc)
	w.Header().Set("x-harness-session-id", turn.Session.ID)
	w.Header().Set("x-harness-run-id", turn.Run.RunID)
	w.Header().Set("x-harness-agent-id", agentID)

	var sub <-chan observability.AgentEvent
	var cancel func()
	if d.Broker != nil {
		// Prefer a session-scoped subscription so descendant (sub-agent) run events
		// are pushed live too; streamAgentChat routes each event by RunID. A broker
		// without SessionSubscriber degrades to a run-scoped stream (root only), and
		// sub-agent activity falls back to the 1s replay poll.
		if ss, ok := d.Broker.(protocol.SessionSubscriber); ok {
			sub, cancel, _ = ss.SubscribeSession(ctx, turn.Session.ID)
		} else {
			sub, cancel, _ = d.Broker.Subscribe(ctx, turn.Run.RunID)
		}
		if cancel != nil {
			defer cancel()
		}
	}
	attachmentViews, err := d.persistAgentChatAttachments(ctx, turn, attachments, r.Header.Get("Idempotency-Key"))
	if err != nil {
		d.failAgentChatUndispatched(ctx, turn, err)
		writeError(w, http.StatusInternalServerError, "ATTACHMENT_UPLOAD_FAILED", err.Error())
		return
	}
	if len(attachmentViews) > 0 {
		if err := d.RunService.BuildContextSnapshot(ctx, turn, contextAttachmentsFromAgentChatViews(attachmentViews)); err != nil {
			writeError(w, statusForStorageErr(err), "CONTEXT_SNAPSHOT_FAILED", err.Error())
			return
		}
	}
	if err := d.dispatchNewTurn(ctx, turn); err != nil {
		writeDispatchError(w, err)
		return
	}
	aw, ok := protocol.NewAISDKWriter(w)
	if !ok {
		writeError(w, http.StatusInternalServerError, "STREAMING_UNSUPPORTED", "response writer is not a flusher")
		return
	}
	d.streamAgentChat(ctx, aw, turn.Run.RunID, sub, attachmentViews, nil)
}

// handleAgentChatResumeStream re-attaches a live AI-SDK stream to an EXISTING run
// (e.g. after a HITL control was answered and the run resumed). It never opens a
// new turn and never dispatches execution — it only replays persisted events and
// subscribes to live ones (replay-then-subscribe), so the browser follows the
// resumed run and its sub-agents in real time instead of polling the transcript.
func (d *Deps) handleAgentChatResumeStream(w http.ResponseWriter, r *http.Request) {
	sid := strings.TrimSpace(r.PathValue("sid"))
	rid := strings.TrimSpace(r.PathValue("rid"))
	if _, ok := d.authorizeRunInSession(w, r, sid, rid); !ok {
		return
	}
	cursor, err := parseAgentChatCursor(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_CHAT_CURSOR", err.Error())
		return
	}
	ctx := r.Context()
	tc := observability.MustTraceContext(ctx)
	tc.SessionID = sid
	tc.RunID = rid
	ctx = observability.WithTraceContext(ctx, tc)

	var sub <-chan observability.AgentEvent
	var cancel func()
	if d.Broker != nil {
		if ss, ok := d.Broker.(protocol.SessionSubscriber); ok {
			sub, cancel, _ = ss.SubscribeSession(ctx, sid)
		} else {
			sub, cancel, _ = d.Broker.Subscribe(ctx, rid)
		}
		if cancel != nil {
			defer cancel()
		}
	}
	aw, ok := protocol.NewAISDKWriter(w)
	if !ok {
		writeError(w, http.StatusInternalServerError, "STREAMING_UNSUPPORTED", "response writer is not a flusher")
		return
	}
	d.streamAgentChat(ctx, aw, rid, sub, nil, cursor)
}

func parseAgentChatCursor(r *http.Request) (map[string]int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if raw == "" {
		return nil, nil
	}
	var cursor agentChatCursor
	if err := json.Unmarshal([]byte(raw), &cursor); err != nil {
		return nil, errors.New("chat cursor must be valid JSON")
	}
	if cursor.SchemaVersion != agentChatCursorSchemaVersion {
		return nil, fmt.Errorf("chat cursor schemaVersion must be %q", agentChatCursorSchemaVersion)
	}
	if len(cursor.Sequences) > agentChatMaxCursorRuns {
		return nil, fmt.Errorf("chat cursor exceeds %d runs", agentChatMaxCursorRuns)
	}
	for runID, sequence := range cursor.Sequences {
		if strings.TrimSpace(runID) == "" || sequence < 0 {
			return nil, errors.New("chat cursor requires non-empty run IDs and non-negative sequences")
		}
	}
	return cursor.Sequences, nil
}

func decodeAgentChatRequest(w http.ResponseWriter, r *http.Request) (agentChatRequest, []agentChatAttachment, error) {
	r.Body = http.MaxBytesReader(w, r.Body, agentChatMaxBodyBytes)
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		var body agentChatRequest
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			return body, nil, err
		}
		return body, nil, nil
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return agentChatRequest{}, nil, err
	}
	defer r.MultipartForm.RemoveAll()
	body := agentChatRequest{SessionID: r.FormValue("sessionId"), Prompt: r.FormValue("prompt")}
	files := append([]*multipart.FileHeader(nil), r.MultipartForm.File["files"]...)
	files = append(files, r.MultipartForm.File["attachments"]...)
	if len(files) > agentChatMaxFiles {
		return body, nil, fmt.Errorf("too many attachments: maximum is %d", agentChatMaxFiles)
	}
	attachments := make([]agentChatAttachment, 0, len(files))
	for _, header := range files {
		file, err := header.Open()
		if err != nil {
			return body, nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(file, agentChatMaxFileBytes+1))
		closeErr := file.Close()
		if readErr != nil {
			return body, nil, readErr
		}
		if closeErr != nil {
			return body, nil, closeErr
		}
		if len(data) > agentChatMaxFileBytes {
			return body, nil, fmt.Errorf("attachment %q exceeds %d bytes", header.Filename, agentChatMaxFileBytes)
		}
		mimeType := strings.TrimSpace(header.Header.Get("Content-Type"))
		if mimeType == "" || mimeType == "application/octet-stream" {
			mimeType = http.DetectContentType(data)
		}
		attachments = append(attachments, agentChatAttachment{
			Name: filepath.Base(header.Filename), MimeType: mimeType, Content: data,
		})
	}
	return body, attachments, nil
}

func composeAgentChatPrompt(prompt string, attachments []agentChatAttachment) string {
	prompt = strings.TrimSpace(prompt)
	if prompt != "" {
		return prompt
	}
	if len(attachments) > 0 {
		return "用户上传了附件。"
	}
	return ""
}

func isInlineTextAttachment(item agentChatAttachment) bool {
	if len(item.Content) > agentChatMaxInlineTextBytes || !utf8.Valid(item.Content) {
		return false
	}
	return strings.HasPrefix(item.MimeType, "text/") || strings.Contains(item.MimeType, "json") || strings.Contains(item.MimeType, "xml") || strings.Contains(item.MimeType, "yaml")
}

func contextAttachmentsFromAgentChatItems(items []agentChatAttachment) []storage.ContextAttachmentSnapshot {
	if len(items) == 0 {
		return nil
	}
	out := make([]storage.ContextAttachmentSnapshot, 0, len(items))
	for _, item := range items {
		artifactType := string(artifact.ArtifactTypeFile)
		if strings.HasPrefix(item.MimeType, "image/") {
			artifactType = string(artifact.ArtifactTypeImage)
		}
		out = append(out, storage.ContextAttachmentSnapshot{
			Name: item.Name, MimeType: item.MimeType, SizeBytes: int64(len(item.Content)), Type: artifactType,
		})
	}
	return out
}

func contextAttachmentsFromAgentChatViews(views []agentChatAttachmentView) []storage.ContextAttachmentSnapshot {
	if len(views) == 0 {
		return nil
	}
	out := make([]storage.ContextAttachmentSnapshot, 0, len(views))
	for _, view := range views {
		out = append(out, storage.ContextAttachmentSnapshot{
			Name: view.Name, MimeType: view.MimeType, SizeBytes: view.SizeBytes,
			ArtifactRef: view.ArtifactRef, Type: view.Type,
		})
	}
	return out
}

func (d *Deps) persistAgentChatAttachments(ctx context.Context, turn *storage.OpenTurnResult, items []agentChatAttachment, requestKey string) ([]agentChatAttachmentView, error) {
	if len(items) == 0 {
		return nil, nil
	}
	tc := observability.MustTraceContext(ctx)
	actorCtx := artifact.ContextWithActor(ctx, artifact.Actor{
		TenantID: tc.TenantID, UserID: tc.UserID, SessionID: turn.Session.ID,
		RunID: turn.Run.RunID, AgentID: turn.Run.AgentID, Role: artifact.ActorUser,
	})
	views := make([]agentChatAttachmentView, 0, len(items))
	for i, item := range items {
		artifactType := artifact.ArtifactTypeFile
		if strings.HasPrefix(item.MimeType, "image/") {
			artifactType = artifact.ArtifactTypeImage
		}
		idempotencyKey := ""
		if requestKey != "" {
			idempotencyKey = fmt.Sprintf("agent-chat:%s:%d", requestKey, i)
		}
		meta, err := d.Artifacts.Put(actorCtx, artifact.PutArtifactRequest{
			TenantID: tc.TenantID, UserID: tc.UserID, SessionID: turn.Session.ID, RunID: turn.Run.RunID,
			OwnerModule: artifact.OwnerModuleProtocol, OwnerID: turn.UserMessage.ID,
			ArtifactType: artifactType, MimeType: item.MimeType, Name: item.Name,
			Visibility: artifact.VisibilityUserVisible, Content: bytes.NewReader(item.Content),
			RetentionPolicy: artifact.RetentionSessionTTL, CreatedBy: tc.UserID,
			IdempotencyKey: idempotencyKey, Metadata: map[string]string{"source": "agent_chat"},
		})
		if err != nil {
			return nil, err
		}
		view := agentChatAttachmentView{ArtifactRef: meta.ArtifactRef, Name: meta.Name, MimeType: meta.MimeType, SizeBytes: meta.SizeBytes, Type: string(meta.ArtifactType)}
		views = append(views, view)
		payload, _ := json.Marshal(view)
		appended, err := d.Stores.Events.Append(ctx, observability.AgentEvent{
			SessionID: turn.Session.ID, RunID: turn.Run.RunID, AgentID: turn.Run.AgentID,
			EventType: observability.EventArtifactCreated, Visibility: observability.VisibilityUserVisible,
			PayloadPreview: payload, PayloadRef: meta.ArtifactRef,
			IdempotencyKey: identifiercontract.ComposeIdempotencyKey("agent-chat-artifact", meta.ArtifactRef),
		})
		if err != nil {
			return nil, err
		}
		if d.Broker != nil {
			if err := d.Broker.Publish(ctx, appended.Event); err != nil {
				return nil, err
			}
		}
	}
	return views, nil
}

func (d *Deps) failAgentChatUndispatched(ctx context.Context, turn *storage.OpenTurnResult, cause error) {
	terminal, err := d.RunService.FailUndispatchedRun(ctx, turn.Run, cause)
	if err != nil {
		if d.Logger != nil {
			d.Logger.Error(ctx, "terminalize agent chat upload failure", err, observability.String("run_id", turn.Run.RunID))
		}
		return
	}
	if d.Broker != nil {
		_ = d.Broker.Publish(ctx, terminal.Event)
	}
}

func (d *Deps) streamAgentChat(ctx context.Context, aw *protocol.AISDKWriter, runID string, sub <-chan observability.AgentEvent, attachments []agentChatAttachmentView, initialCursor map[string]int64) {
	_ = aw.Part(protocol.AISDKStart(runID))
	_ = aw.Part(protocol.AISDKStartStep())
	if len(attachments) > 0 {
		_ = aw.Part(protocol.AISDKDataPart("attachments", attachments))
	}
	debugEnabled := observability.MustTraceContext(ctx).DebugEnabled
	visibilities := []observability.EventVisibility{observability.VisibilityUserVisible}
	if debugEnabled {
		visibilities = append(visibilities, observability.VisibilityDebug, observability.VisibilityInternal, observability.VisibilityRestricted)
	}
	seen := map[string]bool{}
	processProjector := newAgentChatProcessProjector()
	lastSequence := map[string]int64{}
	lastCursorJSON := ""
	emitCursor := func() {
		if len(lastSequence) == 0 {
			return
		}
		sequences := make(map[string]int64, len(lastSequence))
		for currentRunID, sequence := range lastSequence {
			if sequence > 0 {
				sequences[currentRunID] = sequence
			}
		}
		if len(sequences) == 0 {
			return
		}
		cursor := agentChatCursor{SchemaVersion: agentChatCursorSchemaVersion, Sequences: sequences}
		encoded, err := json.Marshal(cursor)
		if err != nil || string(encoded) == lastCursorJSON {
			return
		}
		lastCursorJSON = string(encoded)
		_ = aw.Part(protocol.AISDKDataPart("cursor", cursor))
	}
	lastRunState := map[string]string{}
	seenKey := func(event observability.AgentEvent) string {
		if event.EventID != "" {
			return "event:" + event.EventID
		}
		if event.RunID != "" && event.Sequence > 0 {
			return fmt.Sprintf("sequence:%s:%d", event.RunID, event.Sequence)
		}
		return ""
	}
	alreadySeen := func(event observability.AgentEvent) bool {
		key := seenKey(event)
		return key != "" && seen[key]
	}
	markSeen := func(event observability.AgentEvent) {
		if key := seenKey(event); key != "" {
			seen[key] = true
		}
	}
	// The subscription is session-scoped, so it also carries events from runs that
	// are not part of this root's tree (other turns in the same session). resolveRun
	// answers "is this RunID the root or one of its descendants?". Positive and
	// definitively unrelated verdicts are cached, but transient misses are retried:
	// live child events can arrive before the child Run row is readable.
	runCache := map[string]*storage.Run{}
	member := map[string]bool{runID: true}
	resolveRun := func(rid string) (*storage.Run, bool) {
		if verdict, ok := member[rid]; ok {
			return runCache[rid], verdict
		}
		target, err := d.Stores.Runs.Get(ctx, rid)
		if err != nil || target == nil {
			return nil, false
		}
		runCache[rid] = target
		for cur, hops := target, 0; cur != nil && hops < 64; hops++ {
			if cur.RunID == runID {
				member[rid] = true
				return target, true
			}
			if cur.ParentRunID == "" {
				break
			}
			parent := runCache[cur.ParentRunID]
			if parent == nil {
				parent, err = d.Stores.Runs.Get(ctx, cur.ParentRunID)
				if err != nil || parent == nil {
					return nil, false
				}
				runCache[cur.ParentRunID] = parent
			}
			cur = parent
		}
		member[rid] = false
		return nil, false
	}
	textOpen := false
	commentarySent := false
	const textID = "answer"
	emit := func(event observability.AgentEvent, run *storage.Run, root bool) bool {
		if root && !commentarySent {
			if commentary, ok := agentChatCommentaryProjection(event); ok {
				// The acknowledgement and the first process update share one
				// durable tool event; serialize the acknowledgement first.
				_ = aw.Part(protocol.AISDKDataPart("commentary", commentary))
				commentarySent = true
			}
		}
		if file, ok := d.agentChatFileProjection(ctx, event, run); ok {
			_ = aw.Part(protocol.AISDKDataPart("files", []agentChatTranscriptAttachment{file}))
		}
		if process, ok := processProjector.Project(event, debugEnabled); ok {
			decorateAgentChatProcess(process, run)
			_ = aw.Part(protocol.AISDKDataPart("process", process))
		}
		if contextView, ok := agentChatContextProjection(event, debugEnabled); ok {
			decorateAgentChatContext(contextView, run)
			_ = aw.Part(protocol.AISDKDataPart("context", contextView))
		}
		if !root {
			return false
		}
		if control, ok := agentChatControlProjection(event); ok {
			_ = aw.Part(protocol.AISDKDataPart("control", control))
			if textOpen {
				_ = aw.Part(protocol.AISDKTextEnd(textID))
			}
			return true
		}
		switch event.EventType {
		case observability.EventAgentTextDelta:
			// Raw assistant deltas include text from tool rounds and sub-agent
			// orchestration. Agent Chat renders only the explicit commentary and
			// the canonical final response; generic Harness streams stay unchanged.
		case observability.EventFinalResponse:
			if !textOpen {
				if text := eventText(event); text != "" {
					_ = aw.Part(protocol.AISDKTextStart(textID))
					_ = aw.Part(protocol.AISDKTextDelta(textID, text))
					textOpen = true
				}
			}
		case observability.EventRunFailed:
			if textOpen {
				_ = aw.Part(protocol.AISDKTextEnd(textID))
			}
			_ = aw.Part(protocol.AISDKError(agentChatFailureText(event, run)))
			return true
		case observability.EventRunCompleted, observability.EventRunCancelled, observability.EventRunExpired:
			if textOpen {
				_ = aw.Part(protocol.AISDKTextEnd(textID))
			}
			return true
		}
		return false
	}
	finish := func() {
		_ = aw.Part(protocol.AISDKFinishStep())
		_ = aw.Part(protocol.AISDKFinish())
		_ = aw.Done()
	}
	relevant := func(event observability.AgentEvent) bool {
		if event.Visibility == observability.VisibilityUserVisible {
			return true
		}
		switch event.EventType {
		case observability.EventRunFailed, observability.EventRunCancelled, observability.EventRunExpired:
			return true
		}
		return debugEnabled
	}
	replay := func() bool {
		rootRun, err := d.Stores.Runs.Get(ctx, runID)
		if err != nil || rootRun == nil {
			return false
		}
		runs, err := d.Stores.Runs.ListBySession(ctx, rootRun.SessionID)
		if err != nil {
			return false
		}
		runsByID := make(map[string]*storage.Run, len(runs))
		for _, run := range runs {
			runsByID[run.RunID] = run
		}
		order := agentChatRunTreeOrder(runs, []string{runID})
		for _, currentRunID := range order {
			currentRun := runsByID[currentRunID]
			if currentRun == nil || currentRun.ParentRunID == "" {
				continue
			}
			state := string(currentRun.Status) + "|" + currentRun.StartedAt.String() + "|" + currentRun.EndedAt.String()
			if lastRunState[currentRunID] != state {
				lastRunState[currentRunID] = state
				_ = aw.Part(protocol.AISDKDataPart("process", agentChatRunProjection(currentRun)))
			}
		}
		terminal := false
		for _, currentRunID := range order {
			currentRun := runsByID[currentRunID]
			if currentRun == nil {
				continue
			}
			if _, initialized := lastSequence[currentRunID]; !initialized {
				lastSequence[currentRunID] = initialCursor[currentRunID]
			}
			for {
				events, queryErr := d.Stores.Events.Query(ctx, storage.EventQuery{RunID: currentRunID, AfterSequence: lastSequence[currentRunID], Limit: 500, Visibilities: visibilities})
				if queryErr != nil {
					break
				}
				for _, event := range events {
					if event.Sequence > lastSequence[currentRunID] {
						lastSequence[currentRunID] = event.Sequence
					}
					if alreadySeen(event) || !relevant(event) {
						continue
					}
					markSeen(event)
					if emit(event, currentRun, currentRunID == runID) {
						terminal = true
					}
				}
				if len(events) < 500 {
					break
				}
			}
		}
		emitCursor()
		if !terminal && agentChatRunTerminal(rootRun.Status) {
			// A reconnect cursor may already be beyond the persisted terminal
			// event. The Run state still owns stream termination; do not leave the
			// client waiting on heartbeats merely because that event was skipped.
			terminal = true
		}
		return terminal
	}
	if replay() {
		finish()
		return
	}
	if event, ok := d.redactedTerminalFromRun(ctx, runID); ok {
		run, _ := d.Stores.Runs.Get(ctx, runID)
		_ = emit(event, run, true)
		finish()
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if replay() {
				finish()
				return
			}
			if event, ok := d.redactedTerminalFromRun(ctx, runID); ok {
				run, _ := d.Stores.Runs.Get(ctx, runID)
				_ = emit(event, run, true)
				finish()
				return
			}
		case event, ok := <-sub:
			if !ok {
				sub = nil
				continue
			}
			if alreadySeen(event) || !relevant(event) {
				continue
			}
			if event.RunID != runID {
				// Descendant (sub-agent) event pushed live: emit its process row only
				// (root=false). Events from unrelated runs in the session are dropped.
				child, ok := resolveRun(event.RunID)
				if !ok {
					continue
				}
				if event.Sequence > lastSequence[event.RunID] {
					lastSequence[event.RunID] = event.Sequence
				}
				state := string(child.Status) + "|" + child.StartedAt.String() + "|" + child.EndedAt.String()
				if lastRunState[event.RunID] != state {
					lastRunState[event.RunID] = state
					_ = aw.Part(protocol.AISDKDataPart("process", agentChatRunProjection(child)))
				}
				markSeen(event)
				_ = emit(event, child, false)
				emitCursor()
				continue
			}
			previousSequence := lastSequence[event.RunID]
			if event.Sequence > lastSequence[event.RunID] {
				lastSequence[event.RunID] = event.Sequence
			}
			markSeen(event)
			run, _ := d.Stores.Runs.Get(ctx, runID)
			terminal := emit(event, run, true)
			if terminal {
				// A best-effort live subscription may deliver the root terminal after
				// dropping an earlier persisted final_response. Rewind to the cursor
				// from before this terminal so replay can fill every durable event in
				// sequence order; seen still suppresses the terminal itself.
				lastSequence[event.RunID] = previousSequence
				_ = replay()
				finish()
				return
			}
			emitCursor()
		}
	}
}

func (d *Deps) agentChatFileProjection(ctx context.Context, event observability.AgentEvent, run *storage.Run) (agentChatTranscriptAttachment, bool) {
	if d.Artifacts == nil || event.Visibility != observability.VisibilityUserVisible || run == nil {
		return agentChatTranscriptAttachment{}, false
	}
	ref := strings.TrimSpace(event.PayloadRef)
	if ref == "" {
		payload := event.PayloadPreview
		if len(payload) == 0 {
			payload = event.Payload
		}
		var raw map[string]any
		if len(payload) == 0 || json.Unmarshal(payload, &raw) != nil {
			return agentChatTranscriptAttachment{}, false
		}
		if value, _ := raw["artifact_ref"].(string); value != "" {
			ref = value
		} else if value, _ := raw["artifactRef"].(string); value != "" {
			ref = value
		} else if value, _ := raw["result_ref"].(string); value != "" {
			ref = value
		}
	}
	if ref == "" {
		return agentChatTranscriptAttachment{}, false
	}
	tc := observability.MustTraceContext(ctx)
	actorCtx := artifact.ContextWithActor(ctx, artifact.Actor{
		TenantID: tc.TenantID, UserID: tc.UserID, SessionID: run.SessionID,
		RunID: run.RunID, AgentID: run.AgentID, Role: artifact.ActorUser,
	})
	meta, err := d.Artifacts.Head(actorCtx, ref)
	if err != nil || meta == nil || !agentChatManageableArtifact(*meta) {
		return agentChatTranscriptAttachment{}, false
	}
	return agentChatFileViewFromMeta(*meta), true
}

func agentChatFailureText(event observability.AgentEvent, run *storage.Run) string {
	code := ""
	if event.Error != nil {
		code = strings.TrimSpace(event.Error.Code)
	}
	if code == "" && run != nil {
		code = strings.TrimSpace(run.ErrorCode)
	}
	label := "运行失败"
	switch code {
	case "SUB_AGENT_TARGET_RESOLVE_FAILED", "sub_agent_target_resolve_failed":
		label = "子 Agent 配置不可用"
	case "TOOL_SCHEMA_VALIDATION_FAILED", "tool_schema_validation_failed":
		label = "工具参数校验失败"
	case "CONTEXT_BUILD_FAILED", "context_build_failed":
		label = "上下文恢复失败"
	case "RESUME_FAILED", "resume_failed":
		label = "运行恢复失败"
	case "MODEL_PROVIDER_4XX", "model_provider_4xx":
		label = "模型服务请求失败"
	case "MODEL_PROVIDER_5XX", "model_provider_5xx":
		label = "模型服务暂时不可用"
	case "MODEL_RATE_LIMITED", "model_rate_limited":
		label = "模型服务限流"
	case "MODEL_TIMEOUT", "model_timeout":
		label = "模型服务超时"
	}
	if code == "" {
		return label
	}
	return label + "（" + boundedAgentChatText(code, 80) + "）"
}

const agentChatControlSchemaVersion = "harness.agent_chat_control.v1"

type agentChatControlOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type agentChatControlQuestion struct {
	Header   string                   `json:"header,omitempty"`
	Question string                   `json:"question"`
	Options  []agentChatControlOption `json:"options,omitempty"`
}

// agentChatControlProjection is the public HITL projection for Agent Chat. It
// deliberately rebuilds the payload from a whitelist instead of forwarding the
// canonical event body. The opaque control ticket is safe for this principal
// and is the only credential the browser may return to the response endpoint.
func agentChatControlProjection(event observability.AgentEvent) (map[string]any, bool) {
	if event.EventType != observability.EventControlRequestCreated || event.Visibility != observability.VisibilityUserVisible {
		return nil, false
	}
	type rawInterrupt struct {
		ID          string `json:"id"`
		IsRootCause bool   `json:"is_root_cause"`
		Info        struct {
			Questions []agentChatControlQuestion `json:"questions"`
		} `json:"info"`
	}
	var raw struct {
		RequestID         string                     `json:"request_id"`
		Type              string                     `json:"type"`
		Title             string                     `json:"title"`
		Prompt            string                     `json:"prompt"`
		PromptPreview     string                     `json:"prompt_preview"`
		InputType         string                     `json:"input_type"`
		Required          bool                       `json:"required"`
		ControlTicket     string                     `json:"control_ticket"`
		Questions         []agentChatControlQuestion `json:"questions"`
		InterruptContexts []rawInterrupt             `json:"interrupt_contexts"`
	}
	payload := event.PayloadPreview
	if len(payload) == 0 {
		payload = event.Payload
	}
	if len(payload) == 0 || json.Unmarshal(payload, &raw) != nil {
		return nil, false
	}
	raw.RequestID = boundedAgentChatText(raw.RequestID, 256)
	raw.ControlTicket = boundedAgentChatText(raw.ControlTicket, 8192)
	if raw.RequestID == "" || raw.ControlTicket == "" {
		return nil, false
	}
	questions := append([]agentChatControlQuestion(nil), raw.Questions...)
	resumeTargets := make([]string, 0, len(raw.InterruptContexts))
	for _, interrupt := range raw.InterruptContexts {
		questions = append(questions, interrupt.Info.Questions...)
		if interrupt.IsRootCause && strings.TrimSpace(interrupt.ID) != "" {
			resumeTargets = append(resumeTargets, boundedAgentChatText(interrupt.ID, 512))
		}
	}
	if len(questions) > 8 {
		questions = questions[:8]
	}
	cleanQuestions := make([]agentChatControlQuestion, 0, len(questions))
	for _, question := range questions {
		question.Header = boundedAgentChatText(question.Header, 120)
		question.Question = boundedAgentChatText(question.Question, 2000)
		if question.Question == "" {
			continue
		}
		if len(question.Options) > 8 {
			question.Options = question.Options[:8]
		}
		cleanOptions := make([]agentChatControlOption, 0, len(question.Options))
		for _, option := range question.Options {
			option.Label = boundedAgentChatText(option.Label, 160)
			option.Description = boundedAgentChatText(option.Description, 500)
			if option.Label != "" {
				cleanOptions = append(cleanOptions, option)
			}
		}
		question.Options = cleanOptions
		cleanQuestions = append(cleanQuestions, question)
	}
	prompt := raw.Prompt
	if prompt == "" {
		prompt = raw.PromptPreview
	}
	view := map[string]any{
		"schemaVersion": agentChatControlSchemaVersion,
		"requestId":     raw.RequestID,
		"createdAt":     event.CreatedAt,
		"type":          boundedAgentChatText(raw.Type, 80),
		"title":         boundedAgentChatText(raw.Title, 240),
		"prompt":        boundedAgentChatText(prompt, 4000),
		"inputType":     boundedAgentChatText(raw.InputType, 80),
		"required":      raw.Required,
		"status":        "pending",
		"controlTicket": raw.ControlTicket,
		"questions":     cleanQuestions,
	}
	if len(resumeTargets) > 0 {
		// Target IDs are opaque runtime addresses, not credentials. A native Chat
		// client needs them to route the answer back to the interrupted component;
		// the signed control ticket still authorizes the response.
		view["resumeTargets"] = resumeTargets
	}
	return view, true
}

func boundedAgentChatText(value string, limit int) string {
	value = strings.TrimSpace(value)
	return boundedAgentChatFragment(value, limit)
}

func boundedAgentChatFragment(value string, limit int) string {
	if limit <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func agentChatCommentaryProjection(event observability.AgentEvent) (map[string]any, bool) {
	if event.Visibility != observability.VisibilityUserVisible {
		return nil, false
	}
	var preview map[string]any
	_ = json.Unmarshal(event.PayloadPreview, &preview)
	if len(preview) == 0 && event.EventType == observability.EventReasoningSummary {
		_ = json.Unmarshal(event.Payload, &preview)
	}
	text := ""
	truncated := false
	switch event.EventType {
	case observability.EventToolCallStarted:
		text = agentChatStringValue(preview["task_acknowledgement"])
		truncated, _ = preview["task_acknowledgement_truncated"].(bool)
	case observability.EventReasoningSummary:
		// Backward-compatible replay for an acknowledgement persisted by an
		// earlier adapter build. Other reasoning summaries remain hidden.
		if agentChatStringValue(preview["reasoning_scope"]) != agentChatReasoningScopeTaskAcknowledgement {
			return nil, false
		}
		text = agentChatStringValue(preview["text"])
		truncated, _ = preview["truncated"].(bool)
	default:
		return nil, false
	}
	text = boundedAgentChatText(text, 160)
	if text == "" {
		return nil, false
	}
	view := map[string]any{
		"schemaVersion": agentChatCommentarySchemaVersion,
		"eventId":       event.EventID,
		"runId":         event.RunID,
		"text":          text,
		"createdAt":     event.CreatedAt,
	}
	if truncated {
		view["truncated"] = true
	}
	return view, true
}

type agentChatProcessProjector struct {
	activeReasoningByRun map[string]string
	publicReasoningByRun map[string]string
}

func newAgentChatProcessProjector() *agentChatProcessProjector {
	return &agentChatProcessProjector{
		activeReasoningByRun: make(map[string]string),
		publicReasoningByRun: make(map[string]string),
	}
}

func agentChatProcessProjection(event observability.AgentEvent, debugEnabled bool) (map[string]any, bool) {
	return newAgentChatProcessProjector().Project(event, debugEnabled)
}

func (p *agentChatProcessProjector) Project(event observability.AgentEvent, debugEnabled bool) (map[string]any, bool) {
	category := agentChatProcessCategory(event.EventType)
	if agentChatReasoningBoundary(event.EventType) {
		p.BreakReasoning(event.RunID)
	}
	if category == "" {
		return nil, false
	}
	var preview map[string]any
	if category != "reasoning" || event.Visibility == observability.VisibilityUserVisible || agentChatModelLifecycleEvent(event.EventType) {
		_ = json.Unmarshal(event.PayloadPreview, &preview)
		if len(preview) == 0 && (event.EventType == observability.EventReasoningSummary || agentChatModelLifecycleEvent(event.EventType)) {
			_ = json.Unmarshal(event.Payload, &preview)
		}
	}
	if category == "reasoning" {
		// The workbench process stream is a user-facing projection, not a raw
		// model trace. Debug lifecycle/thought events remain available through
		// observability APIs but never become process rows, even in debug mode.
		if event.Visibility != observability.VisibilityUserVisible {
			return nil, false
		}
		// Agent-produced prose is rendered only through the dedicated commentary
		// projection. Reasoning summaries never become execution-process rows.
		if event.EventType == observability.EventReasoningSummary {
			return nil, false
		}
	}
	processID, reasoningActive := p.processID(event, category, preview)
	if event.Visibility != observability.VisibilityUserVisible && !debugEnabled {
		return nil, false
	}
	status := agentChatProcessStatus(event.EventType)
	if event.EventType == observability.EventReasoningSummary && reasoningActive {
		status = "running"
	}
	view := map[string]any{
		"schemaVersion": "harness.agent_chat_process.v1",
		"eventId":       event.EventID, "eventType": event.EventType, "category": category,
		"status": status, "createdAt": event.CreatedAt, "runId": event.RunID,
	}
	if event.Sequence > 0 {
		view["sequence"] = event.Sequence
	}
	view["processId"] = processID
	view["groupKey"] = string(category) + ":" + processID
	if status == "running" {
		view["startedAt"] = event.CreatedAt
	} else {
		view["endedAt"] = event.CreatedAt
	}
	if event.AgentID != "" {
		view["agentId"] = event.AgentID
	}
	if event.StepID != "" {
		view["stepId"] = event.StepID
	}
	if category == "tool" {
		presentation := agentChatToolProcessPresentation(preview)
		view["stage"] = map[string]any{
			"id": presentation.Stage.ID, "label": presentation.Stage.Label, "order": presentation.Stage.Order,
		}
		view["activity"] = map[string]any{
			"key": presentation.Activity.Key, "label": presentation.Activity.Label,
			"detailLevel": presentation.Activity.DetailLevel,
		}
	}
	// Reasoning bodies are model chain-of-thought: their raw text is surfaced ONLY
	// when the event is itself user-visible (a deliberately-published summary),
	// never for debug-visibility reasoning even to a debug client (privacy gate,
	// enforced by TestAgentChatProcessProjectionKeepsThinkingPublicAndBounded).
	// Tool arguments/results, however, are inspectable by a debug-authorized client
	// (the pull endpoint already exposes their payload/ref to such targets).
	toolBodyAllowed := event.Visibility == observability.VisibilityUserVisible || debugEnabled
	for _, key := range []string{"display_name", "tool_name", "skill_name", "skill_id", "sub_agent_ref", "runtime_agent_name", "title", "type", "name", "description"} {
		if value, ok := preview[key].(string); ok && value != "" {
			if category == "tool" && value == "ask_user" {
				return nil, false
			}
			view["label"] = value
			view["title"] = value
			break
		}
	}
	if category == "reasoning" {
		view["title"] = agentChatReasoningTitle(event.EventType)
	} else if _, ok := view["title"]; !ok {
		view["title"] = category
	}
	if category == "reasoning" && event.Visibility == observability.VisibilityUserVisible {
		if text, ok := preview["text"].(string); ok && text != "" {
			view["summary"] = boundedAgentChatFragment(text, 1000)
		}
	}
	if category == "tool" && toolBodyAllowed {
		if title, summary, details := agentChatResultPresentation(preview["presentation"]); title != "" || summary != "" || len(details) > 0 {
			if title != "" {
				view["title"] = title
				view["label"] = title
			}
			if summary != "" {
				view["summary"] = summary
			}
			if len(details) > 0 {
				view["details"] = details
			}
		}
	}
	// Tool input (arguments) and output (result) live inline for small payloads and
	// spill to an artifact ref otherwise; expose whichever is present so the client
	// can show it (fetching the artifact on demand). Gated by toolBodyAllowed.
	if category == "tool" && toolBodyAllowed {
		switch event.EventType {
		case observability.EventToolCallStarted, observability.EventToolCallProgress:
			if args, ok := preview["arguments_preview"]; ok && args != nil {
				view["input"] = args
			} else if event.DebugRef != "" {
				view["inputRef"] = event.DebugRef
			} else if event.PayloadRef != "" {
				view["inputRef"] = event.PayloadRef
			}
		case observability.EventToolArtifactCreated:
			if _, ok := preview["input_size_bytes"]; ok {
				if ref, _ := preview["artifact_ref"].(string); ref != "" {
					view["inputRef"] = ref
				} else if event.DebugRef != "" {
					view["inputRef"] = event.DebugRef
				} else if event.PayloadRef != "" {
					view["inputRef"] = event.PayloadRef
				}
			}
		case observability.EventToolCallCompleted, observability.EventToolCallFailed:
			if rp, ok := preview["result_preview"]; ok && rp != nil {
				if m, isMap := rp.(map[string]any); isMap {
					if ref, _ := m["artifact_ref"].(string); ref != "" {
						view["outputRef"] = ref
					} else {
						view["output"] = rp
					}
				} else {
					view["output"] = rp
				}
			} else if event.PayloadRef != "" {
				view["outputRef"] = event.PayloadRef
			}
		}
	}
	if event.Error != nil {
		view["errorCode"] = event.Error.Code
	}
	return view, true
}

func agentChatToolProcessPresentation(preview map[string]any) processpresentation.Metadata {
	var metadata processpresentation.Metadata
	if raw, ok := preview["process_presentation"]; ok {
		if data, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(data, &metadata)
		}
	}
	if processpresentation.ValidateStage(metadata.Stage) != nil || metadata.Stage.Empty() {
		metadata.Stage = processpresentation.DefaultStage()
	}
	if strings.TrimSpace(metadata.Activity.Key) == "" {
		metadata.Activity.Key = boundedAgentChatText(agentChatStringValue(preview["tool_name"]), 160)
	}
	if strings.TrimSpace(metadata.Activity.Key) == "" {
		metadata.Activity.Key = "tool"
	}
	if strings.TrimSpace(metadata.Activity.Label) == "" {
		metadata.Activity.Label = boundedAgentChatText(agentChatStringValue(preview["display_name"]), 160)
	}
	if strings.TrimSpace(metadata.Activity.Label) == "" {
		metadata.Activity.Label = "执行工具"
	}
	switch metadata.Activity.DetailLevel {
	case processpresentation.DetailLevelPrimary, processpresentation.DetailLevelSecondary, processpresentation.DetailLevelDebug:
	default:
		metadata.Activity.DetailLevel = processpresentation.DetailLevelSecondary
	}
	return metadata
}

func agentChatResultPresentation(value any) (string, string, []map[string]string) {
	presentation, ok := value.(map[string]any)
	if !ok {
		return "", "", nil
	}
	title := boundedAgentChatText(agentChatStringValue(presentation["title"]), 160)
	summary := boundedAgentChatText(agentChatStringValue(presentation["summary"]), 1000)
	rawDetails, _ := presentation["details"].([]any)
	details := make([]map[string]string, 0, min(len(rawDetails), 12))
	for _, raw := range rawDetails {
		if len(details) >= 12 {
			break
		}
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		label := boundedAgentChatText(agentChatStringValue(item["label"]), 80)
		value := boundedAgentChatText(agentChatStringValue(item["value"]), 1000)
		if label != "" && value != "" {
			details = append(details, map[string]string{"label": label, "value": value})
		}
	}
	return title, summary, details
}

func agentChatStringValue(value any) string {
	text, _ := value.(string)
	return text
}

func (p *agentChatProcessProjector) BreakReasoning(runID string) {
	delete(p.publicReasoningByRun, runID)
}

func (p *agentChatProcessProjector) processID(event observability.AgentEvent, category string, preview map[string]any) (string, bool) {
	if category != "reasoning" {
		delete(p.publicReasoningByRun, event.RunID)
		if category == "tool" {
			if toolCallID, _ := preview["tool_call_id"].(string); toolCallID != "" {
				return event.RunID + ":tool:" + toolCallID, false
			}
		}
		if event.StepID != "" {
			return event.RunID + ":" + event.StepID, false
		}
		return event.EventID, false
	}

	requestID, _ := preview["request_id"].(string)
	modelProcessID := ""
	if requestID != "" {
		modelProcessID = event.RunID + ":model:" + requestID
	}
	switch event.EventType {
	case observability.EventModelCallStarted:
		if modelProcessID == "" {
			modelProcessID = event.RunID + ":model:" + event.EventID
		}
		p.activeReasoningByRun[event.RunID] = modelProcessID
		delete(p.publicReasoningByRun, event.RunID)
		return modelProcessID, true
	case observability.EventReasoningSummary:
		if active := p.activeReasoningByRun[event.RunID]; active != "" {
			return active, true
		}
		if current := p.publicReasoningByRun[event.RunID]; current != "" {
			return current, false
		}
		processID := event.RunID + ":reasoning:" + event.EventID
		p.publicReasoningByRun[event.RunID] = processID
		return processID, false
	case observability.EventModelCallCompleted, observability.EventModelCallFailed:
		if modelProcessID == "" {
			modelProcessID = p.activeReasoningByRun[event.RunID]
		}
		if modelProcessID == "" {
			modelProcessID = event.RunID + ":model:" + event.EventID
		}
		delete(p.activeReasoningByRun, event.RunID)
		delete(p.publicReasoningByRun, event.RunID)
		return modelProcessID, false
	case observability.EventModelFallbackApplied:
		if active := p.activeReasoningByRun[event.RunID]; active != "" {
			return active, true
		}
	}
	return event.EventID, false
}

func agentChatModelLifecycleEvent(eventType observability.EventType) bool {
	switch eventType {
	case observability.EventModelCallStarted, observability.EventModelCallCompleted,
		observability.EventModelCallFailed, observability.EventModelFallbackApplied:
		return true
	default:
		return false
	}
}

func agentChatReasoningBoundary(eventType observability.EventType) bool {
	switch eventType {
	case observability.EventControlRequestCreated, observability.EventControlResponseReceived,
		observability.EventControlRequestExpired, observability.EventResumeAccepted,
		observability.EventResumeFailed:
		return true
	default:
		return false
	}
}

func agentChatReasoningTitle(eventType observability.EventType) string {
	switch eventType {
	case observability.EventModelCallCompleted:
		return "已完成思考"
	case observability.EventModelCallFailed:
		return "思考失败"
	case observability.EventModelFallbackApplied:
		return "已切换备用模型"
	case observability.EventReasoningSummary:
		return "思考摘要"
	default:
		return "正在思考"
	}
}

// agentChatRunProjection exposes a child Run as a stable process row. The
// projection intentionally excludes runtime bindings, model payloads and the
// child response; its public details are limited to identity, hierarchy and
// lifecycle timestamps.
func agentChatRunProjection(run *storage.Run) map[string]any {
	view := map[string]any{
		"schemaVersion": "harness.agent_chat_process.v1",
		"eventId":       "run:" + run.RunID,
		"processId":     "run:" + run.RunID,
		"groupKey":      "subagent:run:" + run.RunID,
		"eventType":     "sub_agent_run",
		"category":      "subagent",
		"title":         run.AgentID,
		"label":         run.AgentID,
		"status":        agentChatRunStatus(run.Status),
		"runId":         run.RunID,
		"parentRunId":   run.ParentRunID,
		"agentId":       run.AgentID,
	}
	if !run.StartedAt.IsZero() {
		view["startedAt"] = run.StartedAt
	}
	if !run.EndedAt.IsZero() {
		view["endedAt"] = run.EndedAt
	}
	return view
}

func decorateAgentChatProcess(view map[string]any, run *storage.Run) {
	if view == nil || run == nil {
		return
	}
	view["runId"] = run.RunID
	if run.ParentRunID != "" {
		view["parentRunId"] = run.ParentRunID
	}
	if _, exists := view["agentId"]; !exists && run.AgentID != "" {
		view["agentId"] = run.AgentID
	}
}

func agentChatRunStatus(status storage.RunStatus) string {
	switch status {
	case storage.RunStatusCompleted:
		return "completed"
	case storage.RunStatusFailed:
		return "failed"
	case storage.RunStatusCancelled:
		return "cancelled"
	case storage.RunStatusExpired:
		return "expired"
	default:
		return "running"
	}
}

func agentChatRunTerminal(status storage.RunStatus) bool {
	switch status {
	case storage.RunStatusCompleted, storage.RunStatusFailed,
		storage.RunStatusCancelled, storage.RunStatusExpired:
		return true
	default:
		return false
	}
}

func agentChatProcessCategory(eventType observability.EventType) string {
	switch eventType {
	case observability.EventReasoningSummary, observability.EventModelCallStarted, observability.EventModelCallCompleted,
		observability.EventModelCallFailed, observability.EventModelFallbackApplied:
		return "reasoning"
	case observability.EventToolCallStarted, observability.EventToolCallProgress, observability.EventToolCallCompleted,
		observability.EventToolCallFailed, observability.EventToolCallCancelled, observability.EventToolArtifactCreated:
		return "tool"
	case observability.EventSkillStarted, observability.EventSkillCompleted, observability.EventSkillFailed:
		return "skill"
	case observability.EventSubAgentStarted, observability.EventSubAgentProgress, observability.EventSubAgentCompleted,
		observability.EventSubAgentFailed, observability.EventA2ATaskCreated, observability.EventA2ATaskProgress,
		observability.EventA2ATaskCompleted, observability.EventA2ATaskFailed, observability.EventA2ATaskCancelled:
		return "subagent"
	default:
		return ""
	}
}

func agentChatProcessStatus(eventType observability.EventType) string {
	value := string(eventType)
	switch {
	case eventType == observability.EventToolArtifactCreated:
		return "running"
	case strings.HasSuffix(value, "_completed"), strings.HasSuffix(value, "_created"),
		eventType == observability.EventReasoningSummary, eventType == observability.EventModelFallbackApplied:
		return "completed"
	case strings.HasSuffix(value, "_failed"):
		return "failed"
	case strings.HasSuffix(value, "_cancelled"):
		return "cancelled"
	default:
		return "running"
	}
}
