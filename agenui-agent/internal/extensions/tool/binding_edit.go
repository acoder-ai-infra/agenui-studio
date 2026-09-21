package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	agenuiextensions "github.com/AGenUI/agenui-studio/agenui-agent/internal/extensions"
	stepartifact "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/artifact"
	harness "github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// PrepareBindingEdit turns a model semantic decision into a Host-validated
// route fact. It does not select fields, operators, or mappings; those choices
// remain with Binder and the versioned workspace tools.
type PrepareBindingEdit struct {
	sessions *designingSessions
	provider *DesigningProvider
}

func (*PrepareBindingEdit) Name() string { return PrepareBindingEditName }

func (t *PrepareBindingEdit) Invoke(ctx context.Context, call extension.FunctionCall) (*extension.FunctionResult, error) {
	if call.Ctx.AgentID != agenuiextensions.MainAgent || call.Name != PrepareBindingEditName {
		return nil, errors.New("prepare binding edit: caller is not allowed")
	}
	var input struct {
		Query string `json:"query"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(call.Arguments)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || strings.TrimSpace(input.Query) == "" {
		return nil, errors.New("prepare binding edit: query is required")
	}
	if t.provider == nil || t.provider.artifacts == nil {
		return nil, errors.New("prepare binding edit: durable persistence is unavailable")
	}
	key := sessionKey(call.Ctx.TenantID, call.Ctx.UserID, call.Ctx.SessionID)
	t.sessions.mu.RLock()
	design := t.sessions.designs[key]
	t.sessions.mu.RUnlock()
	if strings.TrimSpace(design) == "" {
		if err := t.provider.restoreSession(ctx, call.Ctx); err != nil {
			if errors.Is(err, stepartifact.ErrNotFound) {
				return nil, errors.New("prepare binding edit: no completed generation exists for this session")
			}
			return nil, err
		}
	}
	baseIdentity := harness.Identity{
		TenantID: call.Ctx.TenantID, UserID: call.Ctx.UserID,
		SessionID: call.Ctx.SessionID,
	}
	baseRunID, err := t.provider.artifacts.LatestRunID(
		ctx, baseIdentity, stepartifact.StepFinal,
	)
	if err != nil {
		if errors.Is(err, stepartifact.ErrNotFound) {
			return nil, errors.New("prepare binding edit: no completed generation exists for this session")
		}
		return nil, err
	}
	t.sessions.mu.Lock()
	t.sessions.bindingEdits[key] = bindingEditRequest{
		Query: strings.TrimSpace(input.Query), BaseRunID: baseRunID,
	}
	t.sessions.mu.Unlock()
	response, _ := json.Marshal(map[string]any{
		"status": "prepared",
		"route":  "binding_edit",
	})
	return &extension.FunctionResult{Data: response, MimeType: "application/json"}, nil
}
