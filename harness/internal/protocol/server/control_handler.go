package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/control"
	"github.com/AGenUI/agenui-studio/harness/internal/control/controlticket"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

const maxInlineControlResponseBytes = 256 << 10

// controlResponseRequest is the POST body for answering a control request (§5.4).
type controlResponseRequest struct {
	ClientEventID string `json:"client_event_id,omitempty"`
	ControlTicket string `json:"control_ticket,omitempty"`
	// ResumeToken is retained only for isolated protocol tests that do not install
	// a ticket codec. The formal Composition Root always requires ControlTicket.
	ResumeToken  string          `json:"resume_token,omitempty"`
	Decision     string          `json:"decision,omitempty"`
	Value        json.RawMessage `json:"value,omitempty"`
	Targets      map[string]any  `json:"targets,omitempty"`
	ResponseText string          `json:"response_text,omitempty"`
	// ResponseRef references a pre-written response artifact for large bodies.
	ResponseRef string `json:"response_ref,omitempty"`
}

type controlResponseResult struct {
	RequestID string `json:"request_id"`
	Status    string `json:"status"`
}

// handleControlResponse submits a control response to control.Service.Answer.
// Idempotency / CAS / exactly-once Resume are enforced inside Answer (D2, C-001);
// duplicate submissions return the same answered request.
func (d *Deps) handleControlResponse(w http.ResponseWriter, r *http.Request) {
	rid := r.PathValue("rid")
	if rid == "" {
		writeError(w, http.StatusBadRequest, "MISSING_REQUEST_ID", "control request id required")
		return
	}
	if d.Control == nil {
		writeError(w, http.StatusInternalServerError, "CONTROL_UNAVAILABLE", "control service not configured")
		return
	}
	var body controlResponseRequest
	if r.Body != nil {
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxInlineControlResponseBytes))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
			return
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			if err == nil {
				err = errors.New("request body must contain exactly one JSON object")
			}
			writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
			return
		}
	}
	existing, err := d.Stores.Controls.Get(r.Context(), rid)
	if err != nil {
		writeError(w, statusForStorageErr(err), "CONTROL_GET_FAILED", err.Error())
		return
	}
	run, ok := d.authorizeRun(w, r, existing.RunID)
	if !ok {
		return
	}
	resumeToken, err := d.resolveControlResumeToken(r, run, rid, body)
	if err != nil {
		writeError(w, http.StatusForbidden, "CONTROL_TICKET_INVALID", "control ticket is invalid or expired")
		return
	}
	responseRef := existing.ResponseRef
	if existing.Status == string(control.StatusPending) {
		responseRef, err = d.persistControlResponse(r, run, rid, body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "CONTROL_RESPONSE_INVALID", err.Error())
			return
		}
	}

	cr, err := d.Control.Answer(r.Context(), control.AnswerRequest{
		RequestID:   rid,
		ResumeToken: resumeToken,
		ResponseRef: responseRef,
	})
	if err != nil {
		writeError(w, statusForStorageErr(err), "CONTROL_ANSWER_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, controlResponseResult{RequestID: cr.RequestID, Status: cr.Status})
}

func (d *Deps) resolveControlResumeToken(r *http.Request, run *storage.Run, requestID string, body controlResponseRequest) (string, error) {
	if d.ControlTickets == nil {
		return body.ResumeToken, nil
	}
	claims, expiresAt, err := d.ControlTickets.Open(body.ControlTicket)
	if err != nil || !time.Now().Before(expiresAt) {
		return "", controlticket.ErrInvalidTicket
	}
	tc := observability.MustTraceContext(r.Context())
	if claims.TenantID != run.TenantID || claims.UserID != tc.UserID ||
		claims.SessionID != run.SessionID || claims.RunID != run.RunID || claims.RequestID != requestID {
		return "", controlticket.ErrInvalidTicket
	}
	return claims.ResumeToken, nil
}

func (d *Deps) persistControlResponse(r *http.Request, run *storage.Run, requestID string, body controlResponseRequest) (string, error) {
	if body.ResponseRef != "" {
		if body.Decision != "" || len(body.Value) > 0 || len(body.Targets) > 0 || strings.TrimSpace(body.ResponseText) != "" {
			return "", errors.New("response_ref cannot be combined with inline decision, value, targets, or response_text")
		}
		if d.Artifacts == nil {
			return "", errors.New("artifact store is required for response_ref")
		}
		ctx := controlResponseArtifactContext(r.Context(), run)
		object, err := d.Artifacts.Get(ctx, body.ResponseRef, artifact.GetOptions{Purpose: artifact.PurposeReplay})
		if err != nil {
			return "", err
		}
		if closeErr := object.Content.Close(); closeErr != nil {
			return "", closeErr
		}
		if object.Meta.ArtifactType != artifact.ArtifactTypeControlResponse {
			return "", errors.New("response_ref must reference a control_response artifact")
		}
		return body.ResponseRef, nil
	}
	responseText := strings.TrimSpace(body.ResponseText)
	if body.Decision == "" && len(body.Value) == 0 && len(body.Targets) == 0 && responseText == "" {
		return "", nil
	}
	if d.Artifacts == nil {
		return "", errors.New("artifact store is required for inline control response")
	}
	payload, err := json.Marshal(struct {
		Decision string          `json:"decision,omitempty"`
		Value    json.RawMessage `json:"value,omitempty"`
		Targets  map[string]any  `json:"targets,omitempty"`
		Text     string          `json:"response_text,omitempty"`
	}{Decision: body.Decision, Value: body.Value, Targets: body.Targets, Text: responseText})
	if err != nil {
		return "", fmt.Errorf("marshal control response: %w", err)
	}
	ctx := controlResponseArtifactContext(r.Context(), run)
	meta, err := d.Artifacts.Put(ctx, artifact.PutArtifactRequest{
		TenantID: run.TenantID, SessionID: run.SessionID, RunID: run.RunID,
		OwnerModule: artifact.OwnerModuleProtocol, OwnerID: requestID,
		ArtifactType: artifact.ArtifactTypeControlResponse, MimeType: "application/json",
		Visibility: artifact.VisibilityInternal, Content: bytes.NewReader(payload),
		RetentionPolicy: artifact.RetentionRunTTL, CreatedBy: "control_response",
		IdempotencyKey: fmt.Sprintf("control-response:%s:%s", requestID, body.ClientEventID),
		Metadata:       map[string]string{"request_id": requestID, "schema_version": "harness.control_response.v1"},
	})
	if err != nil {
		return "", err
	}
	return meta.ArtifactRef, nil
}

func controlResponseArtifactContext(ctx context.Context, run *storage.Run) context.Context {
	return artifact.ContextWithActor(ctx, artifact.Actor{
		TenantID: run.TenantID, SessionID: run.SessionID, RunID: run.RunID, Role: artifact.ActorRuntime,
	})
}
