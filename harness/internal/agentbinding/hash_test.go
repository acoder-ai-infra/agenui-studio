package agentbinding

import (
	"strings"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/executionmode"
)

func TestBindingHashExcludesIdentityTimeAndConfig(t *testing.T) {
	t.Parallel()
	base := validEffectiveBinding()
	want, err := ComputeBindingHash(base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(want, "sha256:") || len(want) != len("sha256:")+64 {
		t.Fatalf("hash = %q", want)
	}

	changed := base
	changed.BindingID = "another-binding"
	changed.SessionID = "another-session"
	changed.RunID = "another-run"
	changed.CreatedAt = base.CreatedAt.Add(time.Hour)
	changed.ConfigSnapshotRef = "agent-config://another"
	changed.ConfigHash = "sha256:another"
	changed.CapabilitySnapshotRefs = []string{changed.ConfigSnapshotRef + "#capabilities"}
	got, err := ComputeBindingHash(changed)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("excluded fields changed hash: got %q want %q", got, want)
	}
}

func TestBindingHashIncludesExecutionSemantics(t *testing.T) {
	t.Parallel()
	base := validEffectiveBinding()
	want, _ := ComputeBindingHash(base)
	mutations := []struct {
		name string
		fn   func(*EffectiveBinding)
	}{
		{"agent", func(b *EffectiveBinding) { b.AgentID = "other" }},
		{"version", func(b *EffectiveBinding) { b.AgentVersion = "v2" }},
		{"mode", func(b *EffectiveBinding) { b.ExecutionMode = executionmode.DeepAgent }},
		{"target", func(b *EffectiveBinding) { b.Target.Hash = "sha256:target" }},
		{"source", func(b *EffectiveBinding) { b.Source = SourceSessionDefault }},
		{"rule", func(b *EffectiveBinding) { b.ControlRuleRef, b.ControlRuleRevision = "rule://1", "1" }},
		{"fallback", func(b *EffectiveBinding) {
			b.Fallback = FallbackFact{Applied: true, From: Selection{AgentID: "old"}, To: Selection{AgentID: b.AgentID}, ReasonCode: CodeAgentDisabled}
		}},
	}
	for _, mutation := range mutations {
		mutation := mutation
		t.Run(mutation.name, func(t *testing.T) {
			t.Parallel()
			changed := base
			mutation.fn(&changed)
			got, err := ComputeBindingHash(changed)
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatal("execution semantic did not change binding hash")
			}
		})
	}
}

func TestEffectiveBindingValidateDetectsTampering(t *testing.T) {
	t.Parallel()
	binding := validEffectiveBinding()
	binding.BindingHash, _ = ComputeBindingHash(binding)
	if err := binding.Validate(); err != nil {
		t.Fatalf("valid binding rejected: %v", err)
	}
	binding.AgentVersion = "tampered"
	if err := binding.Validate(); CodeOf(err) != CodeTargetInvalid && CodeOf(err) != CodeBindingHashMismatch {
		t.Fatalf("tamper code = %q", CodeOf(err))
	}
}

func validEffectiveBinding() EffectiveBinding {
	return EffectiveBinding{
		SchemaVersion:     BindingSchemaVersion,
		BindingID:         "bind-1",
		SessionID:         "session-1",
		RunID:             "run-1",
		AgentID:           "agent-a",
		AgentVersion:      "v1",
		ExecutionMode:     executionmode.SingleAgent,
		Target:            Target{Kind: TargetAgent, Ref: "agent-a", Version: "v1"},
		ConfigSnapshotRef: "agent-config://agent-a/v1/hash",
		ConfigHash:        "sha256:config",
		CapabilitySnapshotRefs: []string{
			"agent-config://agent-a/v1/hash#capabilities",
		},
		Source:    SourceRequestParam,
		CreatedAt: time.Unix(100, 0).UTC(),
	}
}
