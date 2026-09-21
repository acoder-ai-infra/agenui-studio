package artifact

import (
	"context"
	"testing"
)

func TestAuthorizeDeniesUserReadingInternalArtifactForModelContext(t *testing.T) {
	ctx := ContextWithActor(context.Background(), Actor{
		TenantID:  "tenant-a",
		UserID:    "user-1",
		SessionID: "sess-1",
		RunID:     "run-1",
		Role:      ActorUser,
	})
	meta := ArtifactMeta{
		TenantID:   "tenant-a",
		SessionID:  "sess-1",
		RunID:      "run-1",
		Visibility: VisibilityInternal,
	}

	err := authorize(ctx, meta, PurposeModelContext)
	if !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("expected user to be denied for internal artifact even with model_context purpose, got %v", err)
	}
}

func TestAuthorizeDebugArtifactPurposeMatrix(t *testing.T) {
	meta := ArtifactMeta{
		TenantID:   "tenant-a",
		SessionID:  "sess-1",
		RunID:      "run-1",
		Visibility: VisibilityDebug,
	}
	runtimeCtx := ContextWithActor(context.Background(), Actor{
		TenantID:  "tenant-a",
		UserID:    "runtime",
		SessionID: "sess-1",
		RunID:     "run-1",
		Role:      ActorRuntime,
	})
	debugCtx := ContextWithActor(context.Background(), Actor{
		TenantID:  "tenant-a",
		UserID:    "debugger",
		SessionID: "sess-1",
		RunID:     "run-1",
		Role:      ActorDebug,
	})
	auditCtx := ContextWithActor(context.Background(), Actor{
		TenantID:  "tenant-a",
		UserID:    "auditor",
		SessionID: "sess-1",
		RunID:     "run-1",
		Role:      ActorAudit,
	})

	if err := authorize(runtimeCtx, meta, PurposeDebug); err != nil {
		t.Fatalf("runtime should read debug artifact for debug purpose: %v", err)
	}
	if err := authorize(runtimeCtx, meta, PurposeModelContext); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("runtime should not read debug artifact for model context, got %v", err)
	}
	if err := authorize(runtimeCtx, meta, PurposeReplay); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("runtime should not read debug artifact for replay, got %v", err)
	}
	if err := authorize(debugCtx, meta, PurposeDebug); err != nil {
		t.Fatalf("debug actor should read debug artifact: %v", err)
	}
	if err := authorize(auditCtx, meta, PurposeReplay); err != nil {
		t.Fatalf("audit actor should read debug artifact: %v", err)
	}
}

