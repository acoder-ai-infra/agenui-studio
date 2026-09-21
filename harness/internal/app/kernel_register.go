package app

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// init 向 internal/kernel 注册本包的组装与迁移实现。注册之后，
// kernel.Build 和 kernel.RunMigration 会委托到本包的实现，而无需直接
// import internal/app，从而打破本可能产生的包循环。
//
// 要使用 kernel 拥有的入口的消费方必须（直接或通过 harness 传递地）import
// internal/app 以触发本 init。harness.Build 就是为了一目的而 import 本包的。
func init() {
	kernel.RegisterComposer(buildComposer)
	kernel.RegisterMigrationFunc(buildMigration)
}

// buildComposer 是 kernel.Composer 的实现。它从磁盘读取 harness 配置
// （尊重已解析的 KernelOptions.ConfigPath / Environment），并委托给
// buildWithDependencies 与本包中的组装助手函数。返回一个组装完成的
// *kernel.Kernel，其资源生命周期与一个未导出的 *App 实例共享，只要
// Kernel.Close 回调仍引用它，那个 *App 就会保持存活。
func buildComposer(ctx context.Context, opts kernel.KernelOptions) (*kernel.Kernel, error) {
	configPath := opts.ConfigPath
	if configPath == "" {
		return nil, fmt.Errorf("app: kernel.Composer requires ConfigPath")
	}
	env := opts.Environment
	if env == "" {
		env = "local"
	}
	harnessCfg, err := LoadHarnessConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("app: load harness config: %w", err)
	}
	if err := ValidateSelectedEnvironment(env, harnessCfg); err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	if err := EnsureRuntimeDirectories(harnessCfg); err != nil {
		return nil, fmt.Errorf("app: ensure runtime directories: %w", err)
	}
	componentCfg, err := LoadComponentConfigs(harnessCfg)
	if err != nil {
		return nil, fmt.Errorf("app: load component configs: %w", err)
	}

	logger := opts.Logger
	var managedLogger *observability.ZapLogger
	if logger == nil {
		managedLogger, err = NewConfiguredLogger(harnessCfg)
		if err != nil {
			return nil, fmt.Errorf("app: %w", err)
		}
		logger = managedLogger
	} else {
		logger = logger.With(
			observability.String("service", harnessCfg.Service.Name),
			observability.String("env", string(harnessCfg.Environment)),
		)
	}
	logger = observability.NewRingLogger(logger, harnessCfg.Logging.RecentBufferItems)
	tracer := opts.Tracer
	if tracer == nil {
		tracer = observability.NewNoopTracer("kernel.compose")
	}

	appOptions := []BuildOption{
		WithHarnessConfig(harnessCfg),
		WithArtifactConfig(componentCfg.Artifact),
		WithCapabilityCatalogs(componentCfg.Skills, componentCfg.Tools, componentCfg.MCP),
	}
	if opts.ContextSnapshots != nil {
		appOptions = append(appOptions, WithContextSnapshotStore(opts.ContextSnapshots))
	}
	if opts.SharedSQLDatabase != nil {
		appOptions = append(appOptions, WithSharedSQLDatabase(opts.SharedSQLDatabase))
	}
	// 扩展目录在 Build 时交给 Composition Root，由各挂载点（broker 装饰器、
	// Tool Gateway、runtime hook 等）消费；同时仍会回填到 Kernel.Extensions
	// 供 BuildReport 与 kernel 预回合管线使用。
	catalog, _ := opts.ExtensionRegistry.(*kernel.ExtensionCatalog)
	if catalog != nil {
		appOptions = append(appOptions, WithExtensionCatalog(catalog))
	}

	application, err := Build(
		componentCfg.Models,
		componentCfg.Storage,
		componentCfg.Auth,
		componentCfg.Redis,
		logger,
		tracer,
		appOptions...,
	)
	if err != nil {
		if managedLogger != nil {
			err = errors.Join(err, managedLogger.Close())
		}
		return nil, fmt.Errorf("app: build composition: %w", err)
	}
	var managedLoggerClose func() error
	if managedLogger != nil {
		managedLoggerClose = managedLogger.Close
	}
	k := newKernelFromApp(application, logger, tracer, managedLoggerClose)
	if catalog != nil {
		// 目录同步记在 Kernel 上：BuildReport 列表、kernel 预回合管线与 SDK 侧
		// 视图填充都从这里读取冻结后的条目。
		k.Extensions = catalog
	}
	return k, nil
}

