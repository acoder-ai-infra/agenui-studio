package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/control"
	"github.com/AGenUI/agenui-studio/harness/internal/control/controlticket"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

const defaultControlRequestTTL = 7 * 24 * time.Hour

type runtimeControlCreator struct {
	service *control.Service
	tickets *controlticket.Codec
}

func (c runtimeControlCreator) CreateControlRequest(ctx context.Context, req agentruntime.ControlRequestCreateRequest) (observability.AgentEvent, error) {
	if c.service == nil {
		return observability.AgentEvent{}, agentruntime.ErrControlRequestCreatorMissing
	}
	if c.tickets == nil {
		return observability.AgentEvent{}, errors.New("control ticket codec is required")
	}
	if req.RequestID == "" {
		return observability.AgentEvent{}, agentruntime.ErrControlRequestIDMissing
	}
	expiresAt := time.Now().Add(defaultControlRequestTTL)
	ticket, err := c.tickets.Seal(controlticket.Claims{
		TenantID: req.TenantID, UserID: req.UserID, SessionID: req.SessionID,
		RunID: req.RunID, RequestID: req.RequestID, ResumeToken: req.ResumeToken,
	}, expiresAt)
	if err != nil {
		return observability.AgentEvent{}, err
	}
	event, err := withControlTicket(req.Event, ticket)
	if err != nil {
		return observability.AgentEvent{}, err
	}
	_, err = c.service.Create(ctx, control.CreateRequest{
		RequestID:     req.RequestID,
		RunID:         req.RunID,
		Type:          req.Type,
		CheckpointID:  req.CheckpointID,
		PromptPreview: req.PromptPreview,
		ResumeToken:   req.ResumeToken,
		TTL:           defaultControlRequestTTL,
		Event:         event,
	})
	if err != nil {
		return observability.AgentEvent{}, err
	}
	return event, nil
}

func withControlTicket(event observability.AgentEvent, ticket string) (observability.AgentEvent, error) {
	payload := map[string]any{}
	if len(event.Payload) > 0 {
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return observability.AgentEvent{}, err
		}
	}
	delete(payload, "resume_token")
	payload["control_ticket"] = ticket
	encoded, err := json.Marshal(payload)
	if err != nil {
		return observability.AgentEvent{}, err
	}
	event.Payload = encoded
	// Protocol adapters intentionally project PayloadPreview only. A control
	// request's sanitized payload is its user-visible protocol body, so publish
	// the exact same body as the preview rather than creating a debug-only path.
	event.PayloadPreview = append(json.RawMessage(nil), encoded...)
	return event, nil
}
