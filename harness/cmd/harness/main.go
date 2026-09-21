package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/app"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

func main() {
	os.Exit(run())
}

func run() (exitCode int) {
	if len(os.Args) > 1 {
		if os.Args[1] == "migrate" {
			return runMigrate(os.Args[2:], os.Getenv, os.Stdout, os.Stderr)
		}
		return 2
	}
	environment := getenv("HARNESS_ENV", "local")
	configPath := getenv("HARNESS_CONFIG", app.DefaultHarnessConfigPath(environment))
	harnessCfg, err := app.LoadHarnessConfig(configPath)
	if err != nil {
		panic(err)
	}
	if err := app.ValidateSelectedEnvironment(environment, harnessCfg); err != nil {
		panic(err)
	}
	if err := app.EnsureRuntimeDirectories(harnessCfg); err != nil {
		panic(err)
	}
	logger, err := app.NewConfiguredLogger(harnessCfg)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := logger.Close(); err != nil && exitCode == 0 {
			exitCode = 1
		}
	}()

	// Retain recent structured logs in memory so the debug console can pull them
	// by trace_id/run_id (GET /api/v1/debug/logs). Wraps zap transparently.
	appLogger := observability.NewRingLogger(logger, harnessCfg.Logging.RecentBufferItems)

	tracer := observability.NewOTelTracer(harnessCfg.Service.Name)

	appLogger.Info(context.Background(), "component config loading", observability.String("config_path", configPath))
	componentCfg, err := app.LoadComponentConfigs(harnessCfg)
	if err != nil {
		appLogger.Error(context.Background(), "component config load failed; refusing to start", err)
		return 1
	}
	appLogger.Info(context.Background(), "application build starting")
	application, err := app.Build(componentCfg.Models, componentCfg.Storage, componentCfg.Auth, componentCfg.Redis, appLogger, tracer,
		app.WithHarnessConfig(harnessCfg), app.WithArtifactConfig(componentCfg.Artifact),
		app.WithCapabilityCatalogs(componentCfg.Skills, componentCfg.Tools, componentCfg.MCP))
	if err != nil {
		appLogger.Error(context.Background(), "app build failed; refusing to start (fail closed)", err)
		return 1
	}
	appLogger.Info(context.Background(), "application build complete")
	defer func() {
		if err := application.Close(); err != nil {
			appLogger.Error(context.Background(), "app resource cleanup failed", err)
			if exitCode == 0 {
				exitCode = 1
			}
		}
	}()
	appLogger.Info(context.Background(), "model providers registered", observability.Any("providers", application.Providers))

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	// Mount the app at root so both /api/v1/* and /demo are reachable; the more
	// specific /healthz above still wins (Go 1.22 ServeMux precedence).
	mux.Handle("/", application.Handler)

	handler := observability.HTTPMiddleware(observability.HTTPMiddlewareConfig{
		ServiceName: harnessCfg.Service.Name,
		Logger:      appLogger,
		Tracer:      tracer,
	})(mux)

	server := &http.Server{
		Addr:              harnessCfg.Service.Address,
		Handler:           handler,
		ReadHeaderTimeout: time.Duration(harnessCfg.Service.ReadHeaderTimeoutMS) * time.Millisecond,
	}

	errCh := make(chan error, 1)
	go func() {
		appLogger.Info(context.Background(), "server starting", observability.String("addr", server.Addr))
		errCh <- server.ListenAndServe()
	}()

	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stopCh)

	select {
	case sig := <-stopCh:
		appLogger.Info(context.Background(), "shutdown signal received", observability.String("signal", sig.String()))
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			appLogger.Error(context.Background(), "server failed", err)
			return 1
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(harnessCfg.Service.ShutdownTimeoutSeconds)*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		appLogger.Error(context.Background(), "server shutdown failed", err)
		return 1
	}
	appLogger.Info(context.Background(), "server stopped")
	return 0
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
