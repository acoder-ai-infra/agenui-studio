package server

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact/downloadtoken"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// Download URL TTL policy. A client may request a shorter window; anything
// above maxDownloadURLTTL is truncated so a leaked bearer token cannot outlive
// the policy window, and a non-positive request falls back to the default. The
// cap matches the object-store hard limit (objectstore.maxDownloadTTL = 5m):
// requesting more is clamped here rather than rejected downstream.
const (
	defaultDownloadURLTTL = 5 * time.Minute
	maxDownloadURLTTL     = 5 * time.Minute
)

// handleArtifact streams an artifact by ref (§5.5). The bare artifact GET path
// carries no session in the URL, so the TraceContext has empty SessionID/RunID —
// building the actor from it would only enforce tenant, leaving same-tenant
// horizontal access open. Instead we parse the ref (which encodes
// tenant/session/run) and verify ownership against server-side facts (the run's
// owning session), then hand the artifact layer an actor scoped to the resolved
// session/run so its visibility×role×scope checks apply too.
func (d *Deps) handleArtifact(w http.ResponseWriter, r *http.Request) {
	if d.Artifacts == nil {
		writeError(w, http.StatusInternalServerError, "ARTIFACT_UNAVAILABLE", "artifact store not configured")
		return
	}
	ref := r.PathValue("ref")
	if q := r.URL.Query().Get("ref"); q != "" {
		ref = q
	}
	if ref == "" {
		writeError(w, http.StatusBadRequest, "MISSING_ARTIFACT_REF", "artifact ref required")
		return
	}

	actor, ok := d.authorizeArtifact(w, r, ref)
	if !ok {
		return
	}
	ctx := artifact.ContextWithActor(r.Context(), actor)

	obj, err := d.Artifacts.Get(ctx, ref, artifact.GetOptions{Purpose: artifact.PurposeView})
	if err != nil {
		writeError(w, statusForArtifactErr(err), "ARTIFACT_GET_FAILED", err.Error())
		return
	}
	defer obj.Content.Close()

	if obj.Meta.MimeType != "" {
		w.Header().Set("content-type", obj.Meta.MimeType)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, obj.Content)
}

// handleArtifactDownload redeems an encrypted download token minted by
// CreateDownloadURL and streams the artifact. The token is a bearer capability:
// it was issued only after the requester passed the full authorization chain,
// so redemption itself carries no session in the URL and re-runs no ownership
// checks — the sealed key and short expiry are the authorization. This is the
// same model as a presigned URL, but the platform proxies the bytes so no
// physical-storage location ever leaves the server.
func (d *Deps) handleArtifactDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if d.Artifacts == nil {
		d.logArtifactDownloadAudit(r, "redeem", "unavailable", "", time.Time{})
		writeError(w, http.StatusInternalServerError, "ARTIFACT_UNAVAILABLE", "artifact store not configured")
		return
	}
	token := r.PathValue("token")
	if token == "" || len(token) > downloadtoken.MaxEncodedTokenBytes {
		d.logArtifactDownloadAudit(r, "redeem", "denied", "", time.Time{})
		writeError(w, http.StatusBadRequest, "INVALID_DOWNLOAD_TOKEN", "valid download token required")
		return
	}
	obj, err := d.Artifacts.OpenDownload(r.Context(), token)
	if err != nil {
		d.logArtifactDownloadAudit(r, "redeem", "denied", "", time.Time{})
		// 兑换端是匿名 bearer 边界，不能把 ref、storage key 或后端错误回显。
		writeError(w, statusForArtifactErr(err), "ARTIFACT_DOWNLOAD_FAILED", "download token is invalid or unavailable")
		return
	}
	defer obj.Content.Close()

	if obj.Meta.MimeType != "" {
		w.Header().Set("content-type", obj.Meta.MimeType)
	}
	if disposition := mime.FormatMediaType("attachment", map[string]string{"filename": safeArtifactFilename(obj.Meta.Name, obj.Meta.ArtifactID)}); disposition != "" {
		w.Header().Set("Content-Disposition", disposition)
	} else {
		w.Header().Set("Content-Disposition", "attachment")
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, obj.Content); err != nil {
		d.logArtifactDownloadAuditForTenant(r, obj.Meta.TenantID, "redeem", "failed", obj.Meta.ArtifactRef, time.Time{})
		return
	}
	d.logArtifactDownloadAuditForTenant(r, obj.Meta.TenantID, "redeem", "succeeded", obj.Meta.ArtifactRef, time.Time{})
}

