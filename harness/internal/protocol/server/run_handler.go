package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

var errDispatchRecoveryRequired = errors.New("idempotent run remains created without a dispatch owner")

const (
	dispatchReplayGracePeriod  = time.Second
	dispatchReplayPollInterval = 50 * time.Millisecond
)

// createRunRequest is the POST body for opening a new turn/run.
//
// NOTE: config_snapshot_ref and agent_binding_id are intentionally NOT accepted
// from the client. They are internal, server-produced facts (Registry/Binding /
// Context Engine) and must never be forgeable by the caller. The corresponding
// OpenTurnRequest fields remain as internal ports for the server to populate once
// those chains are wired (handled at merge time).
type createRunRequest struct {
	AgentID            string `json:"agent_id,omitempty"`
	Channel            string `json:"channel,omitempty"`
	Runtime            string `json:"runtime,omitempty"`
	UserContentPreview string `json:"user_content_preview,omitempty"`
	UserContentRef     string `json:"user_content_ref,omitempty"`
}

type createRunResponse struct {
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id,omitempty"`
}

// handleCreateRun opens a turn via RunService.OpenTurn (fact-first, D5 snapshot
// front-loaded) and then dispatches the run for execution (§5.1). Tenant/user
// come from the TraceContext (never from a client-supplied tenant).
func (d *Deps) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	var body createRunRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
			writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
			return
		}
	}
	if d.RunService == nil {
		writeError(w, http.StatusInternalServerError, "RUN_SERVICE_UNAVAILABLE", "run service not configured")
		return
	}

	// Ownership: an existing session must belong to the caller; blocks same-tenant
	// injection of Message/Run into another user's session via a known session_id.
	if !d.authorizeSessionForOpen(w, r, sid) {
		return
	}

	// TraceContext already carries tenant/user (set by the auth/HTTP middleware);
	// bind the session so downstream stores scope correctly.
	tc := observability.MustTraceContext(r.Context())
	tc.SessionID = sid
	ctx := observability.WithTraceContext(r.Context(), tc)

	// 入口层预回合扩展管线（与 SDK Start 共享 kernel.TurnPipeline）：未配置
	// 时直通；失败 fail-closed，不得开 Turn。
	prepared, err := d.prepareTurnInput(ctx, &tc, body.AgentID, body.UserContentPreview)
	if err != nil {
		writeTurnPipelineError(w, err)
		return
	}
	sid = tc.SessionID
	ctx = observability.WithTraceContext(r.Context(), tc)

	res, err := d.RunService.OpenTurn(ctx, storage.OpenTurnRequest{
		SessionID:          sid,
		TenantID:           tc.TenantID,
		UserID:             tc.UserID,
		AgentID:            body.AgentID,
		Channel:            body.Channel,
		Runtime:            body.Runtime,
		UserContentPreview: prepared.Preview,
		UserContentRef:     body.UserContentRef,
		IdempotencyKey:     r.Header.Get("Idempotency-Key"),
		ContextFragments:   prepared.Fragments,
	})
	if err != nil {
		// OpenTurn is fact-first: a run record may exist even on snapshot failure.
		writeError(w, statusForStorageErr(err), "OPEN_TURN_FAILED", err.Error())
		return
	}

	// Hand off to execution. dispatcher integration is injected as a narrow port;
	// when nil (P0 skeleton) the fact-first ledger is already persisted and
	// dispatch is left as a wiring TODO at the composition root.
	if err := d.dispatchNewTurn(ctx, res); err != nil {
		writeDispatchError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, createRunResponse{
		RunID:     res.Run.RunID,
		SessionID: res.Session.ID,
		TurnID:    res.Turn.ID,
	})
}

func (d *Deps) dispatchNewTurn(ctx context.Context, turn *storage.OpenTurnResult) error {
	if d.Dispatcher == nil || turn == nil {
		return nil
	}
	if turn.Idempotent {
		return d.waitForExistingDispatch(ctx, turn)
	}
	if err := d.Dispatcher.Dispatch(ctx, turn); err != nil {
		terminal, terminalErr := d.RunService.FailUndispatchedRun(ctx, turn.Run, err)
		if terminalErr != nil {
			if d.Logger != nil {
				d.Logger.Error(ctx, "terminalize undispatched run failed", terminalErr,
					observability.String("run_id", turn.Run.RunID))
			}
			return err
		}
		if d.Broker != nil {
			if publishErr := d.Broker.Publish(ctx, terminal.Event); publishErr != nil && d.Logger != nil {
				d.Logger.Error(ctx, "publish undispatched run terminal failed", publishErr,
					observability.String("run_id", turn.Run.RunID))
			}
		}
		return err
	}
	return nil
}

// waitForExistingDispatch 处理 OpenTurn 已提交、原进程却可能在 Dispatch 前退出的窗口。
// 正常并发重放只等待首请求取得执行所有权；持续停在 created 时明确失败并告警，
// 不在协议层猜测性重派，也不引入后台扫描、租约或分布式事务。
func (d *Deps) waitForExistingDispatch(ctx context.Context, turn *storage.OpenTurnResult) error {
	if turn.Run == nil || turn.Run.RunID == "" || d.Stores.Runs == nil {
		return errors.New("idempotent dispatch replay has no queryable run")
	}
	if turn.Run.Status != storage.RunStatusCreated {
		return nil
	}
	timer := time.NewTimer(dispatchReplayGracePeriod)
	defer timer.Stop()
	ticker := time.NewTicker(dispatchReplayPollInterval)
	defer ticker.Stop()
	for {
		run, err := d.Stores.Runs.Get(ctx, turn.Run.RunID)
		if err != nil {
			return fmt.Errorf("inspect idempotent dispatch replay: %w", err)
		}
		if run.Status != storage.RunStatusCreated {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			continue
		case <-timer.C:
			latest, inspectErr := d.Stores.Runs.Get(ctx, turn.Run.RunID)
			if inspectErr != nil {
				return fmt.Errorf("inspect idempotent dispatch replay at deadline: %w", inspectErr)
			}
			if latest.Status != storage.RunStatusCreated {
				return nil
			}
			run = latest
			err := fmt.Errorf("%w: run_id=%s", errDispatchRecoveryRequired, run.RunID)
			if d.Logger != nil {
				d.Logger.Warn(ctx, "idempotent run requires manual dispatch recovery",
					observability.String("run_id", run.RunID), observability.String("session_id", run.SessionID))
			}
			return err
		}
	}
}

func writeDispatchError(w http.ResponseWriter, err error) {
	var authorizationRequired *mcp.AuthorizationRequiredError
	if errors.As(err, &authorizationRequired) {
		writeJSON(w, http.StatusPreconditionRequired, map[string]any{
			"error_code": "MCP_AUTHORIZATION_REQUIRED",
			"message":    "MCP OAuth authorization is required",
			"server_id":  authorizationRequired.ServerID,
			"authorization": map[string]any{
				"type":      "oauth2",
				"provider":  authorizationRequired.Provider,
				"scopes":    authorizationRequired.Scopes,
				"resource":  authorizationRequired.Resource,
				"start_url": "/api/v1/admin/mcp-servers/" + url.PathEscape(authorizationRequired.ServerID) + "/oauth/start",
			},
		})
		return
	}
	if errors.Is(err, errDispatchRecoveryRequired) {
		writeError(w, http.StatusServiceUnavailable, "DISPATCH_RECOVERY_REQUIRED", err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, "DISPATCH_FAILED", err.Error())
}
