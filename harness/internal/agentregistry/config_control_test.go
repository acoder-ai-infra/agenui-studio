package agentregistry

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

func TestAgentConfigControlIsolatesTenantsAndUsesCAS(t *testing.T) {
	db := openConfigControlSQLite(t, filepath.Join(t.TempDir(), "control.db"))
	control := NewAgentConfigControlService(NewSQLAgentConfigControlStore(db))
	ctx := context.Background()

	first := validConfig()
	first.Name = "tenant-a"
	aDraft, err := control.SaveDraft(ctx, SaveAgentConfigDraftRequest{
		TenantID: "tenant-a", Config: first, Actor: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	second := validConfig()
	second.Name = "tenant-b"
	bDraft, err := control.SaveDraft(ctx, SaveAgentConfigDraftRequest{
		TenantID: "tenant-b", Config: second, Actor: "bob",
	})
	if err != nil {
		t.Fatal(err)
	}
	if aDraft.AgentID != bDraft.AgentID || aDraft.ContentHash == bDraft.ContentHash {
		t.Fatalf("same agent id must be isolated with independent content: a=%#v b=%#v", aDraft, bDraft)
	}
	_, aRelease, err := control.PublishDraft(ctx, PublishAgentConfigDraftRequest{
		TenantID: "tenant-a", AgentID: first.AgentID, Environment: ConfigEnvironmentTesting,
		ExpectedDraftRevision: aDraft.Revision, Actor: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, bRelease, err := control.PublishDraft(ctx, PublishAgentConfigDraftRequest{
		TenantID: "tenant-b", AgentID: second.AgentID, Environment: ConfigEnvironmentTesting,
		ExpectedDraftRevision: bDraft.Revision, Actor: "bob",
	})
	if err != nil {
		t.Fatal(err)
	}
	if aRelease.Version != bRelease.Version || aRelease.ContentHash == bRelease.ContentHash {
		t.Fatalf("same version must publish independently by tenant: a=%#v b=%#v", aRelease, bRelease)
	}
	aDrafts, err := control.ListDrafts(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(aDrafts) != 1 || aDrafts[0].Config.Name != "tenant-a" {
		t.Fatalf("tenant-a draft list leaked scope: %#v", aDrafts)
	}

	loadedA, err := control.GetDraft(ctx, "tenant-a", first.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if loadedA.Config.Name != "tenant-a" {
		t.Fatalf("tenant-a read leaked another tenant: %#v", loadedA.Config)
	}
	loadedA.Config.Name = "updated"
	updated, err := control.SaveDraft(ctx, SaveAgentConfigDraftRequest{
		TenantID: "tenant-a", Config: loadedA.Config, ExpectedRevision: loadedA.Revision, Actor: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != loadedA.Revision+1 {
		t.Fatalf("revision = %d, want %d", updated.Revision, loadedA.Revision+1)
	}
	_, err = control.SaveDraft(ctx, SaveAgentConfigDraftRequest{
		TenantID: "tenant-a", Config: loadedA.Config, ExpectedRevision: loadedA.Revision, Actor: "alice",
	})
	if !errors.Is(err, ErrAgentConfigControlConflict) {
		t.Fatalf("stale save error = %v, want CAS conflict", err)
	}
	if _, err := control.GetDraft(ctx, "tenant-c", first.AgentID); !errors.Is(err, ErrAgentConfigControlNotFound) {
		t.Fatalf("cross-tenant read error = %v, want not found", err)
	}
}

func TestAgentConfigControlPublishesImmutableVersionsAndRollsBackRelease(t *testing.T) {
	db := openConfigControlSQLite(t, filepath.Join(t.TempDir(), "control.db"))
	control := NewAgentConfigControlService(NewSQLAgentConfigControlStore(db))
	ctx := context.Background()

	cfg := validConfig()
	draft, err := control.SaveDraft(ctx, SaveAgentConfigDraftRequest{
		TenantID: "tenant-a", Config: cfg, Actor: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	version1, release1, err := control.PublishDraft(ctx, PublishAgentConfigDraftRequest{
		TenantID: "tenant-a", AgentID: cfg.AgentID, Environment: ConfigEnvironmentTesting,
		ExpectedDraftRevision: draft.Revision, ExpectedReleaseRevision: 0, Actor: "alice", Reason: "first publish",
	})
	if err != nil {
		t.Fatal(err)
	}
	if version1.Version != "v1" || release1.Version != "v1" || release1.Revision != 1 {
		t.Fatalf("unexpected first release: version=%#v release=%#v", version1, release1)
	}

	// Retrying the same publish is idempotent even when the caller still has the
	// create revision. This closes the "version committed, response lost" gap.
	retryVersion, retryRelease, err := control.PublishDraft(ctx, PublishAgentConfigDraftRequest{
		TenantID: "tenant-a", AgentID: cfg.AgentID, Environment: ConfigEnvironmentTesting,
		ExpectedDraftRevision: draft.Revision, ExpectedReleaseRevision: 0, Actor: "alice", Reason: "retry",
	})
	if err != nil {
		t.Fatal(err)
	}
	if retryVersion.ContentHash != version1.ContentHash || retryRelease.Revision != release1.Revision {
		t.Fatalf("idempotent retry drifted: version=%#v release=%#v", retryVersion, retryRelease)
	}

	draft.Config.Name = "changed-without-version-bump"
	changed, err := control.SaveDraft(ctx, SaveAgentConfigDraftRequest{
		TenantID: "tenant-a", Config: draft.Config, ExpectedRevision: draft.Revision, Actor: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = control.PublishDraft(ctx, PublishAgentConfigDraftRequest{
		TenantID: "tenant-a", AgentID: cfg.AgentID, Environment: ConfigEnvironmentTesting,
		ExpectedDraftRevision: changed.Revision, ExpectedReleaseRevision: release1.Revision, Actor: "alice",
	})
	if !errors.Is(err, ErrAgentConfigVersionImmutable) {
		t.Fatalf("same version with changed content error = %v, want immutable conflict", err)
	}

	changed.Config.Version = "v2"
	draft2, err := control.SaveDraft(ctx, SaveAgentConfigDraftRequest{
		TenantID: "tenant-a", Config: changed.Config, ExpectedRevision: changed.Revision, Actor: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, release2, err := control.PublishDraft(ctx, PublishAgentConfigDraftRequest{
		TenantID: "tenant-a", AgentID: cfg.AgentID, Environment: ConfigEnvironmentTesting,
		ExpectedDraftRevision: draft2.Revision, ExpectedReleaseRevision: release1.Revision, Actor: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if release2.Version != "v2" || release2.Revision != 2 {
		t.Fatalf("second release = %#v", release2)
	}

	rolledBack, err := control.PromoteVersion(ctx, PromoteAgentConfigVersionRequest{
		TenantID: "tenant-a", AgentID: cfg.AgentID, Version: "v1", Environment: ConfigEnvironmentTesting,
		ExpectedReleaseRevision: release2.Revision, Actor: "alice", Reason: "rollback",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Version != "v1" || rolledBack.Revision != 3 {
		t.Fatalf("rollback release = %#v", rolledBack)
	}
	versions, err := control.ListVersions(ctx, "tenant-a", cfg.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0].Version != "v2" || versions[1].Version != "v1" {
		t.Fatalf("versions = %#v", versions)
	}
	rows, err := db.Query(`SELECT from_version, to_version, release_revision
FROM agent_config_release_events
WHERE tenant_id=? AND environment=? AND agent_id=?
ORDER BY release_revision`, "tenant-a", ConfigEnvironmentTesting, cfg.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := []struct {
		from, to string
		revision int64
	}{{"", "v1", 1}, {"v1", "v2", 2}, {"v2", "v1", 3}}
	var index int
	for rows.Next() {
		if index >= len(want) {
			t.Fatal("unexpected extra release audit event")
		}
		var from, to string
		var revision int64
		if err := rows.Scan(&from, &to, &revision); err != nil {
			t.Fatal(err)
		}
		if from != want[index].from || to != want[index].to || revision != want[index].revision {
			t.Fatalf("audit event %d = %q -> %q @%d, want %#v", index, from, to, revision, want[index])
		}
		index++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if index != len(want) {
		t.Fatalf("release audit events = %d, want %d", index, len(want))
	}
}

func TestAgentConfigControlSQLitePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	db := openConfigControlSQLite(t, path)
	control := NewAgentConfigControlService(NewSQLAgentConfigControlStore(db))
	cfg := validConfig()
	draft, err := control.SaveDraft(context.Background(), SaveAgentConfigDraftRequest{
		TenantID: "tenant-a", Config: cfg, Actor: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := control.PublishDraft(context.Background(), PublishAgentConfigDraftRequest{
		TenantID: "tenant-a", AgentID: cfg.AgentID, Environment: ConfigEnvironmentProduction,
		ExpectedDraftRevision: draft.Revision, Actor: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	reopened.SetMaxOpenConns(1)
	control = NewAgentConfigControlService(NewSQLAgentConfigControlStore(reopened))
	release, err := control.GetRelease(context.Background(), "tenant-a", ConfigEnvironmentProduction, cfg.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if release.Version != "v1" || release.ContentHash == "" {
		t.Fatalf("release after reopen = %#v", release)
	}
}

func TestAgentConfigControlRejectsUntrustedScope(t *testing.T) {
	db := openConfigControlSQLite(t, filepath.Join(t.TempDir(), "control.db"))
	control := NewAgentConfigControlService(NewSQLAgentConfigControlStore(db))
	_, err := control.SaveDraft(context.Background(), SaveAgentConfigDraftRequest{Config: validConfig(), Actor: "alice"})
	if !errors.Is(err, ErrAgentConfigControlInvalid) {
		t.Fatalf("empty tenant error = %v, want invalid scope", err)
	}
}

func TestAgentConfigControlPublishesPreparedRuntimeProjection(t *testing.T) {
	db := openConfigControlSQLite(t, filepath.Join(t.TempDir(), "control.db"))
	control := NewAgentConfigControlService(NewSQLAgentConfigControlStore(db))
	control.SetPreparer(newLegacyTestService())
	cfg := extendedConfig("managed_agent", "v1")
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
	if version.Prepared == nil || version.Prepared.Effective.ConfigSnapshotRef == "" || len(version.Prepared.ConfigSnapshots) == 0 {
		t.Fatalf("published runtime projection is incomplete: %#v", version.Prepared)
	}
	loaded, err := control.GetVersion(context.Background(), "tenant-a", cfg.AgentID, cfg.Version)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Prepared == nil || loaded.Prepared.Effective.ConfigHash != version.Prepared.Effective.ConfigHash {
		t.Fatalf("prepared projection did not round-trip: %#v", loaded.Prepared)
	}
}

func TestAgentConfigControlRejectsMissingAndCyclicSubAgentsAtPublish(t *testing.T) {
	db := openConfigControlSQLite(t, filepath.Join(t.TempDir(), "control.db"))
	control := NewAgentConfigControlService(NewSQLAgentConfigControlStore(db))
	ctx := context.Background()

	a := extendedConfig("agent_a", "v1")
	a.SubAgents = []string{"missing_agent"}
	draft, err := control.SaveDraft(ctx, SaveAgentConfigDraftRequest{TenantID: "tenant-a", Config: a, Actor: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	control.SetPreparer(newLegacyTestService())
	if _, _, err := control.PublishDraft(ctx, PublishAgentConfigDraftRequest{
		TenantID: "tenant-a", AgentID: a.AgentID, Environment: ConfigEnvironmentTesting,
		ExpectedDraftRevision: draft.Revision, Actor: "alice",
	}); !errors.Is(err, ErrAgentConfigControlInvalid) {
		t.Fatalf("missing child error = %v", err)
	}

	// Seed a valid A v1 and B v1 -> A graph through the compatibility path, then
	// verify the managed release gate rejects A v2 -> B because it closes a cycle.
	control.SetPreparer(nil)
	a.SubAgents = nil
	draft, err = control.SaveDraft(ctx, SaveAgentConfigDraftRequest{
		TenantID: "tenant-a", Config: a, ExpectedRevision: draft.Revision, Actor: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, releaseA, err := control.PublishDraft(ctx, PublishAgentConfigDraftRequest{
		TenantID: "tenant-a", AgentID: a.AgentID, Environment: ConfigEnvironmentTesting,
		ExpectedDraftRevision: draft.Revision, Actor: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	b := extendedConfig("agent_b", "v1")
	b.SubAgents = []string{a.AgentID}
	draftB, err := control.SaveDraft(ctx, SaveAgentConfigDraftRequest{TenantID: "tenant-a", Config: b, Actor: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := control.PublishDraft(ctx, PublishAgentConfigDraftRequest{
		TenantID: "tenant-a", AgentID: b.AgentID, Environment: ConfigEnvironmentTesting,
		ExpectedDraftRevision: draftB.Revision, Actor: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	a.Version = "v2"
	a.SubAgents = []string{b.AgentID}
	draft, err = control.SaveDraft(ctx, SaveAgentConfigDraftRequest{
		TenantID: "tenant-a", Config: a, ExpectedRevision: draft.Revision, Actor: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	control.SetPreparer(newLegacyTestService())
	if _, _, err := control.PublishDraft(ctx, PublishAgentConfigDraftRequest{
		TenantID: "tenant-a", AgentID: a.AgentID, Environment: ConfigEnvironmentTesting,
		ExpectedDraftRevision: draft.Revision, ExpectedReleaseRevision: releaseA.Revision, Actor: "alice",
	}); !errors.Is(err, ErrAgentConfigControlInvalid) {
		t.Fatalf("cycle error = %v", err)
	}
}

func TestAgentConfigControlFreezesSubAgentVersions(t *testing.T) {
	db := openConfigControlSQLite(t, filepath.Join(t.TempDir(), "relations.db"))
	control := NewAgentConfigControlService(NewSQLAgentConfigControlStore(db))
	control.SetPreparer(newLegacyTestService())
	ctx := context.Background()
	child := extendedConfig("child_agent", "v3")
	childDraft, err := control.SaveDraft(ctx, SaveAgentConfigDraftRequest{TenantID: "tenant-a", Config: child, Actor: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := control.PublishDraft(ctx, PublishAgentConfigDraftRequest{TenantID: "tenant-a", AgentID: child.AgentID, Environment: ConfigEnvironmentTesting, ExpectedDraftRevision: childDraft.Revision, Actor: "alice"}); err != nil {
		t.Fatal(err)
	}
	parent := extendedConfig("parent_agent", "v1")
	parent.SubAgents = []string{child.AgentID}
	parentDraft, err := control.SaveDraft(ctx, SaveAgentConfigDraftRequest{TenantID: "tenant-a", Config: parent, Actor: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	version, _, err := control.PublishDraft(ctx, PublishAgentConfigDraftRequest{TenantID: "tenant-a", AgentID: parent.AgentID, Environment: ConfigEnvironmentTesting, ExpectedDraftRevision: parentDraft.Revision, Actor: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if version.Prepared == nil || version.Prepared.SubAgentVersions[child.AgentID] != child.Version {
		t.Fatalf("sub-agent version was not frozen: %#v", version.Prepared)
	}
}

func TestAgentConfigControlAllowsStaticSubAgentDuringMigration(t *testing.T) {
	db := openConfigControlSQLite(t, filepath.Join(t.TempDir(), "static-relations.db"))
	base := newLegacyTestService()
	staticChild := extendedConfig("static_child", "v4")
	if _, err := base.RegisterAgent(context.Background(), staticChild); err != nil {
		t.Fatal(err)
	}
	control := NewAgentConfigControlService(NewSQLAgentConfigControlStore(db))
	control.SetPreparer(base)
	parent := extendedConfig("managed_parent", "v1")
	parent.SubAgents = []string{staticChild.AgentID}
	draft, err := control.SaveDraft(context.Background(), SaveAgentConfigDraftRequest{
		TenantID: "tenant-a", Config: parent, Actor: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	version, _, err := control.PublishDraft(context.Background(), PublishAgentConfigDraftRequest{
		TenantID: "tenant-a", AgentID: parent.AgentID, Environment: ConfigEnvironmentTesting,
		ExpectedDraftRevision: draft.Revision, Actor: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if version.Prepared == nil || version.Prepared.SubAgentVersions[staticChild.AgentID] != staticChild.Version {
		t.Fatalf("static sub-agent version was not frozen: %#v", version.Prepared)
	}
}

func TestAgentConfigControlConcurrentDraftCASHasSingleWinner(t *testing.T) {
	db := openConfigControlSQLite(t, filepath.Join(t.TempDir(), "control.db"))
	control := NewAgentConfigControlService(NewSQLAgentConfigControlStore(db))
	ctx := context.Background()
	draft, err := control.SaveDraft(ctx, SaveAgentConfigDraftRequest{
		TenantID: "tenant-a", Config: validConfig(), Actor: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}

	const writers = 20
	start := make(chan struct{})
	results := make(chan error, writers)
	var wait sync.WaitGroup
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			cfg := draft.Config
			cfg.Name = "writer-" + string(rune('a'+index))
			_, err := control.SaveDraft(ctx, SaveAgentConfigDraftRequest{
				TenantID: "tenant-a", Config: cfg, ExpectedRevision: draft.Revision, Actor: "alice",
			})
			results <- err
		}(index)
	}
	close(start)
	wait.Wait()
	close(results)

	var succeeded, conflicted int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrAgentConfigControlConflict):
			conflicted++
		default:
			t.Fatalf("concurrent save error = %v", err)
		}
	}
	if succeeded != 1 || conflicted != writers-1 {
		t.Fatalf("concurrent saves: succeeded=%d conflicted=%d", succeeded, conflicted)
	}
}

func openConfigControlSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := ApplySQLiteBaseline(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := ApplySQLiteConfigControl(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}
