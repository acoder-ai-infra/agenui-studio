package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/AGenUI/agenui-studio/harness/internal/app" // register kernel composer via init
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// Build 组装 SDK Engine。
//
// 它会读取 harness 配置（路径按 WithConfigPath / WithEnvironment /
// HARNESS_CONFIG / HARNESS_ENV 的优先级解析），通过 internal/app +
// internal/kernel 组装共享 Composition Root，并绑定到一个 Engine 后返回。
//
// Build 故意严格：YAML 中未知字段、缺失必需依赖、schema 版本不匹配都会
// fail closed 并返回描述性错误。调用方不得在失败后重试 Build；应先修复配置。
//
// 持久化资源默认由 harness 配置驱动并由 kernel 持有。Embedded 宿主
// 可通过 WithSharedSQLDatabase 复用已创建的 *sql.DB；该连接池始终由
// 调用方持有，Build 和 Engine.Close 均不关闭它。Redis 客户端和对象存储
// provider 仍不允许运行时注入。
//
// 调用方拥有返回的 Engine。在宿主关机时使用一个有限 ctx 调用 Close，以
// 确保 kernel 持有的资源能被释放。
func Build(ctx context.Context, opts ...Option) (Engine, BuildReport, error) {
	settings := &buildSettings{}
	for _, opt := range opts {
		if opt != nil {
			opt(settings)
		}
	}
	kopts := kernel.KernelOptions{}
	if err := applySharedSQLDatabaseOption(settings, &kopts); err != nil {
		return nil, BuildReport{}, err
	}
	if err := applyLoggerOption(settings, &kopts); err != nil {
		return nil, BuildReport{}, err
	}

	configPath, envName, err := resolveConfigPath(settings)
	if err != nil {
		return nil, BuildReport{}, err
	}

	logger := kopts.Logger

	if logger != nil {
		logger.Info(ctx, "harness sdk: building engine via kernel.Build",
			observability.String("config_path", configPath),
			observability.String("environment", envName),
		)
	}

	kopts.ConfigPath = configPath
	kopts.Environment = envName
	extCatalog, extInfos, extErr := buildExtensionCatalog(settings)
	if extErr != nil {
		return nil, BuildReport{}, extErr
	}
	if extCatalog != nil {
		kopts.ExtensionRegistry = extCatalog
	}

	k, err := kernel.Build(ctx, kopts)
	if err != nil {
		return nil, BuildReport{}, fmt.Errorf("harness: build kernel: %w", err)
	}

	report := buildReportFromKernel(k, envName)
	report.Extensions = append(report.Extensions, extInfos...)
	annotateUnboundHandlers(report.Extensions, k.BoundToolHandlers)
	report.BuiltAt = time.Now()

	drainTimeout := settings.runDrainTimeout
	if drainTimeout <= 0 {
		drainTimeout = defaultRunDrainTimeout
	}

	eng := newEngine(k, report, drainTimeout)
	return eng, report, nil
}

func applyLoggerOption(settings *buildSettings, opts *kernel.KernelOptions) error {
	if settings == nil {
		return nil
	}
	if settings.loggerRegistrations > 1 {
		return fmt.Errorf("%w: WithLogger may be registered exactly once", ErrInvalidRequest)
	}
	if opts == nil {
		return fmt.Errorf("%w: kernel options are required", ErrInvalidRequest)
	}
	if settings.loggerRegistrations == 1 {
		logger, err := adaptLogger(settings.logger)
		if err != nil {
			return fmt.Errorf("%w: WithLogger: %v", ErrInvalidRequest, err)
		}
		opts.Logger = logger
	}
	return nil
}

