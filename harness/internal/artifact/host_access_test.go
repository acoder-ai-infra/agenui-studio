package artifact

import (
	"context"
	"strings"
	"testing"
)

func hostActor() Actor {
	return Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", Role: ActorHost}
}

// TestHostActorIdentityRequirements 验证 ActorHost 的身份要求：session 与
// user 必填，RunID 不要求。
func TestHostActorIdentityRequirements(t *testing.T) {
	if _, err := requireActor(ContextWithActor(context.Background(), hostActor())); err != nil {
		t.Fatalf("canonical host actor rejected: %v", err)
	}
	missingSession := Actor{TenantID: "tenant-a", UserID: "user-1", Role: ActorHost}
	if _, err := requireActor(ContextWithActor(context.Background(), missingSession)); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("host without session must be denied, got %v", err)
	}
	missingUser := Actor{TenantID: "tenant-a", SessionID: "sess-1", Role: ActorHost}
	if _, err := requireActor(ContextWithActor(context.Background(), missingUser)); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("host without user must be denied, got %v", err)
	}
}

// TestHostPutScopeMatrix 验证宿主写入的作用域与写入面白名单。
func TestHostPutScopeMatrix(t *testing.T) {
	base := PutArtifactRequest{
		TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-x",
		ArtifactType: ArtifactTypeHostData, Visibility: VisibilityUserVisible,
	}
	tests := []struct {
		name    string
		mutate  func(*PutArtifactRequest)
		actor   Actor
		wantErr string
	}{
		{name: "canonical host put", actor: hostActor()},
		{name: "run id not compared", actor: hostActor(), mutate: func(r *PutArtifactRequest) { r.RunID = "run-other" }},
		{name: "session mismatch", actor: hostActor(), mutate: func(r *PutArtifactRequest) { r.SessionID = "sess-2" }, wantErr: "session mismatch"},
		{name: "user mismatch", actor: hostActor(), mutate: func(r *PutArtifactRequest) { r.UserID = "user-2" }, wantErr: "user mismatch"},
		{name: "tenant mismatch", actor: hostActor(), mutate: func(r *PutArtifactRequest) { r.TenantID = "tenant-b" }, wantErr: "tenant mismatch"},
		{name: "system type denied", actor: hostActor(), mutate: func(r *PutArtifactRequest) { r.ArtifactType = ArtifactTypeToolResult }, wantErr: "not writable by host"},
		{name: "restricted visibility denied", actor: hostActor(), mutate: func(r *PutArtifactRequest) { r.Visibility = VisibilityRestricted }, wantErr: "not writable by host"},
		{name: "internal visibility denied", actor: hostActor(), mutate: func(r *PutArtifactRequest) { r.Visibility = VisibilityInternal }, wantErr: "not writable by host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base
			if tt.mutate != nil {
				tt.mutate(&req)
			}
			err := validatePutScope(ContextWithActor(context.Background(), tt.actor), req)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validatePutScope() error = %v, want nil", err)
				}
				return
			}
			if !IsErrorCode(err, ErrPermissionDenied) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validatePutScope() error = %v, want permission_denied containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestHostRefScopeCrossRun 验证宿主可跨 Run（同 session）解析 ref，
// 跨 session / 跨 tenant 拒绝。
func TestHostRefScopeCrossRun(t *testing.T) {
	actor := hostActor()
	if err := validateRefPartsScope(actor, RefParts{TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-999", ArtifactID: "art"}); err != nil {
		t.Fatalf("same-session cross-run must pass: %v", err)
	}
	if err := validateRefPartsScope(actor, RefParts{TenantID: "tenant-a", SessionID: "sess-2", RunID: "run-1", ArtifactID: "art"}); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("cross-session must be denied, got %v", err)
	}
	if err := validateRefPartsScope(actor, RefParts{TenantID: "tenant-b", SessionID: "sess-1", RunID: "run-1", ArtifactID: "art"}); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("cross-tenant must be denied, got %v", err)
	}
}

// TestHostAuthorizeMatrix 验证宿主读取授权：系统类型黑名单、visibility 与
// purpose 约束。
func TestHostAuthorizeMatrix(t *testing.T) {
	meta := func(mutate func(*ArtifactMeta)) ArtifactMeta {
		m := ArtifactMeta{
			TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-7",
			ArtifactType: ArtifactTypeToolResult, Visibility: VisibilityInternal,
		}
		if mutate != nil {
			mutate(&m)
		}
		return m
	}
	ctx := ContextWithActor(context.Background(), hostActor())

	// 大结果 PayloadRef 场景：internal tool_result 跨 Run 可读。
	if err := authorize(ctx, meta(nil), PurposeHostAccess); err != nil {
		t.Fatalf("internal tool_result must be readable by host: %v", err)
	}
	// 系统类型黑名单。
	for _, at := range []ArtifactType{ArtifactTypeCheckpointState, ArtifactTypeContextSnapshot, ArtifactTypeControlResponse} {
		if err := authorize(ctx, meta(func(m *ArtifactMeta) { m.ArtifactType = at }), PurposeHostAccess); !IsErrorCode(err, ErrPermissionDenied) {
			t.Fatalf("system type %s must be denied for host, got %v", at, err)
		}
	}
	// restricted visibility 拒绝。
	if err := authorize(ctx, meta(func(m *ArtifactMeta) { m.Visibility = VisibilityRestricted }), PurposeHostAccess); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("restricted visibility must be denied for host, got %v", err)
	}
	// user 不匹配拒绝。
	if err := authorize(ctx, meta(func(m *ArtifactMeta) { m.UserID = "user-2" }), PurposeHostAccess); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("user mismatch must be denied for host, got %v", err)
	}
	// 非宿主 purpose 拒绝。
	if err := authorize(ctx, meta(nil), PurposeReplay); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("replay purpose must be denied for host, got %v", err)
	}
	// host_access purpose 对其他角色无泄漏：user 角色沿用既有规则不受影响。
	userCtx := ContextWithActor(context.Background(), Actor{
		TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-7", Role: ActorUser,
	})
	if err := authorize(userCtx, meta(func(m *ArtifactMeta) { m.Visibility = VisibilityUserVisible }), PurposeView); err != nil {
		t.Fatalf("existing user authorization regressed: %v", err)
	}
}
