// Package kernel is the shared Composition Root for the Harness.
//
// Both the hosted service entry (cmd/harness) and the embedded SDK facade
// (harness) build their runtime through this package. It owns the wiring of
// stores, model gateway, agent registry, runtime, orchestrator, dispatcher,
// event broker, hot buffer, snapshots, control service and control ticket
// codec. It intentionally does not construct HTTP routers; the hosted service
// composes an http.Handler on top of the kernel, the SDK exposes the same
// composition through the public harness.Engine facade.
//
// The kernel package sits in internal/ so no external module can bypass the
// public SDK; only harness's own harness/ and internal/app/ packages import it.
// This is the SDK design's "one kernel, two entries" contract (see
// the public SDK contract, §12).
package kernel