func applySharedSQLDatabaseOption(settings *buildSettings, opts *kernel.KernelOptions) error {
	if settings == nil || settings.sharedSQLDatabaseRegistrations == 0 {
		return nil
	}
	if settings.sharedSQLDatabaseRegistrations != 1 {
		return fmt.Errorf("%w: WithSharedSQLDatabase may be registered exactly once", ErrInvalidRequest)
	}
	if settings.sharedSQLDatabase == nil {
		return fmt.Errorf("%w: WithSharedSQLDatabase requires a non-nil *sql.DB", ErrInvalidRequest)
	}
	if opts == nil {
		return fmt.Errorf("%w: kernel options are required", ErrInvalidRequest)
	}
	opts.SharedSQLDatabase = settings.sharedSQLDatabase
	return nil
}

// resolveConfigPath applies the WithConfigPath / WithEnvironment / env-var
// precedence documented on Build.
func resolveConfigPath(s *buildSettings) (path string, env string, err error) {
	env = firstNonEmpty(s.environment, os.Getenv("HARNESS_ENV"), "local")
	env = strings.TrimSpace(env)
	if env == "" {
		env = "local"
	}
	if s.configPath != "" {
		return s.configPath, env, nil
	}
	if v := strings.TrimSpace(os.Getenv("HARNESS_CONFIG")); v != "" {
		return v, env, nil
	}
	return defaultHarnessConfigPath(env), env, nil
}

