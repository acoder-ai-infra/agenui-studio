package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge"
	dkRuntime "github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge/runtime"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/exportpackage"
	inputextension "github.com/AGenUI/agenui-studio/agenui-agent/internal/extensions/input"
	businesstool "github.com/AGenUI/agenui-studio/agenui-agent/internal/extensions/tool"
	stepartifact "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/artifact"
	bindingoperator "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/binding/operator"
	platformoperator "github.com/AGenUI/agenui-studio/agenui-agent/internal/operator"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/orchestration"
	httptransport "github.com/AGenUI/agenui-studio/agenui-agent/internal/transport/http"
	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/agenui/renderercatalog"
	"github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

const defaultCloseTimeout = 10 * time.Second

type MigrationMode string

const (
	MigrationDisabled MigrationMode = "disabled"
	MigrationCheck    MigrationMode = "check"
	MigrationUp       MigrationMode = "up"
)

type EngineFactory func(
	context.Context,
	...harness.Option,
) (harness.Engine, harness.BuildReport, error)

type MigrationRunner func(
	context.Context,
	harness.MigrationOptions,
	MigrationMode,
) (harness.MigrationReport, error)

type Config struct {
	HarnessConfigPath         string
	Environment               string
	MigrationMode             MigrationMode
	AgentID                   string
	AgentVersion              string
	MaxRequestBytes           int64
	HeartbeatInterval         time.Duration
	CloseTimeout              time.Duration
	DesignKnowledgeMaxDocs    int
	DesignKnowledgeMaxBytes   int
	DesignKnowledgeReceiptKey []byte
	RendererCatalogRoot       string
	ExportDataSourceBaseURL   string
}

type Dependencies struct {
	ResolvePrincipal httptransport.PrincipalResolver
	ExtensionOptions []harness.Option
	// OutputValidators are optional application-owned Harness extensions. Studio
	// ships no policy validator; a deployment may register one here and bind its
	// ID in its own Harness agent configuration.
	OutputValidators        []extension.OutputValidator
	EngineFactory           EngineFactory
	MigrationRunner         MigrationRunner
	DesignKnowledge         *designknowledge.Repository
	DesignKnowledgeProvider dkRuntime.RepositoryProvider
	// BindingDependencyTools is the optional governed execution provider for
	// one selected operator_id plus parameters. Operator discovery uses the
	// staged list_developer_operators MCP contract; both discovery and execution
	// remain disabled by default. Production injection must include an
	// authoritative durable child-to-parent Run resolver, and Agent catalogs
	// must opt in by exact Tool/server name, version and ACL.
	BindingDependencyTools *businesstool.BindingDependenciesProvider
	// OperatorRuntimePort is the only production business dependency required
	// to activate operator execution. When supplied, Build constructs the
	// governed execution service, exact child-to-parent resolver and Tool
	// provider; callers must not also inject BindingDependencyTools.
	OperatorRuntimePort bindingoperator.OperatorRuntimePort
	OperatorDetails     platformoperator.DetailClient
}

type App struct {
	Engine            harness.Engine
	NativeHarnessHTTP http.Handler
	Handler           *httptransport.Handler
	Report            harness.BuildReport
	MigrationReport   harness.MigrationReport
	closeTimeout      time.Duration
	cleanup           func()
}

type unavailableOperatorRuntimePort struct{}

func (unavailableOperatorRuntimePort) RunOperator(
	context.Context,
	bindingoperator.OperatorRuntimeRequest,
) (bindingoperator.OperatorRuntimeResult, error) {
	return bindingoperator.OperatorRuntimeResult{}, bindingoperator.ErrRuntimePortUnavailable
}

