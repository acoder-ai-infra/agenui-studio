package harness

import (
	"context"
	"time"
)

// DoctorReport captures the outcome of harness.Doctor. It is the programmatic
// equivalent of the cmd/harness-doctor CLI's "check" output.
type DoctorReport struct {
	// Build is the frozen BuildReport captured after Engine.Build.
	Build BuildReport
	// Readiness is the live ReadinessReport probed against the built kernel.
	Readiness ReadinessReport
	// PassedAt is when the doctor finished. Zero when the doctor never ran to
	// completion (Build error or Readiness error).
	PassedAt time.Time
	// Failures enumerates the reasons the doctor is unhappy. Empty when Ready.
	Failures []string
}

// Doctor is the programmatic diagnostic entry point. It builds an Engine,
// probes Readiness, closes the Engine, and returns a DoctorReport summarising
// what it saw. Doctor is safe to run against production configs; it never
// starts a Run and never mutates state beyond what Build itself does.
//
// Callers use Doctor when they want to fail startup fast if the environment is
// misconfigured. The cmd/harness-doctor CLI is a thin wrapper around this
// function that adds JSON encoding + exit codes.
func Doctor(ctx context.Context, opts ...Option) (DoctorReport, error) {
	engine, buildReport, err := Build(ctx, opts...)
	if err != nil {
		return DoctorReport{
			Failures: []string{"build failed: " + err.Error()},
		}, err
	}
	readiness, readinessErr := engine.Readiness(ctx)
	closeErr := engine.Close(ctx)
	report := DoctorReport{
		Build:      buildReport,
		Readiness:  readiness,
		PassedAt:   time.Now(),
	}
	if readinessErr != nil {
		report.Failures = append(report.Failures, "readiness failed: "+readinessErr.Error())
	}
	if !readiness.Ready {
		report.Failures = append(report.Failures, readiness.Reasons...)
	}
	for _, degraded := range buildReport.Degraded {
		report.Failures = append(report.Failures, "degraded: "+degraded)
	}
	if closeErr != nil {
		report.Failures = append(report.Failures, "close warning: "+closeErr.Error())
	}
	if readinessErr != nil {
		return report, readinessErr
	}
	return report, nil
}
