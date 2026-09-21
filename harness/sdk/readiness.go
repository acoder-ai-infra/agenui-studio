package harness

// This file re-declares Readiness-related SDK types in a dedicated
// readiness.go per the plan's directory layout. The concrete type definitions
// live in view.go alongside BuildReport / RunView because Readiness and Build
// share vocabulary (RuntimeInfo / ExtensionInfo). Splitting the type
// declarations would create a maintenance burden without benefit; this file
// exists as the canonical "documentation entry point" for the Readiness
// contract.
//
// See view.go for:
//
//   type ReadinessReport struct { ... }
//
// See internal_adapter.go for the Engine.Readiness implementation.
//
// Readiness invariants (the public SDK contract):
//
//   - Callers may invoke Engine.Readiness() at any time; the kernel probes
//     each subsystem synchronously bounded by ctx.
//   - Ready == true only when EVERY required subsystem is healthy AND no
//     capability is marked Unsupported. Degraded subsystems keep Ready = true
//     but populate BuildReport.Degraded so operators can prioritize repairs.
//   - Extensions and Runtimes fields reflect the FROZEN Build-time state
//     with live Available / Fingerprint values overwritten each Readiness call.

// ReadinessCheck is a caller-facing helper: given a live ReadinessReport,
// returns true iff the SDK is safe to accept new Start calls. Callers wire it
// into HTTP /healthz handlers or Kubernetes liveness probes without importing
// internal packages.
func ReadinessCheck(report ReadinessReport) bool {
	if !report.Ready {
		return false
	}
	for _, rt := range report.Runtimes {
		if !rt.Available {
			return false
		}
	}
	return true
}
