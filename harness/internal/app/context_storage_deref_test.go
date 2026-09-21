package app

import (
	"context"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	metamem "github.com/AGenUI/agenui-studio/harness/internal/artifact/metastore"
	objmem "github.com/AGenUI/agenui-studio/harness/internal/artifact/objectstore"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// TestLedgerResolvesContentRefForModelInput asserts the context ledger feeds the
// model the FULL message body (dereferenced from ContentRef), not the truncated
// inline preview — so large user messages are not silently clipped before the
// model sees them.
func TestLedgerResolvesContentRefForModelInput(t *testing.T) {
	store := artifact.NewStore(artifact.StoreConfig{
		ObjectStore: objmem.NewMemory(), MetadataStore: metamem.NewMemory(),
	})
	const tenant, session, run = "t1", "sess-1", "run-1"
	fullBody := strings.Repeat("full-body ", 500) // well beyond any preview

	// Externalize the body as a user-input artifact (runtime actor, as the ingest
	// path would), scoped to the current run.
	putCtx := artifact.ContextWithActor(context.Background(), artifact.Actor{
		Role: artifact.ActorRuntime, TenantID: tenant, SessionID: session, RunID: run,
	})
	meta, err := store.Put(putCtx, artifact.PutArtifactRequest{
		TenantID: tenant, UserID: "user-1", SessionID: session, RunID: run,
		OwnerModule: artifact.OwnerModuleRuntime, OwnerID: run,
		ArtifactType: artifact.ArtifactTypeFile, MimeType: "text/plain", Name: "user-input.txt",
		Visibility: artifact.VisibilityUserVisible, RetentionPolicy: artifact.RetentionSessionTTL,
		Content: strings.NewReader(fullBody),
	})
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}

	ledger := storageMessageLedger{artifacts: store}
	msg := &storage.Message{
		ID: "m1", SessionID: session, Role: "user",
		ContentPreview: "full-body full-body …(truncated)", // a short preview only
		ContentRef:     meta.ArtifactRef,
	}
	ctx := observability.WithTraceContext(context.Background(), observability.TraceContext{TenantID: tenant, UserID: "user-1", SessionID: session, RunID: run})

	got, err := ledger.resolveContent(ctx, msg)
	if err != nil {
		t.Fatal(err)
	}
	if got != fullBody {
		t.Fatalf("resolveContent must return the full dereferenced body (len=%d), got len=%d (%q…)", len(fullBody), len(got), got[:min(40, len(got))])
	}

	// A message with no ContentRef keeps its inline content.
	plain := &storage.Message{ID: "m2", SessionID: session, Role: "user", ContentPreview: "hi"}
	if got, err := ledger.resolveContent(ctx, plain); err != nil || got != "hi" {
		t.Fatalf("inline message content = %q, want hi", got)
	}
	inline := &storage.Message{ID: "m2-inline", SessionID: session, Role: "assistant", ContentPreview: "complete small reply", ContentRef: "message://m2-inline/content"}
	if got, err := ledger.resolveContent(ctx, inline); err != nil || got != inline.ContentPreview {
		t.Fatalf("message ref content = %q err=%v", got, err)
	}

	// A bad ref fails closed. Preview fallback belongs to non-model history APIs;
	// using it here would make one snapshot resolve to different model facts.
	bad := &storage.Message{ID: "m3", SessionID: session, Role: "user", ContentPreview: "prev", ContentRef: "artifact://nope"}
	if _, err := ledger.resolveContent(ctx, bad); err == nil {
		t.Fatal("unresolvable model-context ref must fail closed")
	}
	bad.ContentRef = "https://example.com/not-governed"
	if _, err := ledger.resolveContent(ctx, bad); err == nil {
		t.Fatal("unknown content ref scheme must fail closed")
	}
}

func TestLedgerResolvesPreviousRunArtifactOnlyWithinSameUserSession(t *testing.T) {
	store := artifact.NewStore(artifact.StoreConfig{
		ObjectStore: objmem.NewMemory(), MetadataStore: metamem.NewMemory(),
	})
	const tenant, user, session = "t1", "user-1", "sess-1"
	putCtx := artifact.ContextWithActor(context.Background(), artifact.Actor{
		Role: artifact.ActorRuntime, TenantID: tenant, UserID: user, SessionID: session, RunID: "run-1",
	})
	meta, err := store.Put(putCtx, artifact.PutArtifactRequest{
		TenantID: tenant, UserID: user, SessionID: session, RunID: "run-1",
		OwnerModule: artifact.OwnerModuleRuntime, OwnerID: "run-1",
		ArtifactType: artifact.ArtifactTypeFile, MimeType: "text/plain", Name: "message.txt",
		Visibility: artifact.VisibilityInternal, RetentionPolicy: artifact.RetentionSessionTTL,
		Content: strings.NewReader("durable previous-run body"),
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := &storage.Message{ID: "m1", SessionID: session, ContentPreview: "preview", ContentRef: meta.ArtifactRef}
	ledger := storageMessageLedger{artifacts: store}

	allowed := observability.WithTraceContext(context.Background(), observability.TraceContext{
		TenantID: tenant, UserID: user, SessionID: session, RunID: "run-2",
	})
	if got, err := ledger.resolveContent(allowed, msg); err != nil || got != "durable previous-run body" {
		t.Fatalf("same-session previous-run body = %q", got)
	}

	for _, tc := range []observability.TraceContext{
		{TenantID: tenant, UserID: "user-2", SessionID: session, RunID: "run-2"},
		{TenantID: tenant, UserID: user, SessionID: "sess-2", RunID: "run-2"},
		{TenantID: "tenant-2", UserID: user, SessionID: session, RunID: "run-2"},
	} {
		ctx := observability.WithTraceContext(context.Background(), tc)
		if got, err := ledger.resolveContent(ctx, msg); err == nil {
			t.Fatalf("cross-scope content resolved for %#v: %q", tc, got)
		}
	}
}
