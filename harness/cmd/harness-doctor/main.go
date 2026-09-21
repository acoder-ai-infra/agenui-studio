// Command harness-doctor is a stand-alone CLI for verifying an Harness Harness
// deployment: it checks the harness config, runs schema plan / migrations, and
// reports readiness.
//
// Exit codes follow the convention documented in
// the public SDK contract:
//
//	0  every check passed.
//	1  at least one subsystem is Degraded (still usable but flagged).
//	2  at least one required check failed; the Harness would not Build.
//
// Usage:
//
//	harness-doctor plan     - read-only schema plan.
//	harness-doctor up       - apply schema migrations.
//	harness-doctor status   - alias for plan.
//	harness-doctor check    - Build + Readiness + Close (heaviest gate).
//
// Global flags:
//
//	--config <path>         - override harness.yaml path.
//	--env <name>            - override environment.
//	--json                  - emit machine-readable JSON on stdout.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		writeErr(stderr, usage())
		return 2
	}
	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "override harness.yaml path")
	envName := fs.String("env", "", "override environment (local|testing|staging|production)")
	asJSON := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	opts := harness.MigrationOptions{
		ConfigPath:  *configPath,
		Environment: *envName,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	switch command {
	case "plan", "status":
		report, err := harness.MigrationPlan(ctx, opts)
		return emitMigration(stdout, stderr, "plan", report, err, *asJSON)
	case "up":
		report, err := harness.RunMigration(ctx, opts)
		return emitMigration(stdout, stderr, "up", report, err, *asJSON)
	case "check":
		return checkHarness(ctx, stdout, stderr, opts, *asJSON)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage())
		return 0
	default:
		writeErr(stderr, fmt.Sprintf("unknown command %q\n%s", command, usage()))
		return 2
	}
}

func emitMigration(stdout, stderr *os.File, label string, report harness.MigrationReport, err error, asJSON bool) int {
	if err != nil {
		writeErr(stderr, fmt.Sprintf("%s failed: %v\n", label, err))
		return 2
	}
	if asJSON {
		return writeJSON(stdout, report)
	}
	fmt.Fprintf(stdout, "backend=%s action=%s ready=%t noop=%t\n", report.Backend, report.Action, report.Ready, report.Noop)
	if !report.Ready {
		return 1
	}
	return 0
}

func checkHarness(ctx context.Context, stdout, stderr *os.File, opts harness.MigrationOptions, asJSON bool) int {
	// A full Build proves every subsystem wires; Close releases resources so
	// the doctor never leaves a hosted deployment in a half-open state.
	engine, buildReport, err := harness.Build(ctx,
		harness.WithConfigPath(opts.ConfigPath),
		harness.WithEnvironment(opts.Environment),
	)
	if err != nil {
		writeErr(stderr, fmt.Sprintf("build failed: %v\n", err))
		return 2
	}
	readiness, err := engine.Readiness(ctx)
	closeErr := engine.Close(ctx)
	if err != nil {
		writeErr(stderr, fmt.Sprintf("readiness failed: %v\n", err))
		return 2
	}
	if closeErr != nil {
		writeErr(stderr, fmt.Sprintf("close warning: %v\n", closeErr))
	}
	if asJSON {
		return writeJSON(stdout, checkPayload{Build: buildReport, Readiness: readiness})
	}
	fmt.Fprintf(stdout, "sdk_version=%s environment=%s ready=%t degraded=%d unsupported=%d\n",
		buildReport.SDKVersion, buildReport.Environment, readiness.Ready, len(buildReport.Degraded), len(buildReport.Unsupported))
	for _, reason := range readiness.Reasons {
		fmt.Fprintf(stdout, "  reason: %s\n", reason)
	}
	if !readiness.Ready {
		return 2
	}
	if len(buildReport.Degraded) > 0 {
		return 1
	}
	return 0
}

type checkPayload struct {
	Build     harness.BuildReport     `json:"build"`
	Readiness harness.ReadinessReport `json:"readiness"`
}

func writeJSON(w *os.File, v any) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "encode json: %v\n", err)
		return 2
	}
	return 0
}

func writeErr(w *os.File, msg string) {
	_, _ = fmt.Fprint(w, msg)
}

func usage() string {
	return `harness-doctor - Harness Harness diagnostic CLI

Usage:
  harness-doctor <command> [flags]

Commands:
  plan      read-only schema plan
  up        apply schema migrations
  status    alias for plan
  check     full Build + Readiness gate
  help      show this help

Flags:
  --config <path>   override harness.yaml path
  --env <name>      override environment (local|testing|staging|production)
  --json            emit JSON output

Exit codes:
  0  all checks passed
  1  degraded (still usable, flagged)
  2  failure (Harness would not Build)
`
}