func Build(ctx context.Context, config Config, dependencies Dependencies) (*App, error) {
	if err := validate(config, dependencies); err != nil {
		return nil, err
	}
	if dependencies.EngineFactory == nil {
		dependencies.EngineFactory = harness.Build
	}
	if dependencies.MigrationRunner == nil {
		dependencies.MigrationRunner = runHarnessMigration
	}

	migrationReport, err := migrate(ctx, config, dependencies.MigrationRunner)
	if err != nil {
		return nil, err
	}

	designingTools, err := businesstool.NewDesigningProvider(nil, config.RendererCatalogRoot)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: designing tools: %w", err)
	}
	workspaceTools, err := businesstool.NewWorkspaceProvider(config.RendererCatalogRoot)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: AGenUI workspace tools: %w", err)
	}
	if err := workspaceTools.SetEditAuthorizer(designingTools); err != nil {
		return nil, fmt.Errorf("bootstrap: AGenUI workspace edit authorization: %w", err)
	}
	taskInterceptor := orchestration.NewTaskInterceptor()
	taskInterceptor.SetDesigningSnapshotProvider(designingTools)
	taskInterceptor.SetWorkspaceSnapshotProvider(workspaceTools)
	if err := workspaceTools.SetBindingCommitter(taskInterceptor); err != nil {
		return nil, fmt.Errorf("bootstrap: AGenUI workspace binding committer: %w", err)
	}
	searchReceiptInterceptor := orchestration.NewSearchReceiptInterceptor()
	bindingDependencyTools := dependencies.BindingDependencyTools
	if bindingDependencyTools == nil && dependencies.OperatorRuntimePort != nil {
		parentAgentID := strings.TrimSpace(config.AgentID)
		parentAgentVersion := strings.TrimSpace(config.AgentVersion)
		bindingOperatorResolver, resolverErr := businesstool.NewBindingOperatorContextResolver(
			parentAgentID,
			parentAgentVersion,
		)
		if resolverErr != nil {
			return nil, fmt.Errorf("bootstrap: Binder parent scope resolver: %w", resolverErr)
		}
		runtimePort := dependencies.OperatorRuntimePort
		if runtimePort == nil {
			runtimePort = unavailableOperatorRuntimePort{}
		}
		executionService, executionErr := bindingoperator.NewRuntimePortExecutionService(
			runtimePort,
		)
		if executionErr != nil {
			return nil, fmt.Errorf("bootstrap: operator runtime service: %w", executionErr)
		}
		bindingDependencyTools, executionErr = businesstool.NewBindingDependenciesProvider(
			bindingOperatorResolver,
			executionService,
		)
		if executionErr != nil {
			return nil, fmt.Errorf("bootstrap: Binder execution tools: %w", executionErr)
		}
		taskInterceptor.SetBindingOperatorScopeActivator(bindingOperatorResolver)
	}
	designKnowledgeProvider := dependencies.DesignKnowledgeProvider
	if designKnowledgeProvider == nil && dependencies.DesignKnowledge != nil {
		designKnowledgeProvider = dkRuntime.MustStaticProvider(dependencies.DesignKnowledge)
	}
	var designKnowledgeAuthority *designknowledge.ReceiptAuthority
	if designKnowledgeProvider != nil {
		if len(config.DesignKnowledgeReceiptKey) >= 32 {
			designKnowledgeAuthority, err = designknowledge.NewReceiptAuthorityWithKey(
				config.DesignKnowledgeReceiptKey,
			)
		} else {
			designKnowledgeAuthority, err = designknowledge.NewReceiptAuthority()
		}
		if err != nil {
			return nil, fmt.Errorf("bootstrap: design knowledge receipt authority: %w", err)
		}
	}
	generatedHarnessConfigPath := config.HarnessConfigPath
	var rendererPromptCleanup func()
	if strings.TrimSpace(config.RendererCatalogRoot) != "" {
		snapshot, snapshotErr := renderercatalog.LoadCurrent(config.RendererCatalogRoot)
		if snapshotErr != nil {
			return nil, fmt.Errorf("bootstrap: renderer catalog: %w", snapshotErr)
		}
		generatedHarnessConfigPath, rendererPromptCleanup, snapshotErr = materializeRendererPrompt(config.HarnessConfigPath, snapshot)
		if snapshotErr != nil {
			return nil, fmt.Errorf("bootstrap: renderer prompt: %w", snapshotErr)
		}
	}
	options := make([]harness.Option, 0, len(dependencies.ExtensionOptions)+7)
	if generatedHarnessConfigPath != "" {
		options = append(options, harness.WithConfigPath(generatedHarnessConfigPath))
	}
	if config.Environment != "" {
		options = append(options, harness.WithEnvironment(config.Environment))
	}
	if config.CloseTimeout > 0 {
		options = append(options, harness.WithRunDrainTimeout(config.CloseTimeout))
	}
	// Agent-bound providers are registered here and opted in through the Harness
	// agent configuration. Harness itself owns protocol projection and events.
	options = append(options, harness.WithToolCallInterceptorProvider(taskInterceptor))
	options = append(options, harness.WithToolCallInterceptorProvider(searchReceiptInterceptor))
	options = append(options, harness.WithToolProvider(designingTools))
	options = append(options, harness.WithToolProvider(workspaceTools))
	if bindingDependencyTools != nil {
		options = append(options, harness.WithToolProvider(bindingDependencyTools))
	}
	if designKnowledgeProvider != nil {
		designKnowledgeTransformer, transformerErr := inputextension.NewDesignKnowledgeTransformerWithProvider(
			designKnowledgeProvider,
		)
		if transformerErr != nil {
			return nil, fmt.Errorf("bootstrap: design knowledge transformer: %w", transformerErr)
		}
		options = append(options, harness.WithBeforeModelHookProvider(designKnowledgeTransformer))
		designKnowledgeTools, toolErr := businesstool.NewDesignKnowledgeProviderWithProvider(
			designKnowledgeProvider,
			designKnowledgeAuthority,
			config.DesignKnowledgeMaxDocs,
			config.DesignKnowledgeMaxBytes,
		)
		if toolErr != nil {
			return nil, fmt.Errorf("bootstrap: design knowledge tool: %w", toolErr)
		}
		options = append(options, harness.WithToolProvider(designKnowledgeTools))
	}
	for _, outputValidator := range dependencies.OutputValidators {
		if outputValidator != nil {
			options = append(options, harness.WithOutputValidatorProvider(outputValidator))
		}
	}
	options = append(options, dependencies.ExtensionOptions...)

	engine, report, err := dependencies.EngineFactory(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: build harness engine: %w", err)
	}
	if engine == nil {
		return nil, errors.New("bootstrap: engine factory returned nil engine")
	}
	// Test and embedding fakes deliberately implement only the public Engine
	// contract. A source-built Engine additionally exposes its pre-composed
	// native router; absence on a fake must not turn unrelated lifecycle tests
	// into a different build path.
	nativeHarnessHTTP, _ := harness.HTTPHandler(engine)

	readiness, err := engine.Readiness(ctx)
	if err != nil {
		return nil, failAndCleanup(
			fmt.Errorf("bootstrap: readiness: %w", err),
			engine,
			config.CloseTimeout,
		)
	}
	if !readiness.Ready {
		return nil, failAndCleanup(
			fmt.Errorf("bootstrap: not ready: %s", strings.Join(readiness.Reasons, "; ")),
			engine,
			config.CloseTimeout,
		)
	}

	artifactStore, err := stepartifact.NewStore(engine.Artifacts())
	if err != nil {
		return nil, failAndCleanup(
			fmt.Errorf("bootstrap: AGenUI artifact store: %w", err),
			engine,
			config.CloseTimeout,
		)
	}
	packageExporter, err := exportpackage.New(
		artifactStore, config.ExportDataSourceBaseURL,
		dependencies.OperatorDetails,
	)
	if err != nil {
		return nil, failAndCleanup(fmt.Errorf("bootstrap: package exporter: %w", err), engine, config.CloseTimeout)
	}
	taskInterceptor.SetStore(artifactStore)
	if err := designingTools.SetPersistence(artifactStore); err != nil {
		return nil, failAndCleanup(
			fmt.Errorf("bootstrap: designing persistence: %w", err),
			engine,
			config.CloseTimeout,
		)
	}
	closeTimeout := config.CloseTimeout
	if closeTimeout <= 0 {
		closeTimeout = defaultCloseTimeout
	}
	handler, err := httptransport.NewHandler(httptransport.Config{}, httptransport.Dependencies{
		ResolvePrincipal: dependencies.ResolvePrincipal,
		StepArtifacts:    artifactStore,
		PackageExporter:  packageExporter,
	})
	if err != nil {
		return nil, failAndCleanup(
			fmt.Errorf("bootstrap: HTTP handler: %w", err),
			engine,
			config.CloseTimeout,
		)
	}

	return &App{
		Engine:            engine,
		NativeHarnessHTTP: nativeHarnessHTTP,
		Handler:           handler,
		Report:            report,
		MigrationReport:   migrationReport,
		closeTimeout:      closeTimeout,
		cleanup:           rendererPromptCleanup,
	}, nil
}

