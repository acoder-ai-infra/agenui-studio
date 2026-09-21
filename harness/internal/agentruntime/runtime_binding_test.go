package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/skill"
)

func TestRuntimeServicePersistsRuntimeBindingForRun(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := NewMockRuntime(observability.AgentEvent{
		EventType:  EventAgentTextDelta,
		Visibility: observability.VisibilityUserVisible,
		Payload:    JSONPayload(map[string]string{"text": "ok"}),
	})
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	req := testRunRequest()
	req.ConfigSnapshotRef = "config://agent_1/v1"
	req.ConfigHash = "sha256:config-v1"
	req.AgentBindingID = "binding_1"

	events, err := service.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	_ = collect(events)
	snapshot, ok := state.Run(req.RunID)
	if !ok {
		t.Fatal("run snapshot missing")
	}
	binding := snapshot.RuntimeBinding
	if binding.SchemaVersion != RuntimeBindingSchemaVersion || binding.Runtime != RuntimeTypeMock || binding.RuntimeVersion != "test" || binding.AdapterVersion != "test-v1" {
		t.Fatalf("runtime identity was not frozen: %#v", binding)
	}
	if binding.AgentDefinitionHash == "" || binding.ConfigSnapshotRef != req.ConfigSnapshotRef || binding.ConfigHash != req.ConfigHash || binding.AgentBindingID != req.AgentBindingID ||
		binding.TenantID != req.TenantID || binding.UserID != req.UserID {
		t.Fatalf("agent/config identity was not frozen: %#v", binding)
	}
}

func TestRuntimeBindingV1WithoutConfigHashFailsClosed(t *testing.T) {
	binding := RuntimeBinding{
		SchemaVersion: "harness.runtime_binding.v1", Runtime: RuntimeTypeEino,
		RuntimeVersion: "v1", AdapterVersion: "v1", Governance: RuntimeGovernanceBridged,
		AgentDefinitionHash: "sha256:definition", ConfigSnapshotRef: "agent-config://agent/v1/hash",
	}
	if !errors.Is(binding.Validate(), ErrRuntimeBindingInvalid) {
		t.Fatalf("legacy binding without ConfigHash was accepted: %#v", binding)
	}
}

func TestRuntimeCapabilityBindingUsesStableSkillFacts(t *testing.T) {
	first := ModelContextCapabilities{
		Tools:           []string{"tool_b@v1", "tool_a@v1"},
		Skills:          []string{"writer@v1", "planner@v1"},
		EstimatedTokens: 120,
		SkillSnapshots: []skill.Snapshot{{
			ID:                "skill_snapshot_ephemeral_1",
			SkillID:           "planner",
			Version:           "v1",
			TenantID:          "tenant_1",
			ContentHash:       "sha256:skill-content",
			ContentSize:       128,
			EstimatedTokens:   120,
			InjectionStrategy: skill.InjectSystem,
			Dependencies: skill.Dependencies{
				Tools:      []string{"tool_b@v1", "tool_a@v1"},
				MCPServers: []string{"mcp_b", "mcp_a"},
				Skills:     []skill.Ref{{ID: "child_b", Version: "v1"}, {ID: "child_a", Version: "v2"}},
			},
			ResolvedAt: time.Unix(100, 0),
		}},
	}
	second := first
	second.Tools = []string{"tool_a@v1", "tool_b@v1"}
	second.Skills = []string{"planner@v1", "writer@v1"}
	second.EstimatedTokens = 999
	second.SkillSnapshots = append([]skill.Snapshot(nil), first.SkillSnapshots...)
	second.SkillSnapshots[0].ID = "skill_snapshot_ephemeral_2"
	second.SkillSnapshots[0].EstimatedTokens = 999
	second.SkillSnapshots[0].ResolvedAt = time.Unix(200, 0)
	second.SkillSnapshots[0].Dependencies = skill.Dependencies{
		Tools:      []string{"tool_a@v1", "tool_b@v1"},
		MCPServers: []string{"mcp_a", "mcp_b"},
		Skills:     []skill.Ref{{ID: "child_a", Version: "v2"}, {ID: "child_b", Version: "v1"}},
	}

	want, err := newRuntimeCapabilityBinding(first)
	if err != nil {
		t.Fatalf("bind first capability snapshot: %v", err)
	}
	got, err := newRuntimeCapabilityBinding(second)
	if err != nil {
		t.Fatalf("bind repeated capability snapshot: %v", err)
	}
	if want.Hash != got.Hash {
		t.Fatalf("ephemeral skill facts changed capability hash: want=%s got=%s", want.Hash, got.Hash)
	}

	for name, mutate := range map[string]func(*ModelContextCapabilities){
		"content hash": func(capabilities *ModelContextCapabilities) {
			capabilities.SkillSnapshots[0].ContentHash = "sha256:changed"
		},
		"dependency": func(capabilities *ModelContextCapabilities) {
			capabilities.SkillSnapshots[0].Dependencies.Tools = append(capabilities.SkillSnapshots[0].Dependencies.Tools, "tool_c@v1")
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := second
			changed.SkillSnapshots = append([]skill.Snapshot(nil), second.SkillSnapshots...)
			changed.SkillSnapshots[0].Dependencies = canonicalSkillDependencies(second.SkillSnapshots[0].Dependencies)
			mutate(&changed)
			if err := validateRuntimeCapabilityBinding(want, changed); !errors.Is(err, ErrRuntimeCapabilityDrift) {
				t.Fatalf("security-relevant skill change was accepted: %v", err)
			}
		})
	}
}

