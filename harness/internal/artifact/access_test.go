package artifact

import (
	"context"
	"testing"
)

func TestRequireActorRejectsMissingAndIncompleteActors(t *testing.T) {
	tests := []struct {
		name  string
		actor *Actor
	}{
		{name: "missing"},
		{name: "empty tenant", actor: &Actor{Role: ActorDebug}},
		{name: "empty role", actor: &Actor{TenantID: "tenant-a"}},
		{name: "unknown role", actor: &Actor{TenantID: "tenant-a", Role: ActorRole("root")}},
		{name: "user missing user", actor: &Actor{TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-1", Role: ActorUser}},
		{name: "user missing session", actor: &Actor{TenantID: "tenant-a", UserID: "user-1", RunID: "run-1", Role: ActorUser}},
		{name: "user missing run", actor: &Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", Role: ActorUser}},
		{name: "runtime missing session", actor: &Actor{TenantID: "tenant-a", RunID: "run-1", Role: ActorRuntime}},
		{name: "runtime missing run", actor: &Actor{TenantID: "tenant-a", SessionID: "sess-1", Role: ActorRuntime}},
		{name: "context engine missing user", actor: &Actor{TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-1", Role: ActorContextEngine}},
		{name: "context engine missing session", actor: &Actor{TenantID: "tenant-a", UserID: "user-1", RunID: "run-1", Role: ActorContextEngine}},
		{name: "context engine missing run", actor: &Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", Role: ActorContextEngine}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.actor != nil {
				ctx = ContextWithActor(ctx, *tt.actor)
			}
			if _, err := requireActor(ctx); !IsErrorCode(err, ErrPermissionDenied) {
				t.Fatalf("requireActor() error = %v, want permission_denied", err)
			}
		})
	}
}

func TestRequireActorAcceptsCanonicalActors(t *testing.T) {
	actors := []Actor{
		{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-1", Role: ActorUser},
		{TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-1", Role: ActorRuntime},
		{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-1", Role: ActorContextEngine},
		{TenantID: "tenant-a", Role: ActorDebug},
		{TenantID: "tenant-a", Role: ActorAudit},
	}
	for _, actor := range actors {
		t.Run(string(actor.Role), func(t *testing.T) {
			got, err := requireActor(ContextWithActor(context.Background(), actor))
			if err != nil {
				t.Fatalf("requireActor() error = %v", err)
			}
			if got != actor {
				t.Fatalf("requireActor() = %#v, want %#v", got, actor)
			}
		})
	}
}

func TestValidatePutScopeEnforcesCanonicalActorScope(t *testing.T) {
	baseRequest := PutArtifactRequest{
		TenantID:  "tenant-a",
		UserID:    "user-1",
		SessionID: "sess-1",
		RunID:     "run-1",
	}

	tests := []struct {
		name    string
		actor   Actor
		mutate  func(*PutArtifactRequest)
		allowed bool
	}{
		{
			name:    "user exact scope",
			actor:   Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-1", Role: ActorUser},
			allowed: true,
		},
		{
			name:   "user tenant mismatch",
			actor:  Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-1", Role: ActorUser},
			mutate: func(req *PutArtifactRequest) { req.TenantID = "tenant-b" },
		},
		{
			name:   "user session mismatch",
			actor:  Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-1", Role: ActorUser},
			mutate: func(req *PutArtifactRequest) { req.SessionID = "sess-2" },
		},
		{
			name:   "user run mismatch",
			actor:  Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-1", Role: ActorUser},
			mutate: func(req *PutArtifactRequest) { req.RunID = "run-2" },
		},
		{
			name:   "user id mismatch",
			actor:  Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-1", Role: ActorUser},
			mutate: func(req *PutArtifactRequest) { req.UserID = "user-2" },
		},
		{
			name:    "runtime exact scope",
			actor:   Actor{TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-1", Role: ActorRuntime},
			allowed: true,
		},
		{
			name:    "context engine exact scope",
			actor:   Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-1", Role: ActorContextEngine},
			allowed: true,
		},
		{
			name:   "context engine cannot write another run",
			actor:  Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-1", Role: ActorContextEngine},
			mutate: func(req *PutArtifactRequest) { req.RunID = "run-2" },
		},
		{
			name:   "context engine cannot write another user",
			actor:  Actor{TenantID: "tenant-a", UserID: "user-1", SessionID: "sess-1", RunID: "run-1", Role: ActorContextEngine},
			mutate: func(req *PutArtifactRequest) { req.UserID = "user-2" },
		},
		{
			name:   "runtime tenant mismatch",
			actor:  Actor{TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-1", Role: ActorRuntime},
			mutate: func(req *PutArtifactRequest) { req.TenantID = "tenant-b" },
		},
		{
			name:   "runtime session mismatch",
			actor:  Actor{TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-1", Role: ActorRuntime},
			mutate: func(req *PutArtifactRequest) { req.SessionID = "sess-2" },
		},
		{
			name:   "runtime run mismatch",
			actor:  Actor{TenantID: "tenant-a", SessionID: "sess-1", RunID: "run-1", Role: ActorRuntime},
			mutate: func(req *PutArtifactRequest) { req.RunID = "run-2" },
		},
		{
			name:    "tenant only debug",
			actor:   Actor{TenantID: "tenant-a", Role: ActorDebug},
			allowed: true,
		},
		{
			name:    "tenant only audit",
			actor:   Actor{TenantID: "tenant-a", Role: ActorAudit},
			allowed: true,
		},
		{
			name:  "debug tenant mismatch",
			actor: Actor{TenantID: "tenant-b", Role: ActorDebug},
		},
		{
			name:  "debug narrowed session mismatch",
			actor: Actor{TenantID: "tenant-a", SessionID: "sess-2", Role: ActorDebug},
		},
		{
			name:  "audit narrowed run mismatch",
			actor: Actor{TenantID: "tenant-a", RunID: "run-2", Role: ActorAudit},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := baseRequest
			if tt.mutate != nil {
				tt.mutate(&req)
			}
			err := validatePutScope(ContextWithActor(context.Background(), tt.actor), req)
			if tt.allowed {
				if err != nil {
					t.Fatalf("validatePutScope() error = %v", err)
				}
				return
			}
			if !IsErrorCode(err, ErrPermissionDenied) {
				t.Fatalf("validatePutScope() error = %v, want permission_denied", err)
			}
		})
	}
}
