package kernel

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/AGenUI/agenui-studio/harness/internal/agentregistry"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/control"
	"github.com/AGenUI/agenui-studio/harness/internal/control/controlticket"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// Environment mirrors the harness config's environment enum without importing
// the internal/app package (which imports this one).
type Environment string

const (
	EnvironmentLocal      Environment = "local"
	EnvironmentTesting    Environment = "testing"
	EnvironmentStaging    Environment = "staging"
	EnvironmentProduction Environment = "production"
)

// RunEntry is the Composition Root's execution port. It is the union of
// server.RunDispatcher, server.RunCanceller and control.Resumer so the SDK
// facade can drive Start/Resume/Cancel without importing dispatcher internals.
type RunEntry interface {
	Dispatch(ctx context.Context, turn *storage.OpenTurnResult) error
	Resume(ctx context.Context, req control.ResumeRequest) error
	Cancel(ctx context.Context, sessionID, runID, reason string) error
}

// Kernel is the assembled shared Composition Root. Both the hosted server and
// the SDK Engine wrap the same *Kernel; there is exactly one truth per process.
//
// Fields are read-only accessors filled once by Build; 调用方不得修改它们。
// Close 以 LIFO 顺序释放全部托管资源。
type Kernel struct {
	// Environment is the frozen deployment tier from HarnessConfig.
	Environment Environment
	// DefaultAgentID is the harness config's runtime.default_agent_id used
	// when a StartRequest omits Identity.AgentID.
	DefaultAgentID string
	// Providers enumerates all model provider names registered across tenants.
	Providers []string
	// Tenants maps tenant_id -> provider names.
	Tenants map[string][]string

	// Storage & data plane.
	Stores    storage.Stores
	Artifacts *artifact.Store
	Snapshots contextpkg.SnapshotManager

	// Gateway & runtime.
	Gateway modelgateway.ModelGateway
	// Runs is the durable RunService used by SDK Start to open a turn before
	// dispatch. It is the same instance the hosted server injects into its
	// protocol handlers.
	Runs *storage.RunService
	// RunEntry hands off runs to the runtime. Start / Resume / Cancel all go
	// through this port after subscription is installed.
	RunEntry RunEntry
	// Registry is the AgentRegistry service used for readiness and debug.
	Registry *agentregistry.Service
	// Control owns ControlRequest/Response state; SDK exposes only the
	// public-safe subset via ControlTickets.
	Control        *control.Service
	ControlTickets *controlticket.Codec
	// BoundToolHandlers 是被 tools.yaml function 工具引用的 handler 名集合，
	// 供 SDK BuildReport 标注 unbound 的 ToolProvider 实现。
	BoundToolHandlers map[string]bool

	// Protocol projection plane.
	Broker    protocol.EventBroker
	HotBuffer protocol.HotStreamBuffer

	// Handler is the pre-wired hosted server http.Handler. Only cmd/harness uses
	// it; SDK-only entries can ignore it.
	Handler http.Handler

	// Observability.
	Logger observability.StructuredLogger
	Tracer observability.TraceProvider

	// Extensions 非 nil 时，存放已冻结的 SDK 扩展 catalog。运行时扩展管道
	// 通过 ByKind 查找它；BuildReport 也会浮现其中的条目。nil 时表示
	// kernel 在无 SDK 侧扩展的模式下工作（hosted server 默认就是 nil）。
	Extensions *ExtensionCatalog

	// Close releases all managed resources (LIFO). It is safe to call
	// concurrently and repeatedly; every caller sees the same result.
	Close func(ctx context.Context) error
}

// KernelOptions 是 Build 的输入，携带冻结后的配置快照。标为 "optional" 的字段
// 会 fallback 到配置声明的默认值；标为 "required" 的字段缺失时 Build 会失败。
//
// 持久化资源默认由 harness 配置驱动。Embedded SDK 可以显式传入宿主所有的
// SharedSQLDatabase；Redis 和 Artifact 对象存储仍不能通过 KernelOptions 注入。
//
// 调用方在每次 Build 中构造一份 KernelOptions；Build 启动后对它的修改不受支持。
// Composer 需对保留的 slice / map 类字段自行防御性拷贝。
type KernelOptions struct {
	// ConfigPath is the resolved harness.yaml path.
	ConfigPath string
	// Environment is the resolved environment name ("local", "testing", "staging", "production").
	Environment string
	// Logger / Tracer are optional; when nil the composer builds defaults.
	Logger observability.StructuredLogger
	Tracer observability.TraceProvider
	// SharedSQLDatabase is an optional caller-owned pool used only when the
	// frozen storage backend is mysql. The composer must not close or
	// reconfigure it.
	SharedSQLDatabase *sql.DB
	// ContextSnapshots, when set, overrides the default Artifact-backed store.
	ContextSnapshots contextpkg.SnapshotStore
	// ExtensionRegistry 非 nil 时，存放已冻结的 SDK 扩展 registry，kernel 会
	// 将其接入自己的管道。
	ExtensionRegistry any
}
