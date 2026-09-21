package kernel

import (
	"context"
	"errors"
)

// MigrationAction names the supported schema operations.
type MigrationAction string

const (
	MigrationPlan   MigrationAction = "plan"
	MigrationUp     MigrationAction = "up"
	MigrationStatus MigrationAction = "status"
)

// MigrationRequest is what MigrationFunc receives.
type MigrationRequest struct {
	ConfigPath  string
	Environment string
	Action      MigrationAction
}

// MigrationResult echoes the outcome. Backend / Ready / Noop mirror the
// internal/app.StorageSchemaResult surface without importing app.
type MigrationResult struct {
	Backend string
	Action  MigrationAction
	Ready   bool
	Noop    bool
}

// MigrationFunc is the concrete migration implementation registered by
// internal/app during package initialization. It mirrors the Composer
// registration pattern so migration can proceed without booting the full
// Composition Root.
type MigrationFunc func(ctx context.Context, req MigrationRequest) (MigrationResult, error)

// ErrMigrationFuncNotRegistered mirrors ErrComposerNotRegistered for the
// migration path.
var ErrMigrationFuncNotRegistered = errors.New("kernel: no migration func registered; import internal/app once")

var registeredMigration MigrationFunc

// RegisterMigrationFunc installs the migration implementation. Callers must
// invoke it from an init() hook so the registration happens before any SDK
// entry point runs.
func RegisterMigrationFunc(fn MigrationFunc) {
	registeredMigration = fn
}

// RunMigration dispatches to the registered MigrationFunc.
func RunMigration(ctx context.Context, req MigrationRequest) (MigrationResult, error) {
	if registeredMigration == nil {
		return MigrationResult{}, ErrMigrationFuncNotRegistered
	}
	return registeredMigration(ctx, req)
}
