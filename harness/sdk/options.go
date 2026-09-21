package harness

import (
	"database/sql"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// Option configures how the SDK Engine is built. Options are collected on the
// call site and applied by Build in a deterministic order.
//
// SDK 故意保持 Option 列表短小：持久化资源默认由 harness 配置文件完整描述，
// 并由 kernel 持有。Embedded 宿主仅可通过 WithSharedSQLDatabase 复用已创建
// 的 SQL 连接池；Redis 和对象存储仍由配置创建并关闭。
type Option func(*buildSettings)

// buildSettings is the private accumulator produced by Options. It is filled
// in Build and never surfaces to callers; callers use the With* helpers.
type buildSettings struct {
	// configPath is the harness.yaml path; empty falls back to the environment
	// default (configs/environments/<env>/harness.yaml).
	configPath string
	// environment overrides the harness.yaml environment field ONLY for path
	// resolution when configPath is empty. It cannot force a non-matching
	// environment to load: LoadHarnessConfig still validates.
	environment string
	// logger is an optional caller-owned observability collaborator. Nil means
	// the kernel builds its own logger from harness.yaml.
	logger              any
	loggerRegistrations int
	// runDrainTimeout bounds how long Close waits for in-flight runs to drain
	// before force-releasing resources. Zero uses the kernel default.
	runDrainTimeout time.Duration
	// sharedSQLDatabase 是 Embedded 宿主可选注入的共享 SQL 连接池。
	// registrations 单独计数，使重复注册和显式 nil 都能 fail closed。
	sharedSQLDatabase              *sql.DB
	sharedSQLDatabaseRegistrations int

	// extensions 是 WithXxx 扩展注册的累加器；(Kind, ID) 重复会在 Build 时
	// 直接拒绝。
	extensions []extensionRegistration
}

// WithSharedSQLDatabase registers a caller-owned SQL pool for an Embedded
// Engine. The frozen harness config must still select storage.backend sqlite
// or mysql so dialect/readiness semantics remain explicit. The caller opens
// the pool for that configured database and closes it; Build and Engine.Close
// never close it. SQLite callers should share one pool rather than opening the
// same file through another SQLite driver in the same process.
func WithSharedSQLDatabase(db *sql.DB) Option {
	return func(s *buildSettings) {
		s.sharedSQLDatabase = db
		s.sharedSQLDatabaseRegistrations++
	}
}

// WithConfigPath overrides the harness config path. When empty (default),
// Build reads HARNESS_CONFIG or falls back to
// configs/environments/<HARNESS_ENV|local>/harness.yaml.
func WithConfigPath(path string) Option {
	return func(s *buildSettings) {
		s.configPath = path
	}
}

// WithEnvironment sets the environment used to resolve the default config
// path when WithConfigPath is empty. Values must be one of local / testing /
// staging / production and match the environment declared in the loaded config.
func WithEnvironment(env string) Option {
	return func(s *buildSettings) {
		s.environment = env
	}
}

// WithLogger injects a caller-owned logger. logger must be either a
// *zap.Logger or an implementation of the public Logger interface. Its output,
// level, encoding, sampling and rotation policy take precedence over
// harness.yaml. Harness still adds standard fields and the recent-log ring;
// Build and Engine.Close never synchronize or close the injected logger.
//
// The parameter uses any because Go cannot express a union containing both a
// concrete *zap.Logger and arbitrary Logger implementations. Build validates
// the supported types, nil values and duplicate registrations fail closed.
func WithLogger(logger any) Option {
	return func(s *buildSettings) {
		s.logger = logger
		s.loggerRegistrations++
	}
}

// WithRunDrainTimeout bounds how long Engine.Close waits for in-flight runs to
// drain. Callers can also pass a bounded ctx to Close for the same effect.
func WithRunDrainTimeout(d time.Duration) Option {
	return func(s *buildSettings) {
		s.runDrainTimeout = d
	}
}

// WithIdentityResolver 注册一个业务自定义的 IdentityResolver。Engine 会
// 在每个 Run 启动时、入口 guardrail 之前恰好调用一次，将线上传入的
// 业务身份映射为冻结后的 Harness 身份。
//
// IdentityResolver 的 id 需在整个 Engine 内保持唯一；传入 nil impl 会以
// ErrInvalidRequest 失败。
func WithIdentityResolver(id string, impl extension.IdentityResolver) Option {
	return registerTypedExtension(id, extension.KindIdentityResolver, impl)
}

// WithRunInitializer 注册一个 RunInitializer：在身份解析完成后、AgentBinding
// 之前注入本轮的 ScopedData / Artifact 引用。重复 id 会在 Build 时失败。
func WithRunInitializer(id string, impl extension.RunInitializer) Option {
	return registerTypedExtension(id, extension.KindRunInitializer, impl)
}

// WithContextContributor 注册一个 ContextContributor：在 AgentBinding /
// CapabilitySnapshot 之后、InputNormalizer 之前产出业务 context fragment。
// 多个 contributor 按注册顺序确定性地叠加。
func WithContextContributor(id string, impl extension.ContextContributor) Option {
	return registerTypedExtension(id, extension.KindContextContributor, impl)
}

// WithInputNormalizer 注册确定性的输入归一化器。归一化是 Engine 级语义：
// 可注册多个，按注册顺序链式执行（前一个的输出作为下一个的 RawInput），
// 不与 agent_id 绑定。需要按 Agent 差异化处理输入时，应在宿主调用层于
// Start 前处理，或使用 BeforeModelHook 并依据 ctx.AgentID 自过滤。
func WithInputNormalizer(id string, impl extension.InputNormalizer) Option {
	return registerTypedExtension(id, extension.KindInputNormalizer, impl)
}

// WithToolProvider 注册一组业务函数工具的实现提供者（rc.6 语义：工具的
// 定义与治理属性只在 tools.yaml 维护，provider 只提供 handler 名 → Go
// 实现的映射）。变参数组形态，条目 ID 取 impl.ID()；nil 元素 / 空 ID /
// 重复 ID 在 Build 时 fail-closed。所有调用统一经由 Tool Gateway 执行
// （schema 校验、agent ACL、事件落账、超时、大结果溢出）；tools.yaml 引用
// 的实现先查本选项注册，未命中再查全集（extension.RegisterTools），两处
// 都没有时 Build fail-closed。BuildReport.Extensions[].HandlerNames 浮现
// 实现目录。
func WithToolProvider(impls ...extension.ToolProvider) Option {
	return func(s *buildSettings) {
		for _, impl := range impls {
			s.extensions = append(s.extensions, typedExtensionRegistration(extension.KindToolProvider, impl))
		}
	}
}

// WithOutputValidatorProvider 注册一组结构化输出的验证器。Validator 可以
// 选择 accept / retry / fail，但不能修改 canonical event。agent 绑定型扩展：
// 本选项只提供实现，是否执行由 agents.yaml `extensions.output_validators`
// 按 impl.ID() 声明，未声明的 agent（含子 Agent child run）不执行；多个
// validator 按声明顺序执行，首次非-Accept 结果短路。
func WithOutputValidatorProvider(impls ...extension.OutputValidator) Option {
	return func(s *buildSettings) {
		for _, impl := range impls {
			s.extensions = append(s.extensions, typedExtensionRegistration(extension.KindOutputValidator, impl))
		}
	}
}

// WithProtocolProjector 注册将 canonical event 投影为业务协议帧的 projector。
// Projector 只读，不会构造 canonical event。多个 projector 按注册顺序执行，
// 它们产出的 frame 拼接后归入同一个流。
func WithProtocolProjector(id string, impl extension.ProtocolProjector) Option {
	return registerTypedExtension(id, extension.KindProtocolProjector, impl)
}

// WithEventObserver 注册一个旁路 observer，用于审计 / 指标等副作用。
// Observer 不可阻塞主管道不可修改事实链；Engine 会记录失败，但不会回滚
// 已完成的 Run。
func WithEventObserver(id string, impl extension.EventObserver) Option {
	return registerTypedExtension(id, extension.KindEventObserver, impl)
}

// WithBeforeModelHookProvider 注册一组逐轮模型请求塑形 Hook：在每次
// 模型调用前（含多轮工具循环）改写即将送入模型的消息序列、从 Agent 配置
// 工具集中选择本轮可见工具、调整白名单化的 ModelCallOptions。与
// InputNormalizer（每 Run 一次）职责互补。agent 绑定型扩展：本选项只
// 提供实现，是否执行由 agents.yaml `extensions.before_model_hooks` 按
// impl.ID() 声明，未声明的 agent 不执行；多个 Hook 按声明顺序串联（后一个
// 收到前一个的输出），失败 fail closed，改写以 debug 事件留痕。
func WithBeforeModelHookProvider(impls ...extension.BeforeModelHook) Option {
	return func(s *buildSettings) {
		for _, impl := range impls {
			s.extensions = append(s.extensions, typedExtensionRegistration(extension.KindBeforeModelHook, impl))
		}
	}
}

// WithToolCallInterceptorProvider 注册一组工具调用环绕拦截器：可在执行前
// 改写入参、执行后改写模型可见结果、并做同步侧写。Tool Gateway 的审批/
// 幂等/事件链不变，落账仍为真实执行结果。agent 绑定型扩展：本选项只
// 提供实现，是否执行由 agents.yaml `extensions.tool_call_interceptors` 按
// impl.ID() 声明（仅 eino 路径有挂载点）；多个拦截器按声明顺序嵌套，
// 先声明者在最外层。
func WithToolCallInterceptorProvider(impls ...extension.ToolCallInterceptor) Option {
	return func(s *buildSettings) {
		for _, impl := range impls {
			s.extensions = append(s.extensions, typedExtensionRegistration(extension.KindToolCallInterceptor, impl))
		}
	}
}

// registerTypedExtension 是 Engine 级强类型 WithXxx 共享的内部注册入口：
// 保留累加器的内部形状不变，同时让每个对外入口在 impl 上都拿到强类型保护。
func registerTypedExtension(id string, kind extension.Kind, impl any) Option {
	return func(s *buildSettings) {
		s.extensions = append(s.extensions, extensionRegistration{
			kind: string(kind),
			id:   id,
			impl: impl,
		})
	}
}

// identifiedExtension 抽象四类 agent 绑定型扩展接口共有的 ID() 方法，
// typedExtensionRegistration 用它从实现体自身提取条目 ID。
type identifiedExtension interface {
	ID() string
}

// typedExtensionRegistration 把一个自描述 ID 的扩展实现投影为内部注册
// 记录。nil 实现产出空 ID + nil impl 的记录，由 Build 的
// buildExtensionCatalog 统一 fail-closed（Option 闭包无法返回错误）。
func typedExtensionRegistration(kind extension.Kind, impl any) extensionRegistration {
	reg := extensionRegistration{kind: string(kind)}
	identified, ok := impl.(identifiedExtension)
	if !ok || identified == nil {
		return reg
	}
	reg.id = identified.ID()
	reg.impl = impl
	return reg
}

// extensionRegistration 是 WithXxx 强类型注册入口产出的内部记录。调用方
// 应通过公开的 WithIdentityResolver / WithRunInitializer / ... 注册扩展，
// 不应直接构造本结构体。
type extensionRegistration struct {
	kind string
	id   string
	impl any
}