func TestRuntimeCapabilityBindingRejectsMCPSnapshotDrift(t *testing.T) {
	capabilities := ModelContextCapabilities{
		MCPServers: []string{"maps"},
		MCPSnapshots: []mcp.CapabilitySnapshot{{
			ID:             "mcp_snapshot_1",
			ServerID:       "maps",
			ServerVersion:  "v1",
			PrincipalHash:  "sha256:principal",
			PolicyHash:     "sha256:policy",
			CapabilityHash: "sha256:capability",
			Tools: []mcp.Tool{{
				Name:        "search",
				Description: "search maps",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}},
			CreatedAt: time.Unix(100, 0),
		}},
	}
	binding, err := newRuntimeCapabilityBinding(capabilities)
	if err != nil {
		t.Fatalf("bind MCP snapshot: %v", err)
	}

	repeated := capabilities
	repeated.MCPSnapshots = append([]mcp.CapabilitySnapshot(nil), capabilities.MCPSnapshots...)
	repeated.MCPSnapshots[0].CreatedAt = time.Unix(200, 0)
	if err := validateRuntimeCapabilityBinding(binding, repeated); err != nil {
		t.Fatalf("MCP timestamp should not change frozen capability: %v", err)
	}

	changed := repeated
	changed.MCPSnapshots = append([]mcp.CapabilitySnapshot(nil), repeated.MCPSnapshots...)
	changed.MCPSnapshots[0].PolicyHash = "sha256:changed"
	if err := validateRuntimeCapabilityBinding(binding, changed); !errors.Is(err, ErrRuntimeCapabilityDrift) {
		t.Fatalf("MCP policy drift was accepted: %v", err)
	}
}

