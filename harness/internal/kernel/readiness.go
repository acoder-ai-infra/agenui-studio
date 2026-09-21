package kernel

import (
	"context"
	"time"
)

// ReadinessProbe is one liveness check the kernel performs when Readiness is
// requested. Composers register probes for their subsystems (SQL pool ping,
// Redis ping, Artifact object-store head request, etc.).
type ReadinessProbe struct {
	Name string
	// Probe returns nil when the subsystem is healthy. A non-nil error is
	// surfaced in the ReadinessSnapshot as the failure reason for that probe.
	Probe func(ctx context.Context) error
}

// ReadinessSnapshot is the kernel-side snapshot returned by Kernel.CheckReadiness.
// It is intentionally minimal; the SDK layer projects it into the richer
// harness.ReadinessReport by adding extension / schema metadata.
type ReadinessSnapshot struct {
	Ready     bool
	Failures  []ReadinessFailure
	CheckedAt time.Time
}

// ReadinessFailure captures one probe's failure detail.
type ReadinessFailure struct {
	Name   string
	Reason string
}

// CheckReadiness runs every probe under ctx and aggregates the results. The
// first non-nil failure marks the snapshot not-Ready; every failure is
// enumerated so operators can prioritize.
func CheckReadiness(ctx context.Context, probes []ReadinessProbe) ReadinessSnapshot {
	snap := ReadinessSnapshot{Ready: true, CheckedAt: time.Now()}
	for _, p := range probes {
		if p.Probe == nil {
			continue
		}
		if err := p.Probe(ctx); err != nil {
			snap.Ready = false
			snap.Failures = append(snap.Failures, ReadinessFailure{Name: p.Name, Reason: err.Error()})
		}
	}
	return snap
}