func (a *App) RegisterHTTP(mux *http.ServeMux) {
	// Harness owns Chat, Runs, events, transcript and Control/Resume. AGenUI
	// contributes only its domain artifact and runtime-package endpoints.
	if a.NativeHarnessHTTP != nil {
		mux.Handle("/", a.NativeHarnessHTTP)
	}
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /readyz", a.ready)
	a.Handler.Register(mux)
}

func (a *App) Close(ctx context.Context) error {
	engineTimeout := a.closeTimeout / 2
	if engineTimeout <= 0 {
		engineTimeout = defaultCloseTimeout / 2
	}
	engineContext, cancel := context.WithTimeout(context.Background(), engineTimeout)
	defer cancel()
	engineErr := a.Engine.Close(engineContext)
	if a.cleanup != nil {
		a.cleanup()
	}
	return engineErr
}

func (a *App) health(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write([]byte("ok\n"))
}

func (a *App) ready(response http.ResponseWriter, request *http.Request) {
	report, err := a.Engine.Readiness(request.Context())
	if err != nil || !report.Ready {
		http.Error(response, "not ready", http.StatusServiceUnavailable)
		return
	}
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write([]byte("ready\n"))
}

func validate(config Config, dependencies Dependencies) error {
	if err := validateConfig(config); err != nil {
		return err
	}
	if dependencies.ResolvePrincipal == nil {
		return errors.New("bootstrap: principal resolver is required")
	}
	if dependencies.OperatorRuntimePort != nil && dependencies.BindingDependencyTools != nil {
		return errors.New(
			"bootstrap: operator runtime port and custom Binding dependency tools are mutually exclusive",
		)
	}
	return nil
}