func TestRuntimeCapabilityBindingIdempotencyChecksFrozenMCPIdentity(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := newVersionedCountingRuntime(RuntimeTypeEino, "eino-v1", "adapter-v1")
	req := runtimeBindingRunRequest(RuntimeTypeEino)
	if _, err := state.StartRun(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	runtimeBinding, err := newRuntimeBinding(context.Background(), runtime, req.Definition, runtimeBindingFactsFromRun(req))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.BindRuntime(context.Background(), req.RunID, runtimeBinding); err != nil {
		t.Fatal(err)
	}
	capabilityBinding := RuntimeCapabilityBinding{
		Hash: "sha256:capability",
		MCPSnapshots: []RuntimeMCPSnapshotBinding{{
			SnapshotID: "snapshot_1", ServerID: "maps", ServerVersion: "v1",
			PrincipalHash: "sha256:principal", PolicyHash: "sha256:policy", CapabilityHash: "sha256:mcp",
		}},
	}
	if err := state.BindRuntimeCapabilities(context.Background(), req.RunID, capabilityBinding); err != nil {
		t.Fatal(err)
	}
	changed := capabilityBinding
	changed.MCPSnapshots = append([]RuntimeMCPSnapshotBinding(nil), capabilityBinding.MCPSnapshots...)
	changed.MCPSnapshots[0].SnapshotID = "snapshot_2"
	if err := state.BindRuntimeCapabilities(context.Background(), req.RunID, changed); !errors.Is(err, ErrRuntimeCapabilityDrift) {
		t.Fatalf("same hash with changed MCP identity was accepted: %v", err)
	}
}

func TestRuntimeCapabilityBindingContextClearsInheritedParentBinding(t *testing.T) {
	parent, err := newRuntimeCapabilityBinding(ModelContextCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	parentCtx := withRuntimeCapabilityBinding(context.Background(), parent)
	if _, ok := runtimeCapabilityBindingFromContext(parentCtx); !ok {
		t.Fatal("parent capability binding was not installed")
	}

	childCtx := withRuntimeCapabilityBinding(parentCtx, nil)
	if inherited, ok := runtimeCapabilityBindingFromContext(childCtx); ok {
		t.Fatalf("fresh child run inherited parent capability binding: %#v", inherited)
	}
}

func TestRuntimeServiceResumeDoesNotFallbackFromBoundRuntime(t *testing.T) {
	state := NewInMemoryStateManager()
	bound := newVersionedCountingRuntime(RuntimeTypeEino, "eino-v1", "adapter-v1")
	alternate := newVersionedCountingRuntime(RuntimeTypeNative, "native-v1", "adapter-v1")
	service := NewRuntimeService(bound, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	service.RegisterRuntime(RuntimeTypeNative, alternate)
	req := runtimeBindingRunRequest(RuntimeTypeEino)
	req.Definition.Runtime.Candidates = []RuntimeType{RuntimeTypeNative}
	prepareWaitingBoundRun(t, state, bound, req)
	bound.Unhealthy = true
	bound.HealthReason = "bound runtime unavailable"

	_, err := service.Resume(context.Background(), runtimeBindingResumeRequest(req))
	if !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("expected bound runtime failure, got %v", err)
	}
	if got := alternate.ResumeCalls(); got != 0 {
		t.Fatalf("resume switched to fallback runtime: calls=%d", got)
	}
	snapshot, ok := state.Run(req.RunID)
	if !ok || snapshot.Status != RunStatusWaitingControl {
		t.Fatalf("retryable pre-execution failure destroyed resumability: %#v", snapshot)
	}
	if eventIndex(state.Events(req.RunID), EventResumeFailed) < 0 {
		t.Fatalf("resume_failed event missing: %#v", state.Events(req.RunID))
	}

	bound.Unhealthy = false
	bound.HealthReason = ""
	resumed, err := service.Resume(context.Background(), runtimeBindingResumeRequest(req))
	if err != nil {
		var runtimeErr *RuntimeError
		_ = errors.As(err, &runtimeErr)
		t.Fatalf("retry bound runtime: %v cause=%v", err, runtimeErr.Cause)
	}
	_ = collect(resumed)
	snapshot, _ = state.Run(req.RunID)
	if snapshot.Status != RunStatusCompleted || bound.ResumeCalls() != 1 || bound.ResumeConfigHash() != req.ConfigHash ||
		bound.ResumeContextSnapshotRef() != req.ContextSnapshotRef || bound.ResumeTenantID() != req.TenantID || bound.ResumeUserID() != req.UserID {
		t.Fatalf("bound runtime did not recover after retry: run=%#v calls=%d", snapshot, bound.ResumeCalls())
	}
}

func TestRuntimeServiceResumeRejectsRuntimeVersionDrift(t *testing.T) {
	state := NewInMemoryStateManager()
	original := newVersionedCountingRuntime(RuntimeTypeEino, "eino-v1", "adapter-v1")
	req := runtimeBindingRunRequest(RuntimeTypeEino)
	prepareWaitingBoundRun(t, state, original, req)

	replacement := newVersionedCountingRuntime(RuntimeTypeEino, "eino-v2", "adapter-v1")
	service := NewRuntimeService(replacement, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	_, err := service.Resume(context.Background(), runtimeBindingResumeRequest(req))
	if !errors.Is(err, ErrRuntimeBindingMismatch) {
		t.Fatalf("expected runtime binding mismatch, got %v", err)
	}
	if got := replacement.ResumeCalls(); got != 0 {
		t.Fatalf("version-drifted runtime executed resume: calls=%d", got)
	}
	if snapshot, ok := state.Run(req.RunID); !ok || snapshot.Status != RunStatusFailed || snapshot.ErrorCode != "RUNTIME_BINDING_MISMATCH" {
		t.Fatalf("runtime identity drift must fail the claimed resume: %#v", snapshot)
	}
}

func TestRuntimeServiceResumeRejectsCompactionPolicyDrift(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := newVersionedCountingRuntime(RuntimeTypeEino, "eino-v1", "adapter-v1")
	req := runtimeBindingRunRequest(RuntimeTypeEino)
	req.Definition.ContextCompaction = DefaultContextCompactionPolicy()
	prepareWaitingBoundRun(t, state, runtime, req)
	resume := runtimeBindingResumeRequest(req)
	resume.Definition.ContextCompaction.PolicyHash = ""
	resume.Definition.ContextCompaction.TargetRatio = 0.60
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	_, err := service.Resume(context.Background(), resume)
	if !errors.Is(err, ErrRuntimeBindingMismatch) {
		t.Fatalf("expected frozen compaction policy mismatch, got %v", err)
	}
	if got := runtime.ResumeCalls(); got != 0 {
		t.Fatalf("policy-drifted runtime executed resume: calls=%d", got)
	}
}

func TestRuntimeServiceResumeRejectsPrincipalDrift(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := newVersionedCountingRuntime(RuntimeTypeEino, "eino-v1", "adapter-v1")
	req := runtimeBindingRunRequest(RuntimeTypeEino)
	prepareWaitingBoundRun(t, state, runtime, req)
	resume := runtimeBindingResumeRequest(req)
	resume.TenantID = "other_tenant"
	service := NewRuntimeService(runtime, state, observability.NoopLogger{}, observability.NewNoopTracer("test"))

	_, err := service.Resume(context.Background(), resume)
	if !errors.Is(err, ErrRuntimeBindingMismatch) {
		t.Fatalf("expected principal binding mismatch, got %v", err)
	}
	if runtime.ResumeCalls() != 0 {
		t.Fatal("runtime executed after principal drift")
	}
}

func TestInMemoryStateManagerNonRetryableResumeFailureIsTerminal(t *testing.T) {
	state := NewInMemoryStateManager()
	runtime := newVersionedCountingRuntime(RuntimeTypeEino, "eino-v1", "adapter-v1")
	req := runtimeBindingRunRequest(RuntimeTypeEino)
	prepareWaitingBoundRun(t, state, runtime, req)
	resume := runtimeBindingResumeRequest(req)
	const attemptID = "resume_attempt_1"
	accepted := observability.AgentEvent{EventID: "event_resume_accepted", EventType: EventResumeAccepted, RunID: req.RunID}
	if err := state.BeginResume(context.Background(), resume, attemptID, accepted); err != nil {
		t.Fatalf("begin resume: %v", err)
	}
	runtimeErr := NewRuntimeError(ErrorSchemaValidation, "RESUME_SCHEMA_INVALID", "resume schema invalid")
	failed := observability.AgentEvent{EventID: "event_resume_failed", EventType: EventResumeFailed, RunID: req.RunID, Error: runtimeErr.EventError()}
	if err := state.FailResumeAttempt(context.Background(), req.RunID, attemptID, failed, runtimeErr, false); err != nil {
		t.Fatalf("fail resume attempt: %v", err)
	}
	snapshot, ok := state.Run(req.RunID)
	if !ok || snapshot.Status != RunStatusFailed || snapshot.ErrorCode != runtimeErr.Code {
		t.Fatalf("non-retryable resume failure should be terminal: %#v", snapshot)
	}
}

func prepareWaitingBoundRun(t *testing.T, state *InMemoryStateManager, runtime AgentRuntime, req RunRequest) {
	prepareWaitingBoundRunWithCapabilities(t, state, runtime, req, nil)
}

func prepareWaitingBoundRunWithCapabilities(
	t *testing.T,
	state *InMemoryStateManager,
	runtime AgentRuntime,
	req RunRequest,
	capabilities *RuntimeCapabilityBinding,
) {
	t.Helper()
	if _, err := state.StartRun(context.Background(), req); err != nil {
		t.Fatalf("start run: %v", err)
	}
	binding, err := newRuntimeBinding(context.Background(), runtime, req.Definition, runtimeBindingFactsFromRun(req))
	if err != nil {
		t.Fatalf("build runtime binding: %v", err)
	}
	if err := state.BindRuntime(context.Background(), req.RunID, binding); err != nil {
		t.Fatalf("bind runtime: %v", err)
	}
	if capabilities != nil {
		if err := state.BindRuntimeCapabilities(context.Background(), req.RunID, *capabilities); err != nil {
			t.Fatalf("bind runtime capabilities: %v", err)
		}
	}
	if err := state.EnterWaitingControl(context.Background(), WaitingControlRequest{
		SessionID:        req.SessionID,
		RunID:            req.RunID,
		CheckpointID:     "checkpoint_1",
		ControlRequestID: "control_1",
		ResumeToken:      "resume_1",
	}); err != nil {
		t.Fatalf("enter waiting control: %v", err)
	}
}

func runtimeBindingRunRequest(runtimeType RuntimeType) RunRequest {
	return RunRequest{
		SessionID:          "session_binding",
		RunID:              "run_binding",
		ConfigSnapshotRef:  "config://agent_binding/v1",
		ConfigHash:         "sha256:config-v1",
		AgentBindingID:     "binding_runtime_1",
		ContextSnapshotRef: "context://agent_binding/run",
		TenantID:           "tenant_binding",
		UserID:             "user_binding",
		Definition: AgentDefinition{
			AgentID:   "agent_binding",
			AgentType: "assistant",
			Version:   "v1",
			Runtime:   RuntimeSpec{Type: runtimeType, Mode: RuntimeModeReact},
		},
		Trace: observability.TraceContext{TraceID: "trace_binding", TenantID: "tenant_binding", UserID: "user_binding"},
	}
}

func runtimeBindingResumeRequest(req RunRequest) ResumeRequest {
	return ResumeRequest{
		SessionID:          req.SessionID,
		RunID:              req.RunID,
		Definition:         req.Definition,
		ContextSnapshotRef: req.ContextSnapshotRef,
		ConfigSnapshotRef:  req.ConfigSnapshotRef,
		AgentBindingID:     req.AgentBindingID,
		CheckpointID:       "checkpoint_1",
		ControlRequestID:   "control_1",
		ResumeToken:        "resume_1",
		Trace:              observability.TraceContext{TraceID: req.Trace.TraceID},
	}
}

type versionedCountingRuntime struct {
	*MockRuntime
	runtimeVersion   string
	adapterVersion   string
	mu               sync.Mutex
	resumeCalls      int
	resumeConfigHash string
	resumeContextRef string
	resumeTenantID   string
	resumeUserID     string
}

func newVersionedCountingRuntime(runtimeType RuntimeType, runtimeVersion, adapterVersion string) *versionedCountingRuntime {
	mock := NewMockRuntime(observability.AgentEvent{
		EventType:  EventAgentTextDelta,
		Visibility: observability.VisibilityUserVisible,
		Payload:    JSONPayload(map[string]string{"text": "ok"}),
	})
	mock.NameValue = string(runtimeType)
	return &versionedCountingRuntime{MockRuntime: mock, runtimeVersion: runtimeVersion, adapterVersion: adapterVersion}
}

func (r *versionedCountingRuntime) Descriptor(ctx context.Context) RuntimeDescriptor {
	descriptor := r.MockRuntime.Descriptor(ctx)
	descriptor.RuntimeVersion = r.runtimeVersion
	descriptor.AdapterVersion = r.adapterVersion
	descriptor.CheckpointFormat = "checkpoint-v1"
	return descriptor
}

func (r *versionedCountingRuntime) Resume(ctx context.Context, req ResumeRequest) (<-chan observability.AgentEvent, error) {
	r.mu.Lock()
	r.resumeCalls++
	r.resumeConfigHash = req.ConfigHash
	r.resumeContextRef = req.ContextSnapshotRef
	r.resumeTenantID = req.TenantID
	r.resumeUserID = req.UserID
	r.mu.Unlock()
	return r.MockRuntime.Resume(ctx, req)
}

func (r *versionedCountingRuntime) ResumeConfigHash() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resumeConfigHash
}

func (r *versionedCountingRuntime) ResumeContextSnapshotRef() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resumeContextRef
}

func (r *versionedCountingRuntime) ResumeTenantID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resumeTenantID
}

func (r *versionedCountingRuntime) ResumeUserID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resumeUserID
}

func (r *versionedCountingRuntime) ResumeCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resumeCalls
}
