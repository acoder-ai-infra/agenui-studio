package agentregistry

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestReleaseRegistryResolvesTenantReleaseAndFrozenSnapshot(t *testing.T) {
	db := openConfigControlSQLite(t, filepath.Join(t.TempDir(), "release.db"))
	store := NewSQLAgentConfigControlStore(db)
	base := newLegacyTestService()
	control := NewAgentConfigControlService(store)
	control.SetPreparer(base)
	cfg := extendedConfig("tenant_agent", "v9")
	draft, err := control.SaveDraft(context.Background(), SaveAgentConfigDraftRequest{
		TenantID: "tenant-a", Config: cfg, Actor: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	version, _, err := control.PublishDraft(context.Background(), PublishAgentConfigDraftRequest{
		TenantID: "tenant-a", AgentID: cfg.AgentID, Environment: ConfigEnvironmentTesting,
		ExpectedDraftRevision: draft.Revision, Actor: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewReleaseRegistry(base, store, ConfigEnvironmentTesting)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := registry.ResolveEffectiveConfig(context.Background(), ResolveRequest{AgentID: cfg.AgentID, Tenant: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	if effective.Definition.Version != "v9" || effective.ConfigHash != version.Prepared.Effective.ConfigHash {
		t.Fatalf("tenant release was not selected: %#v", effective)
	}
	frozen, err := registry.GetConfigSnapshotForTenant(context.Background(), "tenant-a", effective.ConfigSnapshotRef)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.ConfigHash != effective.ConfigHash {
		t.Fatalf("frozen projection drifted: %#v", frozen)
	}
}

func TestReleaseRegistryRejectsDisabledReleasedVersion(t *testing.T) {
	db := openConfigControlSQLite(t, filepath.Join(t.TempDir(), "disabled.db"))
	store := NewSQLAgentConfigControlStore(db)
	base := newLegacyTestService()
	control := NewAgentConfigControlService(store)
	control.SetPreparer(base)
	cfg := extendedConfig("disabled_agent", "v1")
	cfg.Status = AgentStatusDisabled
	draft, err := control.SaveDraft(context.Background(), SaveAgentConfigDraftRequest{
		TenantID: "tenant-a", Config: cfg, Actor: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := control.PublishDraft(context.Background(), PublishAgentConfigDraftRequest{
		TenantID: "tenant-a", AgentID: cfg.AgentID, Environment: ConfigEnvironmentTesting,
		ExpectedDraftRevision: draft.Revision, Actor: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	registry, err := NewReleaseRegistry(base, store, ConfigEnvironmentTesting)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ResolveEffectiveConfig(context.Background(), ResolveRequest{AgentID: cfg.AgentID, Tenant: "tenant-a"}); !errors.Is(err, ErrAgentDisabled) {
		t.Fatalf("expected disabled released version to be rejected, got err=%v", err)
	}
	// IncludeDisabled bypasses the gate (parity with the base registry path).
	if _, err := registry.ResolveEffectiveConfig(context.Background(), ResolveRequest{AgentID: cfg.AgentID, Tenant: "tenant-a", IncludeDisabled: true}); err != nil {
		t.Fatalf("IncludeDisabled should resolve a disabled release, got err=%v", err)
	}
}

func TestReleaseRegistryFallsBackToStaticExplicitVersion(t *testing.T) {
	db := openConfigControlSQLite(t, filepath.Join(t.TempDir(), "fallback.db"))
	store := NewSQLAgentConfigControlStore(db)
	base := newLegacyTestService()
	static := directAgentConfig("static_agent", "v1")
	if _, err := base.RegisterAgent(context.Background(), static); err != nil {
		t.Fatal(err)
	}
	registry, err := NewReleaseRegistry(base, store, ConfigEnvironmentTesting)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := registry.ResolveEffectiveConfig(context.Background(), ResolveRequest{
		Tenant: "tenant-a", AgentID: static.AgentID, Version: static.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if effective.Definition.AgentID != static.AgentID || effective.Definition.Version != static.Version {
		t.Fatalf("static explicit version did not survive the managed overlay: %#v", effective)
	}
}

func TestReleaseRegistryFallsBackToStaticParentGatewaySnapshot(t *testing.T) {
	ctx := context.Background()
	db := openConfigControlSQLite(t, filepath.Join(t.TempDir(), "gateway-fallback.db"))
	store := NewSQLAgentConfigControlStore(db)
	base := newLegacyTestService()
	target := directAgentConfig("static_child", "v1")
	target.Gateway = validGatewayTargetConfig()
	parent := directAgentConfig("static_parent", "v1")
	parent.SubAgents = []string{target.AgentID}
	for _, cfg := range []AgentConfig{target, parent} {
		if _, err := base.RegisterAgent(ctx, cfg); err != nil {
			t.Fatal(err)
		}
	}
	parentEffective, err := base.ResolveEffectiveConfig(ctx, ResolveRequest{AgentID: parent.AgentID, Version: parent.Version})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewReleaseRegistry(base, store, ConfigEnvironmentTesting)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.ResolveGatewayTarget(ctx, ResolveGatewayTargetRequest{
		TenantID: "tenant-a", ParentAgentID: parent.AgentID, ParentAgentVersion: parent.Version,
		ParentConfigSnapshotRef: parentEffective.ConfigSnapshotRef, ParentConfigHash: parentEffective.ConfigHash,
		SubAgentRef: target.AgentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Effective.Definition.AgentID != target.AgentID || resolved.Effective.Definition.Version != target.Version {
		t.Fatalf("static gateway target did not survive the managed overlay: %#v", resolved)
	}
}