// handleCreateDownloadURL mints a short-lived, sealed download URL for an
// artifact the caller owns. It runs the same ownership resolution as the ref
// GET path (authorizeArtifact), then defers to the artifact layer, which
// re-checks visibility×role×scope for PurposeDownload before sealing a token.
// The returned URL is a bearer capability redeemable at
// GET /api/v1/artifacts/download/{token} without further authentication.
func (d *Deps) handleCreateDownloadURL(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if d.Artifacts == nil {
		d.logArtifactDownloadAudit(r, "issue", "unavailable", "", time.Time{})
		writeError(w, http.StatusInternalServerError, "ARTIFACT_UNAVAILABLE", "artifact store not configured")
		return
	}
	var body struct {
		Ref        string `json:"ref"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
		d.logArtifactDownloadAudit(r, "issue", "denied", "", time.Time{})
		writeError(w, http.StatusBadRequest, "MALFORMED_REQUEST", err.Error())
		return
	}
	ref := strings.TrimSpace(body.Ref)
	if ref == "" {
		d.logArtifactDownloadAudit(r, "issue", "denied", "", time.Time{})
		writeError(w, http.StatusBadRequest, "MISSING_ARTIFACT_REF", "artifact ref required")
		return
	}

	actor, ok := d.authorizeArtifact(w, r, ref)
	if !ok {
		d.logArtifactDownloadAudit(r, "issue", "denied", ref, time.Time{})
		return
	}
	ctx := artifact.ContextWithActor(r.Context(), actor)

	grant, err := d.Artifacts.CreateDownloadURL(ctx, ref, artifact.DownloadURLOptions{TTL: downloadURLTTL(body.TTLSeconds)})
	if err != nil {
		d.logArtifactDownloadAudit(r, "issue", "failed", ref, time.Time{})
		writeError(w, statusForArtifactErr(err), "ARTIFACT_DOWNLOAD_URL_FAILED", err.Error())
		return
	}
	grant.URL, err = canonicalArtifactDownloadPath(grant.URL)
	if err != nil {
		d.logArtifactDownloadAudit(r, "issue", "failed", ref, time.Time{})
		writeError(w, http.StatusInternalServerError, "ARTIFACT_DOWNLOAD_URL_FAILED", "artifact download URL is unavailable")
		return
	}
	d.logArtifactDownloadAudit(r, "issue", "succeeded", ref, grant.ExpiresAt)
	writeJSON(w, http.StatusOK, grant)
}

func canonicalArtifactDownloadPath(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid object-store download URL")
	}
	path := strings.TrimRight(parsed.Path, "/")
	separator := strings.LastIndex(path, "/")
	if separator < 0 || separator == len(path)-1 {
		return "", fmt.Errorf("object-store download token is missing")
	}
	token := path[separator+1:]
	if len(token) > downloadtoken.MaxEncodedTokenBytes || strings.ContainsAny(token, "/\\") {
		return "", fmt.Errorf("object-store download token is invalid")
	}
	return "/api/v1/artifacts/download/" + token, nil
}

func (d *Deps) logArtifactDownloadAudit(r *http.Request, action, result, ref string, expiresAt time.Time) {
	d.logArtifactDownloadAuditForTenant(r, "", action, result, ref, expiresAt)
}

func (d *Deps) logArtifactDownloadAuditForTenant(r *http.Request, tenantID, action, result, ref string, expiresAt time.Time) {
	if d.Logger == nil {
		return
	}
	ctx := r.Context()
	if tenantID != "" {
		// 兑换请求不带用户 JWT，租户只能在令牌通过 AEAD 校验并回查到当前
		// Artifact 元数据后确定。把可信租户补入审计上下文，既支持租户隔离
		// 查询，也不会相信匿名请求头。
		tc := observability.MustTraceContext(ctx)
		tc.TenantID = tenantID
		ctx = observability.WithTraceContext(ctx, tc)
	}
	fields := []observability.Field{
		observability.String("action", action),
		observability.String("result", result),
	}
	if ref != "" {
		fields = append(fields, observability.String("artifact_ref", ref))
	}
	if !expiresAt.IsZero() {
		fields = append(fields, observability.String("expires_at", expiresAt.UTC().Format(time.RFC3339)))
	}
	d.Logger.Info(ctx, "artifact download audit", fields...)
}

func safeArtifactFilename(name, artifactID string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	if index := strings.LastIndex(name, "/"); index >= 0 {
		name = name[index+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		name = strings.TrimSpace(artifactID)
	}
	if name == "" {
		name = "artifact"
	}
	runes := []rune(name)
	if len(runes) > 128 {
		name = string(runes[:128])
	}
	return name
}

// downloadURLTTL clamps a client-requested TTL (seconds) to the policy window.
func downloadURLTTL(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultDownloadURLTTL
	}
	ttl := time.Duration(seconds) * time.Second
	if ttl > maxDownloadURLTTL {
		return maxDownloadURLTTL
	}
	return ttl
}

// authorizeArtifact parses the ref and verifies the caller owns the artifact via
// server-side facts, returning the scoped actor to use for the fetch. It blocks
// same-tenant horizontal access: a user cannot read another user's artifact even
// within the same tenant.
func (d *Deps) authorizeArtifact(w http.ResponseWriter, r *http.Request, ref string) (artifact.Actor, bool) {
	tc := observability.MustTraceContext(r.Context())
	parts, err := artifact.ParseRef(ref)
	if err != nil {
		writeError(w, http.StatusBadRequest, "MALFORMED_ARTIFACT_REF", err.Error())
		return artifact.Actor{}, false
	}
	// Tenant isolation.
	if parts.TenantID != "" && tc.TenantID != "" && parts.TenantID != tc.TenantID {
		writeError(w, http.StatusForbidden, "ARTIFACT_TENANT_MISMATCH", "artifact belongs to another tenant")
		return artifact.Actor{}, false
	}
	// User-facing artifact reads require a complete, server-owned Run -> Session
	// ownership chain. System/sessionless artifacts are not implicitly readable by
	// ordinary users; they need a separate privileged endpoint and policy.
	sessionID := parts.SessionID
	if parts.RunID == "" || sessionID == "" || sessionID == "sessionless" {
		writeError(w, http.StatusForbidden, "ARTIFACT_OWNER_UNRESOLVED", "artifact owner cannot be resolved")
		return artifact.Actor{}, false
	}
	run, err := d.Stores.Runs.Get(r.Context(), parts.RunID)
	if err != nil || run.SessionID == "" || run.SessionID != sessionID {
		writeError(w, http.StatusForbidden, "ARTIFACT_OWNER_UNRESOLVED", "artifact run cannot be resolved")
		return artifact.Actor{}, false
	}
	if run.TenantID != "" && tc.TenantID != "" && run.TenantID != tc.TenantID {
		writeError(w, http.StatusForbidden, "ARTIFACT_TENANT_MISMATCH", "artifact run belongs to another tenant")
		return artifact.Actor{}, false
	}
	sess, err := d.Stores.Sessions.Get(r.Context(), sessionID)
	if err != nil || !sessionAllowed(r, sess) {
		writeError(w, http.StatusForbidden, "ARTIFACT_FORBIDDEN", "artifact does not belong to current user")
		return artifact.Actor{}, false
	}
	role := artifact.ActorUser
	if tc.DebugEnabled {
		role = artifact.ActorDebug
	}
	return artifact.Actor{
		TenantID:  tc.TenantID,
		UserID:    tc.UserID,
		SessionID: parts.SessionID,
		RunID:     parts.RunID,
		AgentID:   tc.AgentID,
		Role:      role,
	}, true
}

func statusForArtifactErr(err error) int {
	switch {
	case artifact.IsErrorCode(err, artifact.ErrNotFound):
		return http.StatusNotFound
	case artifact.IsErrorCode(err, artifact.ErrPermissionDenied):
		return http.StatusForbidden
	case artifact.IsErrorCode(err, artifact.ErrDeleted):
		return http.StatusGone
	case artifact.IsErrorCode(err, artifact.ErrInvalidArgument):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
