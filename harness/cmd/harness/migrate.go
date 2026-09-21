package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/app"
)

func parseMigrateAction(args []string) (string, error) {
	if len(args) == 0 {
		return "up", nil
	}
	if len(args) != 1 {
		return "", fmt.Errorf("usage: harness migrate [up|status]")
	}
	switch args[0] {
	case "up", "status":
		return args[0], nil
	default:
		return "", fmt.Errorf("unknown migration action %q; use up or status", args[0])
	}
}

func runMigrate(args []string, environmentValue func(string) string, stdout, stderr io.Writer) int {
	action, err := parseMigrateAction(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	environment := strings.TrimSpace(environmentValue("HARNESS_ENV"))
	if environment == "" {
		environment = "local"
	}
	configPath := strings.TrimSpace(environmentValue("HARNESS_CONFIG"))
	if configPath == "" {
		configPath = app.DefaultHarnessConfigPath(environment)
	}
	harnessConfig, err := app.LoadHarnessConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "migration failed: %v\n", err)
		return 1
	}
	if err := app.ValidateSelectedEnvironment(environment, harnessConfig); err != nil {
		fmt.Fprintf(stderr, "migration failed: %v\n", err)
		return 1
	}
	storageConfig, err := app.LoadStorageConfig(harnessConfig)
	if err != nil {
		fmt.Fprintf(stderr, "migration failed: %v\n", err)
		return 1
	}

	var result app.StorageSchemaResult
	if action == "status" {
		result, err = app.CheckStorageSchema(context.Background(), storageConfig, !harnessConfig.Environment.IsLocal())
	} else {
		result, err = app.MigrateStorage(context.Background(), storageConfig, !harnessConfig.Environment.IsLocal())
	}
	if err != nil {
		fmt.Fprintf(stderr, "migration failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "storage schema %s complete: backend=%s ready=%t noop=%t\n", result.Action, result.Backend, result.Ready, result.Noop)
	return 0
}
