package artifact_test

import (
	"strings"
	"testing"

	. "github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

func TestDeterministicArtifactIDRequiresIdempotencyAndRejectsCollision(t *testing.T) {
	store := newTestStore(t)
	ctx := runtimeContext()
	base := PutArtifactRequest{
		ArtifactID: "artifact_final_1", TenantID: "tenant-a", UserID: "user-1",
		SessionID: "sess-1", RunID: "run-1", OwnerModule: OwnerModuleRuntime,
		OwnerID: "run-1", ArtifactType: ArtifactTypeFinalResult, MimeType: "text/plain",
		Visibility: VisibilityUserVisible, RetentionPolicy: RetentionSessionTTL,
	}
	withoutKey := base
	withoutKey.Content = strings.NewReader("first")
	if _, err := store.Put(ctx, withoutKey); !IsErrorCode(err, ErrInvalidArgument) {
		t.Fatalf("deterministic id without idempotency key: %v", err)
	}

	first := base
	first.IdempotencyKey = "run-1:final"
	first.Content = strings.NewReader("first")
	meta, err := store.Put(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ArtifactID != base.ArtifactID {
		t.Fatalf("artifact id = %s", meta.ArtifactID)
	}

	collision := base
	collision.IdempotencyKey = "run-1:other"
	collision.Content = strings.NewReader("different")
	if _, err := store.Put(ctx, collision); !IsErrorCode(err, ErrConflict) {
		t.Fatalf("deterministic id collision: %v", err)
	}
}