func TestAuthorizeEnforcesActorScope(t *testing.T) {
	meta := ArtifactMeta{
		TenantID:   "tenant-a",
		UserID:     "user-1",
		SessionID:  "sess-1",
		RunID:      "run-1",
		Visibility: VisibilityUserVisible,
	}
	tests := []struct {
		name    string
		actor   Actor
		allowed bool
	}{
		{
			name:    "matching user",
			actor:   Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-1", Role: ActorUser},
			allowed: true,
		},
		{name: "wrong tenant", actor: Actor{TenantID: "tenant-b", Role: ActorAudit}},
		{name: "wrong user", actor: Actor{TenantID: "tenant-a", UserID: "user-2", SessionID: "sess-1", RunID: "run-1", Role: ActorUser}},
		{name: "user wrong session", actor: Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-2", RunID: "run-1", Role: ActorUser}},
		{name: "user wrong run", actor: Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-2", Role: ActorUser}},
		{name: "runtime wrong session", actor: Actor{TenantID: "tenant-a", SessionID: "sess-2", RunID: "run-1", Role: ActorRuntime}},
		{name: "runtime wrong run", actor: Actor{TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-2", Role: ActorRuntime}},
		{name: "context engine same session previous run", actor: Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-2", Role: ActorContextEngine}, allowed: true},
		{name: "context engine wrong user", actor: Actor{TenantID: "tenant-a", UserID: "user-2", SessionID: "sess-1", RunID: "run-2", Role: ActorContextEngine}},
		{name: "context engine wrong session", actor: Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-2", RunID: "run-2", Role: ActorContextEngine}},
		{name: "narrowed debug wrong session", actor: Actor{TenantID: "tenant-a", SessionID: "sess-2", Role: ActorDebug}},
		{name: "narrowed audit wrong run", actor: Actor{TenantID: "tenant-a", RunID: "run-2", Role: ActorAudit}},
		{name: "tenant only debug", actor: Actor{TenantID: "tenant-a", Role: ActorDebug}, allowed: true},
		{name: "tenant only audit", actor: Actor{TenantID: "tenant-a", Role: ActorAudit}, allowed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			purpose := PurposeView
			if tt.actor.Role == ActorContextEngine {
				purpose = PurposeModelContext
			}
			err := authorize(ContextWithActor(context.Background(), tt.actor), meta, purpose)
			if tt.allowed {
				if err != nil {
					t.Fatalf("authorize() error = %v", err)
				}
				return
			}
			if !IsErrorCode(err, ErrPermissionDenied) {
				t.Fatalf("authorize() error = %v, want permission_denied", err)
			}
		})
	}
}

func TestAuthorizeContextEngineRejectsArtifactWithoutUserOwnership(t *testing.T) {
	meta := ArtifactMeta{TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-1", Visibility: VisibilityInternal}
	actor := Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-2", Role: ActorContextEngine}
	if err := authorize(ContextWithActor(context.Background(), actor), meta, PurposeModelContext); !IsErrorCode(err, ErrPermissionDenied) {
		t.Fatalf("authorize() error = %v, want permission_denied", err)
	}
}

func TestAuthorizeP0VisibilityPurposeMatrix(t *testing.T) {
	roles := []ActorRole{ActorUser, ActorRuntime, ActorContextEngine, ActorDebug, ActorAudit}
	visibilities := []Visibility{VisibilityUserVisible, VisibilityInternal, VisibilityDebug, VisibilityRestricted}
	purposes := []Purpose{PurposeView, PurposeDownload, PurposeModelContext, PurposeDebug, PurposeReplay}

	for _, role := range roles {
		for _, visibility := range visibilities {
			for _, purpose := range purposes {
				name := string(role) + "/" + string(visibility) + "/" + string(purpose)
				t.Run(name, func(t *testing.T) {
					meta := ArtifactMeta{
						TenantID:   "tenant-a",
						UserID:     "user-1",
						SessionID:  "sess-1",
						RunID:      "run-1",
						Visibility: visibility,
					}
					actor := Actor{TenantID: "tenant-a", Role: role}
					if role == ActorUser || role == ActorContextEngine {
						actor.UserID = "user-1"
					}
					if role == ActorUser || role == ActorRuntime || role == ActorContextEngine {
						actor.SessionID = "sess-1"
						actor.RunID = "run-1"
					}

					err := authorize(ContextWithActor(context.Background(), actor), meta, purpose)
					allowed := p0VisibilityAllowed(role, visibility, purpose)
					if allowed && err != nil {
						t.Fatalf("authorize() error = %v, want allowed", err)
					}
					if !allowed && !IsErrorCode(err, ErrPermissionDenied) {
						t.Fatalf("authorize() error = %v, want permission_denied", err)
					}
				})
			}
		}
	}
}

func TestAuthorizeRejectsUnknownPurposeBeforeActorValidation(t *testing.T) {
	meta := ArtifactMeta{
		TenantID:   "tenant-a",
		UserID:     "user-1",
		SessionID:  "sess-1",
		RunID:      "run-1",
		Visibility: VisibilityUserVisible,
	}
	for _, purpose := range []Purpose{"", Purpose("private")} {
		if err := authorize(context.Background(), meta, purpose); !IsErrorCode(err, ErrInvalidArgument) {
			t.Fatalf("authorize() purpose %q error = %v, want invalid_argument", purpose, err)
		}
	}
}

func TestAuthorizeRejectsUnknownVisibility(t *testing.T) {
	ctx := ContextWithActor(context.Background(), Actor{TenantID: "tenant-a", Role: ActorAudit})
	for _, visibility := range []Visibility{"", Visibility("private")} {
		meta := ArtifactMeta{
			TenantID:   "tenant-a",
			SessionID:  "sess-1",
			RunID:      "run-1",
			Visibility: visibility,
		}
		if err := authorize(ctx, meta, PurposeView); !IsErrorCode(err, ErrInvalidArgument) {
			t.Fatalf("authorize() visibility %q error = %v, want invalid_argument", visibility, err)
		}
	}
}

func p0VisibilityAllowed(role ActorRole, visibility Visibility, purpose Purpose) bool {
	if role == ActorContextEngine && purpose != PurposeModelContext {
		return false
	}
	switch visibility {
	case VisibilityUserVisible:
		return true
	case VisibilityInternal:
		return role == ActorRuntime || role == ActorContextEngine || role == ActorDebug || role == ActorAudit
	case VisibilityDebug:
		return role == ActorDebug || role == ActorAudit || role == ActorRuntime && purpose == PurposeDebug
	case VisibilityRestricted:
		return role == ActorDebug || role == ActorAudit
	default:
		return false
	}
}