// buildMigration 是 kernel.MigrationFunc 的实现。它复用已有的 MigrateStorage /
// CheckStorageSchema 助手，令 hosted operator（通过 cmd/harness migrate）和 SDK
// 调用方（通过 harness.RunMigration）看到完全一致的 schema 决策。
func buildMigration(ctx context.Context, req kernel.MigrationRequest) (kernel.MigrationResult, error) {
	configPath := req.ConfigPath
	if configPath == "" {
		return kernel.MigrationResult{}, fmt.Errorf("app: kernel.Migration requires ConfigPath")
	}
	env := req.Environment
	if env == "" {
		env = "local"
	}
	harnessCfg, err := LoadHarnessConfig(configPath)
	if err != nil {
		return kernel.MigrationResult{}, fmt.Errorf("app: load harness config: %w", err)
	}
	if err := ValidateSelectedEnvironment(env, harnessCfg); err != nil {
		return kernel.MigrationResult{}, fmt.Errorf("app: %w", err)
	}
	if err := EnsureRuntimeDirectories(harnessCfg); err != nil {
		return kernel.MigrationResult{}, fmt.Errorf("app: ensure runtime directories: %w", err)
	}
	componentCfg, err := LoadComponentConfigs(harnessCfg)
	if err != nil {
		return kernel.MigrationResult{}, fmt.Errorf("app: load component configs: %w", err)
	}
	production := harnessCfg.Environment != EnvironmentLocal
	var res StorageSchemaResult
	switch req.Action {
	case kernel.MigrationUp:
		res, err = MigrateStorage(ctx, componentCfg.Storage, production)
	case kernel.MigrationPlan, kernel.MigrationStatus:
		res, err = CheckStorageSchema(ctx, componentCfg.Storage, production)
	default:
		return kernel.MigrationResult{}, fmt.Errorf("app: unsupported migration action %q", req.Action)
	}
	if err != nil {
		return kernel.MigrationResult{}, err
	}
	return kernel.MigrationResult{
		Backend: res.Backend,
		Action:  req.Action,
		Ready:   res.Ready,
		Noop:    res.Noop,
	}, nil
}

// newKernelFromApp 将已组装的 *App 提升为 *kernel.Kernel。它取代了旧的
// harness.kernelFromApp 适配器，使 hosted（cmd/harness → app.Build）和 SDK
// （harness.Build → kernel.Build）两个入口共享同一个适配器。
func newKernelFromApp(a *App, logger observability.StructuredLogger, tracer observability.TraceProvider, managedLoggerClose func() error) *kernel.Kernel {
	if a == nil {
		return nil
	}
	var closeOnce sync.Once
	var closeErr error
	return &kernel.Kernel{
		Environment:       kernel.Environment(string(a.Environment)),
		DefaultAgentID:    a.DefaultAgentID,
		Providers:         a.Providers,
		Tenants:           a.Tenants,
		Stores:            a.Stores,
		Artifacts:         a.Artifacts,
		Gateway:           a.Gateway,
		Runs:              a.RunService,
		RunEntry:          a.RunEntry,
		Registry:          a.Registry,
		Control:           a.Control,
		ControlTickets:    a.ControlTickets,
		BoundToolHandlers: a.BoundToolHandlers,
		Broker:            a.Broker,
		HotBuffer:         a.HotBuffer,
		Handler:           a.Handler,
		Logger:            logger,
		Tracer:            tracer,
		Close: func(context.Context) error {
			closeOnce.Do(func() {
				closeErr = a.Close()
				if managedLoggerClose != nil {
					closeErr = errors.Join(closeErr, managedLoggerClose())
				}
			})
			return closeErr
		},
	}
}