// defaultHarnessConfigPath mirrors internal/app.DefaultHarnessConfigPath
// without importing internal/app (which would defeat the kernel-Owned
// composition contract). The convention lives here so external module fixtures
// stay decoupled from internal packages.
func defaultHarnessConfigPath(env string) string {
	env = strings.TrimSpace(env)
	if env == "" {
		env = "local"
	}
	return "configs/environments/" + env + "/harness.yaml"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

const defaultRunDrainTimeout = 30 * time.Second

// buildReportFromKernel captures the frozen build state as a BuildReport.
func buildReportFromKernel(k *kernel.Kernel, envName string) BuildReport {
	if k == nil {
		return BuildReport{SDKVersion: Version, Environment: envName}
	}
	report := BuildReport{
		SDKVersion:     Version,
		Environment:    envName,
		SchemaVersions: canonicalSchemaVersions(),
		Providers:      append([]string(nil), k.Providers...),
		Tenants:        cloneTenants(k.Tenants),
	}
	if k.Broker == nil {
		report.Degraded = append(report.Degraded, "event broker not installed")
	}
	if k.HotBuffer == nil {
		report.Degraded = append(report.Degraded, "hot stream buffer not installed")
	}
	if k.Runs == nil {
		report.Unsupported = append(report.Unsupported, "run service missing; Start will fail")
	}
	if k.RunEntry == nil {
		report.Unsupported = append(report.Unsupported, "run entry missing; Start/Resume/Cancel will fail")
	}
	// Expose the configured default Agent for a lightweight readiness check.
	if k.DefaultAgentID != "" {
		report.Agents = []AgentInfo{{AgentID: k.DefaultAgentID}}
	}
	return report
}

// canonicalSchemaVersions 委托 kernel 的唯一事实源，避免 SDK 侧副本漂移。
func canonicalSchemaVersions() []string {
	return kernel.CanonicalSchemaVersions()
}

func cloneTenants(in map[string][]string) map[string][]string {
	if in == nil {
		return nil
	}
	out := make(map[string][]string, len(in))
	for k, v := range in {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// closedErrorf is a small helper for the internal_adapter to build wrapped
// ErrClosed errors with context. It is here to keep errors.go free of format
// helpers.
func closedErrorf(op string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrClosed, op)
	}
	return fmt.Errorf("%w: %s: %v", ErrClosed, op, cause)
}

// wrapInvalidRequest returns %w-wrapped ErrInvalidRequest for internal_adapter
// preflight validation. Placed here so adapters do not accumulate their own
// wrappers.
func wrapInvalidRequest(msg string) error {
	return fmt.Errorf("%w: %s", ErrInvalidRequest, msg)
}

// buildExtensionCatalog turns settings.extensions into an ExtensionCatalog
// suitable for KernelOptions plus a BuildReport-friendly ExtensionInfo slice.
// It fails closed on duplicate (Kind, ID), unknown Kinds, nil implementations
// and empty IDs.
func buildExtensionCatalog(settings *buildSettings) (*kernel.ExtensionCatalog, []ExtensionInfo, error) {
	if settings == nil || len(settings.extensions) == 0 {
		return nil, nil, nil
	}
	entries := make([]kernel.ExtensionEntry, 0, len(settings.extensions))
	infos := make([]ExtensionInfo, 0, len(settings.extensions))
	for i, reg := range settings.extensions {
		kind := kernel.ExtensionKind(reg.kind)
		if kernel.PhaseOfKernel(kind) == 0 {
			return nil, nil, fmt.Errorf("%w: unknown extension kind %q for id %q", ErrInvalidRequest, reg.kind, reg.id)
		}
		// nil 实现或空 ID（含 Provider 变参入口收到 nil 元素 / impl.ID() 空）
		// 一律 fail-closed。
		if reg.impl == nil || reg.id == "" {
			return nil, nil, fmt.Errorf("%w: extension kind %q requires a non-nil implementation with a non-empty ID", ErrInvalidRequest, reg.kind)
		}
		fingerprint := extensionFingerprint(reg.id, reg.kind, Version)
		entries = append(entries, kernel.ExtensionEntry{
			ID:             reg.id,
			Kind:           kind,
			Order:          i,
			Failure:        kernel.FailClosed,
			Fingerprint:    fingerprint,
			Implementation: reg.impl,
		})
		info := ExtensionInfo{
			ID:            reg.id,
			Kind:          reg.kind,
			Order:         i,
			FailurePolicy: string(kernel.FailClosed),
			Fingerprint:   fingerprint,
		}
		if kind == kernel.ExtToolProvider {
			info.HandlerNames = collectHandlerNames(reg.impl)
		}
		infos = append(infos, info)
	}
	cat, err := kernel.NewExtensionCatalog(entries)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return cat, infos, nil
}

// collectHandlerNames 浮现由 harness.WithToolProvider 注册的 function 工具
// 实现名（rc.6：定义在 tools.yaml，代码只交实现）；非 provider 实现返回 nil。
func collectHandlerNames(impl any) []string {
	provider, ok := impl.(extension.ToolProvider)
	if !ok {
		return nil
	}
	tools := provider.FunctionTools()
	if len(tools) == 0 {
		return nil
	}
	out := make([]string, 0, len(tools))
	for _, item := range tools {
		if item == nil {
			continue
		}
		out = append(out, item.Name())
	}
	return out
}

// annotateUnboundHandlers 给未被任何 tools.yaml function 工具引用的实现名
// 追加 " (unbound)" 后缀：注册了实现但配置未启用不视为错误（多环境共用宿主
// 代码），仅在 BuildReport 审计面浮现绑定状态。
func annotateUnboundHandlers(infos []ExtensionInfo, bound map[string]bool) {
	for i := range infos {
		for j, name := range infos[i].HandlerNames {
			if !bound[name] {
				infos[i].HandlerNames[j] = name + " (unbound)"
			}
		}
	}
}

// extensionFingerprint hashes the extension identity + build seed into a
// stable content fingerprint. It is used both on ExtensionEntry (kernel-side)
// and ExtensionInfo (BuildReport-side) so operators can detect implementation
// drift on Resume.
func extensionFingerprint(id, kind, buildSeed string) string {
	h := sha256.New()
	h.Write([]byte(buildSeed))
	h.Write([]byte("\x00"))
	h.Write([]byte(id))
	h.Write([]byte("\x00"))
	h.Write([]byte(kind))
	sum := h.Sum(nil)
	return "sha256:" + hex.EncodeToString(sum[:16])
}

// errRunNotFound 是包内私有的哨兵错误，供 internal_adapter 前置检查使用。
// 它包裹公开的 ErrNotFound，让 SDK 调用方可以 errors.Is(err, ErrNotFound)
// 分类，同时保持包内既有 errors.Is(err, errRunNotFound) 断言不变。
var errRunNotFound = fmt.Errorf("%w: run not found", ErrNotFound)