func validateConfig(config Config) error {
	if strings.TrimSpace(config.AgentID) == "" {
		return errors.New("bootstrap: agent id is required")
	}
	switch config.Environment {
	case "", "local", "testing", "staging", "production":
	default:
		return fmt.Errorf("bootstrap: unsupported environment %q", config.Environment)
	}
	switch config.MigrationMode {
	case "", MigrationDisabled, MigrationCheck, MigrationUp:
	default:
		return fmt.Errorf("bootstrap: unsupported migration mode %q", config.MigrationMode)
	}
	if config.MigrationMode != "" && config.MigrationMode != MigrationDisabled &&
		strings.TrimSpace(config.HarnessConfigPath) == "" {
		return errors.New("bootstrap: harness config path is required for schema migration")
	}
	// 生产红线:绝不允许在启动时自动执行 DDL(MigrationUp)。read-only check 与
	// disabled 都放行——预发已验证 disabled + harness.Build(复用注入的共享 DB、带表
	// 校验)可确认 schema 就绪,且能避开 CheckSchema/RunMigration 自建连接扇出到不可达
	// 库组而崩的问题。生产库表均 out-of-band 预建,故生产可与预发一致走 disabled。
	if config.Environment == "production" && config.MigrationMode == MigrationUp {
		return errors.New(
			"bootstrap: production must not auto-run schema migrations (up); use disabled or check",
		)
	}
	if config.CloseTimeout < 0 || config.HeartbeatInterval < 0 ||
		config.MaxRequestBytes < 0 {
		return errors.New("bootstrap: timeouts and size limits must not be negative")
	}
	return nil
}

func migrate(
	ctx context.Context,
	config Config,
	runner MigrationRunner,
) (harness.MigrationReport, error) {
	mode := config.MigrationMode
	if mode == "" || mode == MigrationDisabled {
		return harness.MigrationReport{}, nil
	}
	report, err := runner(ctx, harness.MigrationOptions{
		ConfigPath:  config.HarnessConfigPath,
		Environment: config.Environment,
	}, mode)
	if err != nil {
		return harness.MigrationReport{}, fmt.Errorf("bootstrap: schema %s: %w", mode, err)
	}
	if !report.Ready {
		return harness.MigrationReport{}, fmt.Errorf(
			"bootstrap: schema %s completed without readiness",
			mode,
		)
	}
	return report, nil
}

func runHarnessMigration(
	ctx context.Context,
	options harness.MigrationOptions,
	mode MigrationMode,
) (harness.MigrationReport, error) {
	switch mode {
	case MigrationCheck:
		return harness.CheckSchema(ctx, options)
	case MigrationUp:
		return harness.RunMigration(ctx, options)
	default:
		return harness.MigrationReport{}, fmt.Errorf("unsupported migration mode %q", mode)
	}
}

func failAndCleanup(cause error, engine harness.Engine, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = defaultCloseTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := engine.Close(ctx); err != nil {
		return errors.Join(cause, fmt.Errorf("bootstrap: cleanup engine: %w", err))
	}
	return cause
}
