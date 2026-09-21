package harness

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/AGenUI/agenui-studio/harness/internal/app" // 通过 init 注册 kernel migration
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
)

// MigrationAction 是 SDK 对外支持的 schema 管理动作。
type MigrationAction string

const (
	// MigrationPlanAction 只上报当前 schema 状态，不会改变任何东西。
	MigrationPlanAction MigrationAction = "plan"
	// MigrationUpAction 按顺序应用每一份待执行的 migration。
	MigrationUpAction MigrationAction = "up"
	// MigrationStatusAction 是 plan 的只读别名。
	MigrationStatusAction MigrationAction = "status"
)

// MigrationOptions 配置一次 migration 调用。它与 harness.Build 使用相同的
// harness.yaml 解析与共享资源注入机制，确保 migration 与 Build 看到一致
// 的配置。
type MigrationOptions struct {
	// ConfigPath 覆盖 harness 配置路径，优先级与 harness.WithConfigPath 一致。
	ConfigPath string
	// Environment 在 ConfigPath 为空时控制路径解析所用环境，语义与
	// harness.WithEnvironment 一致。
	Environment string
}

// MigrationReport 记录一次 migration 调用的结果。
type MigrationReport struct {
	// Backend 回显配置声明的存储后端（sqlite / mysql / memory）。
	Backend string
	// Action 镜像请求的 MigrationAction。
	Action MigrationAction
	// Ready 为 true 时表示目标 schema 已就绪，可以进入 Build。
	Ready bool
	// Noop 为 true 时表示本次动作无实际变化（如 memory 后端或 schema
	// 已经迁移完成）。
	Noop bool
	// SchemaVersions 回显当前构建支持的 canonical schema 版本。
	SchemaVersions []string
	// CheckedAt 是本 report 产出时的时刻。
	CheckedAt time.Time
}

// MigrationPlan 上报当前 schema 状态，绝不修改 DB。调用方用它在 Build
// 之前确认存储后端已就绪。
func MigrationPlan(ctx context.Context, opts MigrationOptions) (MigrationReport, error) {
	return runMigration(ctx, opts, MigrationStatusAction)
}

// RunMigration 应用所有待执行的 migration。仅在面向不同 DSN 时可并发调用；
// 面向同一个 DSN 时必须串行。
func RunMigration(ctx context.Context, opts MigrationOptions) (MigrationReport, error) {
	return runMigration(ctx, opts, MigrationUpAction)
}

// CheckSchema 是一个仅元数据级别的 readiness 探针。它浮现与 Engine 在 Build
// 时执行的相同存储后端健康检查。
func CheckSchema(ctx context.Context, opts MigrationOptions) (MigrationReport, error) {
	return runMigration(ctx, opts, MigrationStatusAction)
}

func runMigration(ctx context.Context, opts MigrationOptions, action MigrationAction) (MigrationReport, error) {
	configPath := opts.ConfigPath
	env := opts.Environment
	if env == "" {
		env = strings.TrimSpace(os.Getenv("HARNESS_ENV"))
	}
	if env == "" {
		env = "local"
	}
	if configPath == "" {
		configPath = strings.TrimSpace(os.Getenv("HARNESS_CONFIG"))
	}
	if configPath == "" {
		return MigrationReport{}, fmt.Errorf("%w: MigrationOptions.ConfigPath or HARNESS_CONFIG is required", ErrInvalidRequest)
	}
	kAction, err := toKernelMigrationAction(action)
	if err != nil {
		return MigrationReport{}, err
	}
	result, err := kernel.RunMigration(ctx, kernel.MigrationRequest{
		ConfigPath:  configPath,
		Environment: env,
		Action:      kAction,
	})
	if err != nil {
		return MigrationReport{}, fmt.Errorf("harness: %s migration: %w", action, err)
	}
	return MigrationReport{
		Backend:        result.Backend,
		Action:         action,
		Ready:          result.Ready,
		Noop:           result.Noop,
		SchemaVersions: canonicalSchemaVersions(),
		CheckedAt:      time.Now(),
	}, nil
}

func toKernelMigrationAction(a MigrationAction) (kernel.MigrationAction, error) {
	switch a {
	case MigrationUpAction:
		return kernel.MigrationUp, nil
	case MigrationPlanAction:
		return kernel.MigrationPlan, nil
	case MigrationStatusAction:
		return kernel.MigrationStatus, nil
	}
	return "", fmt.Errorf("%w: unsupported migration action %q", ErrInvalidRequest, a)
}
