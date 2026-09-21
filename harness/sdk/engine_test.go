package harness

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

func TestTextMessageProducesTextPart(t *testing.T) {
	m := TextMessage("hello")
	if m.Role != RoleUser {
		t.Fatalf("role: want %q got %q", RoleUser, m.Role)
	}
	if len(m.Parts) != 1 {
		t.Fatalf("parts: want 1 got %d", len(m.Parts))
	}
	if m.Parts[0].Kind != PartKindText {
		t.Fatalf("kind: want %q got %q", PartKindText, m.Parts[0].Kind)
	}
	if m.Parts[0].Text != "hello" {
		t.Fatalf("text: want %q got %q", "hello", m.Parts[0].Text)
	}
}

func TestValidateStartRequestRejectsEmptyIdentity(t *testing.T) {
	err := validateStartRequest(StartRequest{
		Input: TextMessage("hi"),
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest, got %v", err)
	}
}

func TestValidateStartRequestRejectsEmptyParts(t *testing.T) {
	err := validateStartRequest(StartRequest{
		Identity: Identity{TenantID: "t"},
		Input:    Message{Role: RoleUser},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest, got %v", err)
	}
}

func TestValidateStartRequestRejectsNonUserRole(t *testing.T) {
	err := validateStartRequest(StartRequest{
		Identity: Identity{TenantID: "t"},
		Input: Message{
			Role:  RoleAssistant,
			Parts: []MessagePart{{Kind: PartKindText, Text: "no"}},
		},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest, got %v", err)
	}
}

func TestValidateStartRequestEnforcesUserIDCharacterLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		userID  string
		wantErr bool
	}{
		{name: "64 ASCII characters", userID: strings.Repeat("u", 64)},
		{name: "64 UTF-8 characters", userID: strings.Repeat("用", 64)},
		{name: "65 characters", userID: strings.Repeat("u", 65), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateStartRequest(StartRequest{
				Identity: Identity{TenantID: "t", UserID: tc.userID},
				Input:    TextMessage("hi"),
			})
			if tc.wantErr != errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("validateStartRequest() error = %v, want ErrInvalidRequest=%v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateStartRequestEnforcesIdentifierCharacterLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  StartRequest
	}{
		{name: "tenant 65", req: StartRequest{Identity: Identity{TenantID: strings.Repeat("t", 65)}, Input: TextMessage("hi")}},
		{name: "session 65", req: StartRequest{Identity: Identity{TenantID: "t", SessionID: strings.Repeat("s", 65)}, Input: TextMessage("hi")}},
		{name: "agent 129", req: StartRequest{Identity: Identity{TenantID: "t", AgentID: strings.Repeat("a", 129)}, Input: TextMessage("hi")}},
		{name: "idempotency 129", req: StartRequest{Identity: Identity{TenantID: "t"}, IdempotencyKey: strings.Repeat("i", 129), Input: TextMessage("hi")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateStartRequest(tc.req); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("validateStartRequest() error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

type oversizedIdentityResolver struct {
	resolved extension.ResolvedIdentity
}

func (r oversizedIdentityResolver) Resolve(context.Context, extension.IdentityRequest) (extension.ResolvedIdentity, error) {
	return r.resolved, nil
}

func TestPrepareTurnRejectsOversizedResolvedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resolved extension.ResolvedIdentity
	}{
		{name: "tenant_id", resolved: extension.ResolvedIdentity{TenantID: strings.Repeat("t", 65)}},
		{name: "user_id", resolved: extension.ResolvedIdentity{UserID: strings.Repeat("u", 65)}},
		{name: "session_id", resolved: extension.ResolvedIdentity{SessionID: strings.Repeat("s", 65)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog, err := kernel.NewExtensionCatalog([]kernel.ExtensionEntry{{
				ID: "oversized.identity", Kind: kernel.ExtIdentityResolver,
				Implementation: oversizedIdentityResolver{resolved: tc.resolved},
			}})
			if err != nil {
				t.Fatalf("catalog: %v", err)
			}
			engine := &engineImpl{turns: kernel.NewTurnPipeline(catalog, kernel.TurnEnvironment{})}
			_, _, err = engine.prepareTurn(context.Background(), StartRequest{
				Identity: Identity{TenantID: "t", UserID: "caller"},
				Input:    TextMessage("hi"),
			})
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("prepareTurn() error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestClassifyStartErrorMapsPermissionDenied(t *testing.T) {
	err := classifyStartError(storage.NewError(storage.ErrPermissionDenied, "session owner mismatch"))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("classifyStartError() error = %v, want ErrPermissionDenied", err)
	}
}

func TestValidatePartMustMatchKind(t *testing.T) {
	cases := []struct {
		name string
		part MessagePart
	}{
		{"text without text", MessagePart{Kind: PartKindText}},
		{"json without json", MessagePart{Kind: PartKindJSON}},
		{"image_ref without id", MessagePart{Kind: PartKindImageRef, Ref: ArtifactRef{}}},
		{"file_ref without id", MessagePart{Kind: PartKindFileRef, Ref: ArtifactRef{}}},
		{"artifact_ref without id", MessagePart{Kind: PartKindArtifactRef, Ref: ArtifactRef{}}},
		{"inline_binary without bytes", MessagePart{Kind: PartKindInlineBinary}},
		{"unknown kind", MessagePart{Kind: "unknown"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validatePart(tc.part); err == nil {
				t.Fatalf("want error, got nil")
			}
		})
	}
}

func TestRenderPreviewJoinsPartsInOrder(t *testing.T) {
	msg := Message{
		Role: RoleUser,
		Parts: []MessagePart{
			{Kind: PartKindText, Text: "line 1"},
			{Kind: PartKindText, Text: "line 2"},
			{Kind: PartKindArtifactRef, Ref: ArtifactRef{ID: "art_42"}},
			{Kind: PartKindInlineBinary, Inline: []byte{1, 2, 3}},
			{Kind: PartKindJSON, JSON: json.RawMessage(`{"k":"v"}`)},
		},
	}
	got := renderPreview(msg)
	want := "line 1\nline 2\n[artifact:art_42]\n[inline:3 bytes]\n{\"k\":\"v\"}"
	if got != want {
		t.Fatalf("preview mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestRunStatusIsTerminal(t *testing.T) {
	terminals := []RunStatus{RunStatusCompleted, RunStatusFailed, RunStatusCancelled, RunStatusExpired}
	for _, s := range terminals {
		if !s.IsTerminal() {
			t.Fatalf("%q should be terminal", s)
		}
	}
	nonTerminals := []RunStatus{RunStatusCreated, RunStatusRunning, RunStatusWaitingControl, RunStatusResuming}
	for _, s := range nonTerminals {
		if s.IsTerminal() {
			t.Fatalf("%q should not be terminal", s)
		}
	}
}

func TestIsTerminalStreamError(t *testing.T) {
	if !IsTerminalStreamError(io.EOF) {
		t.Fatal("io.EOF should be terminal")
	}
	if !IsTerminalStreamError(ErrClosed) {
		t.Fatal("ErrClosed should be terminal")
	}
	if IsTerminalStreamError(nil) {
		t.Fatal("nil should not be terminal")
	}
	if IsTerminalStreamError(ErrInvalidRequest) {
		t.Fatal("ErrInvalidRequest should not be terminal")
	}
}

// TestEventStreamDeliversInOrder wires kernel.Subscription -> harness.EventStream
// and verifies Sequence order, terminal detection, and Cursor tracking.
func TestEventStreamDeliversInOrder(t *testing.T) {
	broker := protocol.NewMemoryBroker()
	k := &kernel.Kernel{Broker: broker}
	sub, err := k.SubscribeRun(context.Background(), "run_a")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	stream := newEventStream(sub, "run_a", nil)

	events := []observability.AgentEvent{
		{EventID: "e1", RunID: "run_a", Sequence: 1, EventType: observability.EventRunStarted, Visibility: observability.VisibilityDebug, CreatedAt: time.Now()},
		{EventID: "e2", RunID: "run_a", Sequence: 2, EventType: observability.EventAgentTextDelta, Visibility: observability.VisibilityUserVisible, CreatedAt: time.Now()},
		{EventID: "e3", RunID: "run_a", Sequence: 3, EventType: observability.EventRunCompleted, Visibility: observability.VisibilityDebug, CreatedAt: time.Now()},
	}
	go func() {
		for _, e := range events {
			_ = broker.Publish(context.Background(), e)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for i, want := range events {
		got, err := stream.Next(ctx)
		if err != nil {
			t.Fatalf("Next[%d]: %v", i, err)
		}
		if got.EventID != want.EventID {
			t.Fatalf("Next[%d]: EventID want %q got %q", i, want.EventID, got.EventID)
		}
		if got.Sequence != want.Sequence {
			t.Fatalf("Next[%d]: Sequence want %d got %d", i, want.Sequence, got.Sequence)
		}
	}
	if c := stream.Cursor(); c.AfterSequence != 3 || c.RunID != "run_a" {
		t.Fatalf("cursor: got %+v", c)
	}

	// Closing the broker's subscription (via stream.Close) should stop delivery.
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := stream.Next(ctx); !errors.Is(err, ErrClosed) && !errors.Is(err, io.EOF) {
		t.Fatalf("after close: want ErrClosed or EOF, got %v", err)
	}
}

// TestEventStreamSkipUntilHonoursAfterSequence ensures SubscribeRequest.AfterSequence
// filters out already-seen events.
func TestEventStreamSkipUntilHonoursAfterSequence(t *testing.T) {
	broker := protocol.NewMemoryBroker()
	k := &kernel.Kernel{Broker: broker}
	sub, err := k.SubscribeRun(context.Background(), "run_b")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	stream := newEventStream(sub, "run_b", nil)
	stream.setSkipUntil(2)

	go func() {
		_ = broker.Publish(context.Background(), observability.AgentEvent{EventID: "e1", RunID: "run_b", Sequence: 1, EventType: observability.EventRunStarted, Visibility: observability.VisibilityDebug})
		_ = broker.Publish(context.Background(), observability.AgentEvent{EventID: "e2", RunID: "run_b", Sequence: 2, EventType: observability.EventAgentTextDelta, Visibility: observability.VisibilityUserVisible})
		_ = broker.Publish(context.Background(), observability.AgentEvent{EventID: "e3", RunID: "run_b", Sequence: 3, EventType: observability.EventRunCompleted, Visibility: observability.VisibilityDebug})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got, err := stream.Next(ctx)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if got.Sequence != 3 {
		t.Fatalf("want first delivered event Sequence=3 got %d", got.Sequence)
	}
	_ = stream.Close()
}

// TestBuildReportSurfacesSchemaVersions is a small smoke test for
// buildReportFromKernel without importing internal/app.
func TestBuildReportSurfacesSchemaVersions(t *testing.T) {
	k := &kernel.Kernel{
		DefaultAgentID: "demo",
		Providers:      []string{"mock"},
		Tenants:        map[string][]string{"default": {"mock"}},
		Broker:         protocol.NewMemoryBroker(),
	}
	report := buildReportFromKernel(k, "local")
	if report.SDKVersion != Version {
		t.Fatalf("SDKVersion: want %q got %q", Version, report.SDKVersion)
	}
	found := false
	for _, s := range report.SchemaVersions {
		if s == "harness.agent_event.v1" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("SchemaVersions missing canonical event schema: %v", report.SchemaVersions)
	}
	if len(report.Agents) != 1 || report.Agents[0].AgentID != "demo" {
		t.Fatalf("Agents: want demo-only, got %+v", report.Agents)
	}
	if len(report.Unsupported) == 0 {
		t.Fatalf("Unsupported: want warnings for missing runs/run_entry, got none")
	}
}
