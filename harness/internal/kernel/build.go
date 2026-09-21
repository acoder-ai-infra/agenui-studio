package kernel

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrComposerNotRegistered is returned by Build when no composer has been
// registered by an implementing package (currently internal/app registers via
// an init side-effect). It signals a build misconfiguration, not a runtime
// error, and is unrecoverable from Kernel callers.
var ErrComposerNotRegistered = errors.New("kernel: no composer registered; import internal/app once")

// Composer is the composition callback registered by internal/app during
// package initialization. Kernel.Build delegates to it so the kernel package
// itself does not have to import the application's infrastructure wiring.
//
// The composer receives already-validated KernelOptions (config paths resolved,
// resource registrations enumerated) and returns a fully-wired *Kernel or an
// error. It must never block; asynchronous readiness happens later.
type Composer func(ctx context.Context, opts KernelOptions) (*Kernel, error)

var (
	composerMu       sync.RWMutex
	registeredCompos Composer
)

// RegisterComposer installs the composition callback. It is safe to call from
// init(); subsequent calls REPLACE the composer so tests can inject a fake for
// isolation.
func RegisterComposer(c Composer) {
	composerMu.Lock()
	defer composerMu.Unlock()
	registeredCompos = c
}

// Build is the composition entry point. It looks up the registered composer
// and delegates to it. Callers (harness.Build, cmd/harness, internal/app.Build)
// must ensure the composer is registered before invoking Build.
//
// Build performs no work itself beyond dispatching; observability, config
// validation and resource lifecycle live inside the composer's implementation.
func Build(ctx context.Context, opts KernelOptions) (*Kernel, error) {
	composerMu.RLock()
	composer := registeredCompos
	composerMu.RUnlock()
	if composer == nil {
		return nil, ErrComposerNotRegistered
	}
	if ctx == nil {
		return nil, fmt.Errorf("kernel: Build requires a non-nil context")
	}
	return composer(ctx, opts)
}
