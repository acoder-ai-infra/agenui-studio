package kernel_test

import (
	"context"
	"errors"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
)

// TestBuildErrorWithoutComposer verifies Build fails closed when no composer
// has been registered. This is the guard that would fire if internal/app is
// not imported (its init() is the sole registration site today).
func TestBuildErrorWithoutComposer(t *testing.T) {
	// Save and restore the global registration to avoid affecting other tests.
	prev := swapComposer(nil)
	defer func() { swapComposer(prev) }()

	if _, err := kernel.Build(context.Background(), kernel.KernelOptions{}); !errors.Is(err, kernel.ErrComposerNotRegistered) {
		t.Fatalf("want ErrComposerNotRegistered, got %v", err)
	}
}

// TestBuildDelegatesToComposer verifies Build calls whatever composer is
// registered.
func TestBuildDelegatesToComposer(t *testing.T) {
	sentinel := &kernel.Kernel{DefaultAgentID: "fake"}
	prev := swapComposer(func(ctx context.Context, opts kernel.KernelOptions) (*kernel.Kernel, error) {
		if opts.ConfigPath != "expected.yaml" {
			t.Fatalf("composer received unexpected ConfigPath: %q", opts.ConfigPath)
		}
		return sentinel, nil
	})
	defer func() { swapComposer(prev) }()

	got, err := kernel.Build(context.Background(), kernel.KernelOptions{ConfigPath: "expected.yaml"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got != sentinel {
		t.Fatal("Build did not return the composer's Kernel instance")
	}
}

func TestBuildRequiresContext(t *testing.T) {
	prev := swapComposer(func(context.Context, kernel.KernelOptions) (*kernel.Kernel, error) {
		return &kernel.Kernel{}, nil
	})
	defer func() { swapComposer(prev) }()

	//nolint:staticcheck // Intentionally nil to exercise the guard.
	if _, err := kernel.Build(nil, kernel.KernelOptions{}); err == nil {
		t.Fatal("Build should reject nil context")
	}
}

func TestMigrationErrorWithoutRegistration(t *testing.T) {
	prevMig := swapMigration(nil)
	defer func() { swapMigration(prevMig) }()

	_, err := kernel.RunMigration(context.Background(), kernel.MigrationRequest{
		ConfigPath: "x.yaml", Action: kernel.MigrationPlan,
	})
	if !errors.Is(err, kernel.ErrMigrationFuncNotRegistered) {
		t.Fatalf("want ErrMigrationFuncNotRegistered, got %v", err)
	}
}

func TestExtensionCatalogDeterministicOrdering(t *testing.T) {
	entries := []kernel.ExtensionEntry{
		{ID: "b", Kind: kernel.ExtToolProvider, Order: 2},
		{ID: "a", Kind: kernel.ExtIdentityResolver, Order: 1},
		{ID: "c", Kind: kernel.ExtToolProvider, Order: 1},
	}
	cat, err := kernel.NewExtensionCatalog(entries)
	if err != nil {
		t.Fatalf("build catalog: %v", err)
	}
	out := cat.Entries()
	if out[0].ID != "a" || out[1].ID != "c" || out[2].ID != "b" {
		t.Fatalf("unexpected order: %v", []string{out[0].ID, out[1].ID, out[2].ID})
	}
}

func TestExtensionCatalogRejectsDuplicates(t *testing.T) {
	_, err := kernel.NewExtensionCatalog([]kernel.ExtensionEntry{
		{ID: "same", Kind: kernel.ExtIdentityResolver},
		{ID: "same", Kind: kernel.ExtIdentityResolver},
	})
	if err == nil {
		t.Fatal("duplicate (Kind, ID) must be rejected")
	}
}

// swapComposer replaces the registered composer with next and returns the
// previous value so the caller can restore state. Because kernel.RegisterComposer
// has no getter, we track the current value in a package-scoped var owned by
// the test binary. Tests must not call kernel.RegisterComposer directly.
func swapComposer(next kernel.Composer) kernel.Composer {
	prev := currentComposer
	currentComposer = next
	kernel.RegisterComposer(next)
	return prev
}

var currentComposer kernel.Composer

func swapMigration(next kernel.MigrationFunc) kernel.MigrationFunc {
	prev := currentMigration
	currentMigration = next
	kernel.RegisterMigrationFunc(next)
	return prev
}

var currentMigration kernel.MigrationFunc
