package agentruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func TestRuntimeEnvironmentValidateFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		dependency RuntimeDependency
	}{
		{name: "model", dependency: RuntimeDependencyModel},
		{name: "tool", dependency: RuntimeDependencyTool},
		{name: "sub-agent", dependency: RuntimeDependencySubAgent},
		{name: "checkpoint", dependency: RuntimeDependencyCheckpoint},
		{name: "unknown", dependency: RuntimeDependency("unknown")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (RuntimeEnvironment{}).Validate(RuntimeEnvironmentRequirements{Dependencies: []RuntimeDependency{tt.dependency}})
			if !errors.Is(err, ErrRuntimeEnvironmentDependencyMissing) {
				t.Fatalf("expected missing dependency error, got %v", err)
			}
		})
	}
}

func TestRuntimeEnvironmentRejectsRawModelGateway(t *testing.T) {
	requirement := RuntimeEnvironmentRequirements{Dependencies: []RuntimeDependency{RuntimeDependencyModel}}
	raw := RuntimeEnvironment{Models: managedRuntimeModelInvoker{}}
	if err := raw.Validate(requirement); !errors.Is(err, ErrRuntimeEnvironmentDependencyMissing) {
		t.Fatalf("raw model gateway must not be exposed to an adapter: %v", err)
	}
	governed := NewEinoRuntime(nil, raw, nil).Environment
	if err := governed.Validate(requirement); err != nil {
		t.Fatalf("constructor did not install model input governance: %v", err)
	}
}

func TestRuntimeEnvironmentResolvesCompactorFromFrozenPolicy(t *testing.T) {
	called := false
	environment := RuntimeEnvironment{ContextCompactors: ContextCompactorProviderFunc(func(_ context.Context, policy ContextCompactionPolicy, descriptor RuntimeDescriptor) (RuntimePreModelCompactor, error) {
		called = true
		if policy.PolicyHash == "" || descriptor.Name != RuntimeTypeNative {
			t.Fatalf("unfrozen provider input: policy=%#v descriptor=%#v", policy, descriptor)
		}
		return &DefaultRuntimePreModelCompactor{}, nil
	})}
	compactor, err := environment.ResolvePreModelCompactor(context.Background(), ContextCompactionPolicy{}, RuntimeDescriptor{Name: RuntimeTypeNative})
	if err != nil || compactor == nil || !called {
		t.Fatalf("resolve compactor: compactor=%T called=%v err=%v", compactor, called, err)
	}
	policy := DefaultContextCompactionPolicy()
	policy.PolicyHash = ""
	policy.SemanticSummary = SemanticSummaryRequired
	if compactor, err := (RuntimeEnvironment{}).ResolvePreModelCompactor(context.Background(), policy, RuntimeDescriptor{Name: RuntimeTypeNative}); err != nil || compactor == nil {
		t.Fatalf("default provider was not installed: compactor=%T error=%v", compactor, err)
	}
	nilProvider := RuntimeEnvironment{ContextCompactors: ContextCompactorProviderFunc(func(context.Context, ContextCompactionPolicy, RuntimeDescriptor) (RuntimePreModelCompactor, error) {
		return nil, nil
	})}
	if _, err := nilProvider.ResolvePreModelCompactor(context.Background(), policy, RuntimeDescriptor{Name: RuntimeTypeNative}); !errors.Is(err, ErrRuntimePreModelCompactorMissing) {
		t.Fatalf("nil required provider result error=%v", err)
	}
}

func TestRuntimeCapabilitiesSupportsCanonicalNames(t *testing.T) {
	capabilities := RuntimeCapabilities{Streaming: true, Checkpoint: true, A2A: true}
	for _, capability := range []string{"streaming", "checkpoint", "a2a"} {
		if !capabilities.Supports(capability) {
			t.Fatalf("expected %s to be supported", capability)
		}
	}
	for _, capability := range []string{"tool_call", "unknown"} {
		if capabilities.Supports(capability) {
			t.Fatalf("expected %s to be unsupported", capability)
		}
	}
}

func TestValidateRequiredRuntimeCapabilitiesRejectsUnknownAndMissing(t *testing.T) {
	for _, required := range [][]string{{"tool_call"}, {"not_registered"}} {
		if err := validateRequiredRuntimeCapabilities(required, RuntimeCapabilities{}); !errors.Is(err, ErrRuntimeCapabilityMissing) {
			t.Fatalf("expected required capability rejection for %v, got %v", required, err)
		}
	}
	if err := validateRequiredRuntimeCapabilities([]string{"streaming"}, RuntimeCapabilities{Streaming: true}); err != nil {
		t.Fatalf("expected supported capability: %v", err)
	}
}

func TestEinoDescriptorDeclaresBridgedNativeCheckpoint(t *testing.T) {
	runtime := NewEinoRuntime(nil, RuntimeEnvironment{}, nil)
	descriptor := runtime.Descriptor(context.Background())
	if descriptor.Name != RuntimeTypeEino || descriptor.RuntimeVersion != EinoRuntimeVersion || descriptor.AdapterVersion != EinoAdapterVersion || descriptor.Governance != RuntimeGovernanceBridged || descriptor.CheckpointFormat != EinoCheckpointFormat {
		t.Fatalf("unexpected descriptor: %#v", descriptor)
	}
}

func TestRuntimeSelectionRejectsMissingRequiredCapability(t *testing.T) {
	runtime := NewNativeDirectRuntime(managedRuntimeModelInvoker{}, nil, nil)
	service := NewRuntimeService(runtime, nil, observability.NoopLogger{}, observability.NewNoopTracer("test"))
	definition := AgentDefinition{
		AgentID:              "agent_1",
		Version:              "v1",
		Runtime:              RuntimeSpec{Type: RuntimeTypeNative, Mode: RuntimeModeDirect},
		RequiredCapabilities: []string{"checkpoint"},
	}
	_, _, _, _, err := service.buildRuntime(context.Background(), RunRequest{RunID: "run_1", Definition: definition})
	if !errors.Is(err, ErrRuntimeCapabilityMissing) {
		t.Fatalf("expected capability mismatch, got %v", err)
	}
}

type managedRuntimeModelInvoker struct{}

func (managedRuntimeModelInvoker) Invoke(context.Context, ModelInvokeRequest) (<-chan ModelStreamItem, error) {
	return nil, nil
}
