package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/agentbinding"
	"github.com/AGenUI/agenui-studio/harness/internal/agentgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/agentregistry"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/control"
	"github.com/AGenUI/agenui-studio/harness/internal/control/controlticket"
	"github.com/AGenUI/agenui-studio/harness/internal/dispatcher"
	"github.com/AGenUI/agenui-studio/harness/internal/httptooldef"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/orchestrator"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
	"github.com/AGenUI/agenui-studio/harness/internal/runtimestore"
	"github.com/AGenUI/agenui-studio/harness/internal/skill"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/toolconfig"
	"github.com/AGenUI/agenui-studio/harness/internal/toolgateway"
)

type buildSettings struct {
	harness           *HarnessConfig
	artifact          *ArtifactConfig
	catalogs          []CapabilityCatalog
	contextSnapshots  contextpkg.SnapshotStore
	extensions        *kernel.ExtensionCatalog
	sharedSQLDatabase *sql.DB
}

type BuildOption func(*buildSettings)

func WithHarnessConfig(cfg HarnessConfig) BuildOption {
	return func(settings *buildSettings) { settings.harness = &cfg }
}

func WithArtifactConfig(cfg ArtifactConfig) BuildOption {
	return func(settings *buildSettings) { settings.artifact = &cfg }
}

func WithCapabilityCatalogs(catalogs ...CapabilityCatalog) BuildOption {
	return func(settings *buildSettings) { settings.catalogs = append([]CapabilityCatalog(nil), catalogs...) }
}

// WithContextSnapshotStore 替换默认的 Artifact-backed ContextSnapshot 存储。
// 该扩展点用于测试或受控后端替换；非本地环境仍禁止内存实现。
func WithContextSnapshotStore(store contextpkg.SnapshotStore) BuildOption {
	return func(settings *buildSettings) { settings.contextSnapshots = store }
}

// WithExtensionCatalog 把 SDK 注册、kernel 冻结后的扩展目录交给 Composition
// Root：composer 在 Build 时把各类扩展投影到对应挂载点（event_observer →
// broker 装饰器，tool_provider → Tool Gateway，output_validator → runtime
// hook 等），而不再只是事后挂到 Kernel.Extensions 字段上。
func WithExtensionCatalog(catalog *kernel.ExtensionCatalog) BuildOption {
	return func(settings *buildSettings) { settings.extensions = catalog }
}

// WithSharedSQLDatabase installs a caller-owned pool at the Composition Root.
// buildStores preserves the configured SQLite/MySQL identity and readiness
// checks, skips opening a second pool and never closes the caller-owned pool.
func WithSharedSQLDatabase(db *sql.DB) BuildOption {
	return func(settings *buildSettings) { settings.sharedSQLDatabase = db }
}

type formalComposition struct {
	runs            *storage.RunService
	dispatcher      *formalRunDispatcher
	registry        *agentregistry.Service
	runtimeRegistry runtimeAgentRegistry
	configControl   *agentregistry.AgentConfigControlService
	prompts         agentregistry.PromptStore
	mcpService      *mcp.Service
	managedMCP      *mcp.SQLManagedRegistry
	mcpOAuth        *mcp.OAuthAuthorizationManager
	toolConfig      *toolconfig.SQLManagedRegistry
	httpTools       *httptooldef.SQLManagedRegistry
	skillService    *skill.Service
	toolRegistry    toolgateway.ToolLister
	lifecycle       *runLifecycle
	snapshots       contextpkg.SnapshotManager
	// boundToolHandlers 是被 tools.yaml 引用的 function handler 名集合，
	// 供 App/Kernel 透传给 BuildReport 标注 unbound 扩展实现。
	boundToolHandlers map[string]bool
}

type runtimeAgentRegistry interface {
	agentregistry.Registry
	GetConfigSnapshotForTenant(context.Context, string, string) (agentregistry.EffectiveConfig, error)
	ResolveGatewayTarget(context.Context, agentregistry.ResolveGatewayTargetRequest) (agentregistry.ResolvedGatewayTarget, error)
}

type formalCompositionDependencies struct {
	database        *sql.DB
	controlDatabase *sql.DB
	// agentsFromDatabase / promptsFromDatabase 直接取自 harness.yaml 的
	// components source 声明（ADR-0003：显式 source 完全取代环境+后端的
	// 隐式推导）。
	agentsFromDatabase  bool
	promptsFromDatabase bool
	contextSnapshots    contextpkg.SnapshotStore
	controlTickets      *controlticket.Codec
	tokenBudgets        agentruntime.TokenBudgetAllocator
	// extensions 是 SDK 注册、kernel 冻结后的扩展目录；tool_provider /
	// output_validator 等条目在组装时投影到对应挂载点。
	extensions *kernel.ExtensionCatalog
}

func buildFormalComposition(
	ctx context.Context,
	cfg HarnessConfig,
	models ModelConfig,
	stores storage.Stores,
	artifacts *artifact.Store,
	state agentruntime.RuntimeStateManager,
	gatewayModel agentruntime.ModelInvoker,
	writer agentruntime.StorageWriteExecutor,
	broker protocol.EventBroker,
	hotBuffer protocol.HotStreamBuffer,
	logger observability.StructuredLogger,
	tracer observability.TraceProvider,
	dependencies formalCompositionDependencies,
) (*formalComposition, error) {
	snapshotStore := dependencies.contextSnapshots
	if snapshotStore == nil {
		snapshotStore = artifactContextSnapshotStore{artifacts: artifacts}
	}
	if !cfg.Environment.IsLocal() {
		if _, inMemory := snapshotStore.(*contextpkg.InMemorySnapshotStore); inMemory {
			return nil, errors.New("in-memory ContextSnapshot store is not allowed outside local environment")
		}
		if dependencies.database == nil {
			return nil, errors.New("production Agent Registry database is required")
		}
	}
	capabilities, err := installConfiguredCapabilitiesWithExtensions(ctx, cfg, stores, artifacts, logger, tracer, dependencies.extensions, dependencies.controlDatabase)
	if err != nil {
		return nil, fmt.Errorf("install configured capabilities: %w", err)
	}
	// registry/prompt 的 SQL 后端统一复用共享连接池（sqlite/mysql 都是
	// SQLDB）；是否真正用库由各组件的 source 声明决定。
	registryDatabase := dependencies.controlDatabase
	if dependencies.agentsFromDatabase && registryDatabase == nil {
		return nil, errors.New("components.agents: source \"database\" requires a sqlite/mysql storage backend")
	}
	if dependencies.promptsFromDatabase && registryDatabase == nil {
		return nil, errors.New("components.prompts: source \"database\" requires a sqlite/mysql storage backend")
	}
	var agentLoader agentregistry.Loader
	if !dependencies.agentsFromDatabase {
		agentLoader = agentregistry.NewYAMLLoader(cfg.Components.Agents.Path)
	}
	registry, promptResolver, _, err := buildAgentRegistry(ctx, agentRegistryBuildOptions{
		Production:          !cfg.Environment.IsLocal(),
		DatabaseOnly:        dependencies.agentsFromDatabase,
		Database:            registryDatabase,
		PromptsFromDatabase: dependencies.promptsFromDatabase,
		ToolRegistry:        capabilities.Registry,
		Loader:              agentLoader,
		PromptCatalogPath:   cfg.Components.Prompts.Path,
		Logger:              logger,
		Tracer:              tracer,
	})
	if err != nil {
		return nil, err
	}
	promptStore := agentregistry.PromptStoreFromResolver(promptResolver)
	if promptStore == nil {
		return nil, errors.New("composed PromptStore is unavailable")
	}
	// 管理面 PromptStore 始终指向数据库：管理接口管理的是数据库记录，本地
	// 文件的改动经重启生效，不通过管理接口写进程内存。prompts=file 时运行时
	// 仍只读本地 YAML（内存 store），两者互不影响（与 skills 的 AdminSkills
	// 同一模式）。表结构与其他管理面表一致，由 migration 负责，此处不建表。
	adminPromptStore := promptStore
	if !dependencies.promptsFromDatabase && registryDatabase != nil {
		sqlPrompts, err := agentregistry.NewSQLPromptStore(registryDatabase)
		if err != nil {
			return nil, fmt.Errorf("build admin prompt store: %w", err)
		}
		adminPromptStore = sqlPrompts
	}
	var runtimeRegistry runtimeAgentRegistry = registry
	var configControl *agentregistry.AgentConfigControlService
	if dependencies.controlDatabase != nil {
		controlStore := agentregistry.NewSQLAgentConfigControlStore(dependencies.controlDatabase)
		configControl = agentregistry.NewAgentConfigControlService(controlStore)
		configControl.SetPreparer(registry)
		// The runtime resolve path re-reads the released version on every run;
		// cache the immutable version snapshots so only the live release pointer
		// (GetRelease) hits the DB, keeping publishes immediately effective.
		cachedControlStore := agentregistry.NewCachingAgentConfigControlStore(controlStore, agentregistry.DefaultConfigVersionCacheSize)
		released, releaseErr := agentregistry.NewReleaseRegistry(registry, cachedControlStore, agentregistry.ConfigEnvironment(cfg.Environment))
		if releaseErr != nil {
			return nil, releaseErr
		}
		runtimeRegistry = released
	}
	if !dependencies.agentsFromDatabase {
		// YAML 加载模式下 Build 期预检 agent 声明的扩展绑定存在性
		//（Options + 全集），fail-closed。预检只消费本次文件快照，不能
		// 全表扫描 Registry 历史版本；历史行的旧 schema/hash 与当前 YAML
		// 声明无关，也不应阻断当前 Composition。DB 动态发布无法静态预检，
		// 由运行期解析兜底。
		if err := preflightFileAgentExtensionBindings(ctx, agentLoader, dependencies.extensions); err != nil {
			return nil, fmt.Errorf("load file agents for extension binding preflight: %w", err)
		}
		effective, err := registry.ResolveEffectiveConfig(ctx, agentregistry.ResolveRequest{AgentID: cfg.Runtime.DefaultAgentID})
		if err != nil {
			return nil, fmt.Errorf("resolve default agent %q: %w", cfg.Runtime.DefaultAgentID, err)
		}
		if effective.Definition.Runtime.Mode == agentruntime.RuntimeModeWorkflow || effective.Definition.Runtime.Mode == agentruntime.RuntimeModeGraph {
			return nil, fmt.Errorf("default agent mode %q has no installed executor", effective.Definition.Runtime.Mode)
		}
	}

	ledger := storageMessageLedger{messages: stores.Messages, artifacts: artifacts}
	snapshots := contextpkg.SnapshotManager{
		Ledger: ledger, Store: snapshotStore,
		IDs:         observability.NewULIDGenerator("ctx").NewRequestID,
		MaxMessages: cfg.Runtime.ContextMessages,
	}
	runtimePromptResolver, err := agentregistry.NewRuntimePromptResolver(promptResolver)
	if err != nil {
		return nil, err
	}
	assembler := agentruntime.NewDefaultRuntimeContextAssembler(nil)
	assembler.ContextSnapshots = agentruntime.LedgerContextSnapshotProvider{Manager: snapshots}
	assembler.CapabilitySnapshots = capabilities.Snapshots
	assembler.SystemPrompts = runtimePromptResolver
	assembler.TokenBudgets = configModelTokenBudgetAllocator{models: models}
	if dependencies.tokenBudgets != nil {
		assembler.TokenBudgets = dependencies.tokenBudgets
	}
	compaction := agentruntime.NewContextCompactionServices(nil)
	compaction.ConfigureAssembler(assembler)

	nativeRuntime := agentruntime.NewNativeDirectRuntime(gatewayModel, capabilities.Tools, capabilities.Rebuilder)
	runtimeService := agentruntime.NewRuntimeService(nativeRuntime, state, logger, tracer)
	turnEnvironment := kernel.TurnEnvironment{
		Environment:    string(cfg.Environment),
		SDKVersion:     kernel.SDKContractVersion,
		SchemaVersions: kernel.CanonicalSchemaVersions(),
	}
	// BeforeModelHook 扩展在 native direct 路径的模型前钩子（方案 3.3）。
	nativeRuntime.ModelInputTransform = newExtensionNativeModelTransform(dependencies.extensions, turnEnvironment)
	// OutputValidator 扩展桥接为 runtime.before_response hook（R2c）：校验
	// 发生在 final_response 落盘之前，fail-closed 拒绝会使 Run 失败。
	if err := registerExtensionRuntimeHooks(runtimeService, dependencies.extensions, turnEnvironment); err != nil {
		return nil, err
	}
	runtimeService.ControlRequests = runtimeControlCreator{service: control.New(stores, nil), tickets: dependencies.controlTickets}
	runtimeService.Assembler = assembler
	runtimeService.Writer = writer
	subAgents := &subAgentInvokerProxy{}
	environment := compaction.ConfigureEnvironment(agentruntime.RuntimeEnvironment{
		Models: gatewayModel, Tools: capabilities.Tools, SubAgents: subAgents,
		Checkpoints: agentruntime.StaticRuntimeCheckpointStore{Store: &runtimestore.ArtifactCheckpointStore{Metadata: stores.Checkpoints, Artifacts: artifacts}},
	})
	einoRuntime := agentruntime.NewEinoRuntime(
		agentruntime.NewEinoDeepAgentFactory(), environment, agentruntime.DefaultEinoControlRequestFactory{},
	)
	// BeforeModelHook / ToolCallInterceptor 扩展经 PreModelHandlers 通道
	// 投影为 eino 模型前钩子与工具环绕（方案 3.3 / 3.4），顺序在 compaction
	// handler 之后，保证扩展看到压缩后的最终输入。
	einoRuntime.PreModelHandlers = newExtensionPreModelHandlerResolver(einoRuntime.PreModelHandlers, dependencies.extensions, turnEnvironment)
	runtimeService.RegisterRuntime(agentruntime.RuntimeTypeEino, einoRuntime)

	// The local Composition Root intentionally has no queue worker. Installing
	// an in-memory scheduler alone would accept Runs that nobody consumes.
	// Checkpoint-capable Eino Runs execute inline; explicit scheduled Runs fail
	// closed until a scheduler, durable request store and owned worker are wired.
	runDispatcher := dispatcher.NewRunDispatcher(runtimeService, nil, dispatcher.Policy{InlineMaxDuration: 2 * time.Second}, logger, tracer)
	runDispatcher.Readiness = formalExecutorReadiness{runtime: runtimeService}
	runDispatcher.InlineModes[dispatcher.ExecutionModeSingleAgent] = true
	runDispatcher.InlineModes[dispatcher.ExecutionModeDeepAgent] = true
	orchestratorService, err := orchestrator.NewService(orchestrator.Options{
		Registry: runtimeRegistry, Dispatcher: runDispatcher, Runtime: runtimeService,
		Writer: writer, Logger: logger, Tracer: tracer,
	})
	if err != nil {
		return nil, err
	}
	var remote agentgateway.RemoteA2AProvider
	if cfg.Features.A2A {
		remote, err = agentgateway.NewA2AProvider(agentgateway.A2AProviderConfig{})
		if err != nil {
			return nil, fmt.Errorf("build A2A provider: %w", err)
		}
	}
	// 子 Run 恢复桥：gateway 先于 dispatcher 构造，dispatcher 就绪后回填。
	resumeBridge := &childResumeBridge{}
	gateway, err := agentgateway.NewService(agentgateway.ServiceConfig{
		Targets: registryGatewayTargetResolver{registry: runtimeRegistry, runs: stores.Runs},
		Local: agentgateway.RuntimeLocalProvider{Executor: localChildRunExecutor{
			orchestrator: orchestratorService, runs: stores.Runs, snapshots: snapshots,
			ids: observability.NewULIDGenerator("child_run"), broker: broker,
			resume: resumeBridge,
		}},
		Remote: remote, Logger: logger, Tracer: tracer,
	})
	if err != nil {
		return nil, fmt.Errorf("build Agent Gateway: %w", err)
	}
	if err := subAgents.Set(gateway); err != nil {
		return nil, err
	}
	lifecycle := newRunLifecycle()
	protocolDispatcher := &formalRunDispatcher{
		orchestrator: orchestratorService, broker: broker, hotBuffer: hotBuffer,
		runs: stores.Runs, registry: runtimeRegistry, artifacts: artifacts, ledger: ledger, snapshots: snapshotStore,
		// Resume 前置 Answer 环节（方案 4.1 链路验证暴露的断点）：SDK 的
		// ControlTicket 密文在此解封为原始 resume token，先把 control request
		// 置为 answered 再进入 runtime claim。
		controls: control.New(stores, nil), tickets: dependencies.controlTickets,
		defaultAgentID: cfg.Runtime.DefaultAgentID,
		slots:          make(chan struct{}, cfg.Runtime.MaxConcurrent), logger: logger,
		ids: observability.NewULIDGenerator("binding"), lifecycle: lifecycle,
	}
	// 回填子 Run 恢复桥：gateway 的 ResumeChild 经此走 canonical Resume 正门。
	resumeBridge.dispatcher.Store(protocolDispatcher)
	return &formalComposition{
		runs:       storage.NewRunService(stores, contextSnapshotBuilder{manager: snapshots}),
		dispatcher: protocolDispatcher, registry: registry, runtimeRegistry: runtimeRegistry, configControl: configControl, lifecycle: lifecycle,
		// skillService 只喂管理面（serverDeps.ManagedSkills）：file 模式下为
		// DB-only 视图，运行时技能快照仍走 capabilities.Snapshots（ADR-0005/0006）。
		mcpService: capabilities.MCP, managedMCP: capabilities.ManagedMCP, mcpOAuth: capabilities.MCPOAuth, toolConfig: capabilities.ToolConfig, httpTools: capabilities.HTTPTools, skillService: capabilities.AdminSkills,
		toolRegistry:      toolListerOrNil(capabilities.Registry),
		boundToolHandlers: capabilities.BoundToolHandlers,
		prompts:           adminPromptStore, snapshots: snapshots,
	}, nil
}

func preflightFileAgentExtensionBindings(
	ctx context.Context,
	loader agentregistry.Loader,
	catalog *kernel.ExtensionCatalog,
) error {
	if loader == nil {
		return errors.New("file agent loader is not configured")
	}
	configs, err := loader.Load(ctx)
	if err != nil {
		return err
	}
	return preflightAgentExtensionBindings(configs, catalog)
}

// toolListerOrNil exposes a tool catalog for management/UI listing only when
// the registry supports enumeration. A registry that cannot list yields a nil
// interface so the endpoint degrades to 501 rather than panicking.
func toolListerOrNil(registry toolgateway.ToolRegistry) toolgateway.ToolLister {
	if lister, ok := registry.(toolgateway.ToolLister); ok {
		return lister
	}
	return nil
}

type formalRunDispatcher struct {
	orchestrator   orchestrator.Orchestrator
	runs           storage.RunStore
	registry       runtimeAgentRegistry
	artifacts      *artifact.Store
	ledger         contextpkg.MessageLedger
	snapshots      contextpkg.SnapshotStore
	broker         protocol.EventBroker
	hotBuffer      protocol.HotStreamBuffer
	controls       *control.Service
	tickets        *controlticket.Codec
	defaultAgentID string
	slots          chan struct{}
	logger         observability.StructuredLogger
	ids            observability.IDGenerator
	lifecycle      *runLifecycle
}

func (d *formalRunDispatcher) Resume(ctx context.Context, req control.ResumeRequest) error {
	runCtx, events, runID, releaseAll, err := d.resumeCore(ctx, req, false)
	if err != nil {
		return err
	}
	d.drainResumed(runCtx, runID, events, releaseAll)
	return nil
}

// ResumeChildInline 是 Agent Gateway 的受托子 Run 恢复入口：走与公共 Resume
// 完全相同的 canonical 链路（ticket 解封 → Answer → runtime claim），但允许
// ParentRunID 非空，且把事件流交还调用方 inline 消费（gateway 收集子结果）。
// 调用方消费完毕后必须调用 release。
func (d *formalRunDispatcher) ResumeChildInline(ctx context.Context, req control.ResumeRequest) (<-chan observability.AgentEvent, func(), error) {
	_, events, _, releaseAll, err := d.resumeCore(ctx, req, true)
	if err != nil {
		return nil, nil, err
	}
	return events, releaseAll, nil
}

// drainResumed 复用 drain 的泵送逻辑；slot 释放已并入 releaseAll。
func (d *formalRunDispatcher) drainResumed(ctx context.Context, runID string, events <-chan observability.AgentEvent, releaseAll func()) {
	go func() {
		defer releaseAll()
		var deltaSequence int64
		for event := range events {
			if d.hotBuffer != nil && observability.IsEphemeralDelta(event.EventType) && event.Visibility == observability.VisibilityUserVisible {
				deltaSequence++
				if err := d.hotBuffer.Push(ctx, runID, protocol.HotDelta{DeltaSeq: deltaSequence, Content: hotDeltaContent(event), CreatedAt: event.CreatedAt}); err != nil {
					d.logger.Error(ctx, "runtime hot stream publish failed", err, observability.String("run_id", runID))
				}
			}
			if d.broker != nil {
				if err := d.broker.Publish(ctx, event); err != nil {
					d.logger.Error(ctx, "runtime event publish failed", err, observability.String("run_id", runID))
				}
			}
		}
	}()
}

// resumeCore 是 Resume 的共享核心。返回的 releaseAll 同时释放 run 生命周期
// 与调度 slot；出错路径内部已完成释放。
func (d *formalRunDispatcher) resumeCore(ctx context.Context, req control.ResumeRequest, allowChild bool) (context.Context, <-chan observability.AgentEvent, string, func(), error) {
	if d.orchestrator == nil || d.runs == nil || d.registry == nil {
		return nil, nil, "", nil, errors.New("resume composition unavailable")
	}
	select {
	case d.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, "", nil, ctx.Err()
	default:
		return nil, nil, "", nil, errors.New("runtime dispatch capacity exhausted")
	}
	release := true
	defer func() {
		if release {
			<-d.slots
		}
	}()
	run, err := d.runs.Get(ctx, req.RunID)
	if err != nil {
		return nil, nil, "", nil, err
	}
	if run.ParentRunID != "" && !allowChild {
		return nil, nil, "", nil, storage.NewError(storage.ErrUnsupportedCapability, "platform child run resume is unsupported")
	}
	if run.ConfigSnapshotRef == "" {
		return nil, nil, "", nil, errors.New("run config snapshot is missing")
	}
	effective, err := d.registry.GetConfigSnapshotForTenant(ctx, run.TenantID, run.ConfigSnapshotRef)
	if err != nil {
		return nil, nil, "", nil, err
	}
	if effective.Definition.AgentID != run.AgentID || effective.ConfigSnapshotRef != run.ConfigSnapshotRef {
		return nil, nil, "", nil, errors.New("frozen agent config does not match run binding")
	}
	// SDK 侧的 ControlTicket 是 codec 密文：解封出原始 resume token 并校验
	// 票据与 Run/Request 的绑定；解封失败时按原始 token 兜底（内部调用方）。
	resumeToken := req.ResumeToken
	if d.tickets != nil && req.ResumeToken != "" {
		if claims, expiresAt, openErr := d.tickets.Open(req.ResumeToken); openErr == nil {
			if !time.Now().Before(expiresAt) || claims.RunID != run.RunID || claims.RequestID != req.RequestID || claims.TenantID != run.TenantID {
				return nil, nil, "", nil, storage.NewError(storage.ErrPermissionDenied, "control ticket is invalid or expired")
			}
			resumeToken = claims.ResumeToken
		}
	}
	// 先持久化内联答复，再 Answer（pending → answered +
	// control_response_received 事件）。这样 SDK Resume 与 hosted/embedded
	// control response 入口共享同一条可审计、可回放的 ControlResponse 事实。
	responseRef := req.ResponseRef
	if responseRef == "" && len(req.Response) > 0 && d.controls != nil {
		if d.artifacts == nil {
			return nil, nil, "", nil, errors.New("artifact store unavailable for inline control response")
		}
		artifactCtx := artifact.ContextWithActor(ctx, artifact.Actor{
			TenantID: run.TenantID, SessionID: run.SessionID, RunID: run.RunID,
			Role: artifact.ActorRuntime,
		})
		meta, putErr := d.artifacts.Put(artifactCtx, artifact.PutArtifactRequest{
			TenantID: run.TenantID, SessionID: run.SessionID, RunID: run.RunID,
			OwnerModule: artifact.OwnerModuleProtocol, OwnerID: req.RequestID,
			ArtifactType: artifact.ArtifactTypeControlResponse, MimeType: "application/json",
			Visibility: artifact.VisibilityInternal, Content: bytes.NewReader(req.Response),
			RetentionPolicy: artifact.RetentionRunTTL, CreatedBy: "control_response",
			IdempotencyKey: "control-response:" + req.RequestID,
			Metadata:       map[string]string{"request_id": req.RequestID, "schema_version": "harness.control_response.v1"},
		})
		if putErr != nil {
			return nil, nil, "", nil, putErr
		}
		responseRef = meta.ArtifactRef
	}
	if d.controls != nil {
		if _, answerErr := d.controls.Answer(ctx, control.AnswerRequest{
			RequestID: req.RequestID, ResumeToken: resumeToken, ResponseRef: responseRef,
		}); answerErr != nil {
			return nil, nil, "", nil, answerErr
		}
	}
	trace := observability.MustTraceContext(ctx)
	trace.SessionID, trace.RunID, trace.AgentID = run.SessionID, run.RunID, run.AgentID
	var payload []byte
	if len(req.Response) > 0 {
		// 内联答复：SDK Resume 直接携带的小 JSON payload；ResponseRef 非空
		// 时以 artifact 为准覆盖。
		payload = append([]byte(nil), req.Response...)
	}
	if responseRef != "" {
		if d.artifacts == nil {
			return nil, nil, "", nil, errors.New("artifact store unavailable for control response")
		}
		artifactCtx := artifact.ContextWithActor(ctx, artifact.Actor{TenantID: run.TenantID, SessionID: run.SessionID, RunID: run.RunID, Role: artifact.ActorRuntime})
		object, getErr := d.artifacts.Get(artifactCtx, responseRef, artifact.GetOptions{Purpose: artifact.PurposeReplay})
		if getErr != nil {
			return nil, nil, "", nil, getErr
		}
		if object.Meta.ArtifactType != artifact.ArtifactTypeControlResponse {
			_ = object.Content.Close()
			return nil, nil, "", nil, errors.New("control response ref has invalid artifact type")
		}
		payload, err = io.ReadAll(object.Content)
		closeErr := object.Content.Close()
		if err != nil {
			return nil, nil, "", nil, err
		}
		if closeErr != nil {
			return nil, nil, "", nil, closeErr
		}
	}
	resultVisibility := observability.VisibilityUserVisible
	if run.ParentRunID != "" {
		resultVisibility = observability.VisibilityInternal
	}
	runCtx, releaseRun, err := d.startRun(observability.WithTraceContext(ctx, trace))
	if err != nil {
		return nil, nil, "", nil, err
	}
	events, err := d.orchestrator.Resume(runCtx, agentruntime.ResumeRequest{
		SessionID: run.SessionID, RunID: run.RunID, ParentRunID: run.ParentRunID, Definition: effective.Definition,
		ContextSnapshotRef: run.ContextSnapshotRef, ConfigSnapshotRef: effective.ConfigSnapshotRef,
		ConfigHash: effective.ConfigHash, AgentBindingID: run.AgentBindingID, TenantID: run.TenantID,
		CheckpointID: req.CheckpointID, ControlRequestID: req.RequestID,
		ResumeToken: resumeToken, ControlPayload: payload, Trace: trace,
		ResultVisibility: resultVisibility,
	})
	if err != nil {
		releaseRun()
		return nil, nil, "", nil, err
	}
	if events == nil {
		releaseRun()
		return nil, nil, "", nil, errors.New("runtime resume returned no events")
	}
	release = false
	releaseAll := func() {
		releaseRun()
		<-d.slots
	}
	return runCtx, events, run.RunID, releaseAll, nil
}

func (d *formalRunDispatcher) Cancel(ctx context.Context, sessionID, runID, reason string) error {
	if d.orchestrator == nil {
		return errors.New("orchestrator unavailable")
	}
	return d.orchestrator.Cancel(ctx, agentruntime.CancelRequest{SessionID: sessionID, RunID: runID, Reason: reason})
}

func (d *formalRunDispatcher) Dispatch(ctx context.Context, turn *storage.OpenTurnResult) error {
	if turn == nil || turn.Run == nil || turn.UserMessage == nil {
		return storage.NewError(storage.ErrInvalidArgument, "open turn result, run and user message are required")
	}
	select {
	case d.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	default:
		return errors.New("runtime dispatch capacity exhausted")
	}
	agentID := turn.Run.AgentID
	if agentID == "" {
		agentID = d.defaultAgentID
	}
	trace := observability.MustTraceContext(ctx)
	trace.SessionID, trace.RunID, trace.AgentID = turn.Run.SessionID, turn.Run.RunID, agentID
	frozenInput, err := d.frozenCurrentInput(observability.WithTraceContext(ctx, trace), turn)
	if err != nil {
		<-d.slots
		return err
	}
	request := orchestrator.RunRequest{
		BindingRequest: agentbinding.BindingRequest{
			SchemaVersion: agentbinding.BindingSchemaVersion,
			BindingID:     d.ids.NewRequestID(), SessionID: turn.Run.SessionID, RunID: turn.Run.RunID,
			Request: &agentbinding.Selection{AgentID: agentID},
		},
		Input:              []agentruntime.Message{frozenInput},
		ContextSnapshotRef: turn.ContextSnapshotRef,
		// 入口层预回合管线产出的业务 fragment 随 Turn 透传进 ModelContext 装配。
		ExternalFragments: turn.ContextFragments,
		// 入口层随 Turn 透传的 per-Run ScopedData 冻结快照，经 orchestrator
		// 进入 RunRequest.ScopedData，供 BeforeModelHook 等运行期扩展只读消费。
		ScopedData: scopedDataFromTurn(turn.ScopedData),
		UserID:     trace.UserID, TenantID: turn.Run.TenantID, Trace: trace,
	}
	runCtx, releaseRun, err := d.startRun(observability.WithTraceContext(ctx, trace))
	if err != nil {
		<-d.slots
		return err
	}
	result, err := d.orchestrator.Run(runCtx, request)
	if err != nil {
		releaseRun()
		<-d.slots
		return err
	}
	if result == nil || result.Events == nil {
		releaseRun()
		<-d.slots
		return errors.New("formal composition only accepts inline execution until scheduler worker is installed")
	}
	d.drain(runCtx, turn.Run.RunID, result.Events, releaseRun)
	return nil
}

func (d *formalRunDispatcher) frozenCurrentInput(ctx context.Context, turn *storage.OpenTurnResult) (agentruntime.Message, error) {
	if d.ledger == nil || d.snapshots == nil || turn.ContextSnapshotRef == "" {
		return agentruntime.Message{}, errors.New("frozen current-input dependencies are unavailable")
	}
	snapshot, err := d.snapshots.Load(ctx, turn.ContextSnapshotRef)
	if err != nil {
		return agentruntime.Message{}, err
	}
	if snapshot.SessionID != turn.Run.SessionID || snapshot.RunID != turn.Run.RunID || !containsString(snapshot.MessageIDs, turn.UserMessage.ID) {
		return agentruntime.Message{}, errors.New("current user message is not part of the frozen context snapshot")
	}
	messages, err := d.ledger.GetByIDs(ctx, turn.Run.SessionID, []string{turn.UserMessage.ID})
	if err != nil {
		return agentruntime.Message{}, err
	}
	if len(messages) != 1 || messages[0].ID != turn.UserMessage.ID || messages[0].Role != contextpkg.RoleUser {
		return agentruntime.Message{}, errors.New("frozen current user message could not be resolved exactly")
	}
	return agentruntime.Message{
		ID: messages[0].ID, IdempotencyKey: messages[0].IdempotencyKey,
		Role: string(messages[0].Role), Content: messages[0].Content,
		// Attachment bytes remain in Artifact Store. The immutable snapshot facts
		// are projected into the current user message so the model receives actual
		// image/file parts rather than attachment metadata alone.
		Parts: frozenInputParts(messages[0], snapshot.Attachments),
	}, nil
}

func frozenInputParts(message contextpkg.Message, attachments []contextpkg.AttachmentFact) []contextpkg.ContentPart {
	if len(message.Parts) > 0 {
		return append([]contextpkg.ContentPart(nil), message.Parts...)
	}
	return contextpkg.MessagePartsFromAttachments(message.Content, attachments)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// scopedDataFromTurn 把随 Turn 透传的 ScopedData 条目还原为 Runtime 的
// Run 级 ScopedData 快照（空 key 条目丢弃，与 kernel 管线的 key 非空约束对齐）。
func scopedDataFromTurn(items []storage.ScopedDataItem) agentruntime.ScopedData {
	if len(items) == 0 {
		return agentruntime.ScopedData{}
	}
	run := make(map[string]agentruntime.ScopedDataItem, len(items))
	for _, item := range items {
		if item.Key == "" {
			continue
		}
		run[item.Key] = agentruntime.ScopedDataItem{
			Source:     item.Source,
			Visibility: item.Visibility,
			Value:      append(json.RawMessage(nil), item.Value...),
			Ref:        item.Ref,
			Hash:       item.Hash,
		}
	}
	if len(run) == 0 {
		return agentruntime.ScopedData{}
	}
	return agentruntime.ScopedData{Run: run}
}

func (d *formalRunDispatcher) startRun(ctx context.Context) (context.Context, func(), error) {
	if d.lifecycle != nil {
		return d.lifecycle.Start(ctx)
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return runCtx, cancel, nil
}

func (d *formalRunDispatcher) drain(ctx context.Context, runID string, events <-chan observability.AgentEvent, releaseRun func()) {
	go func() {
		defer releaseRun()
		defer func() { <-d.slots }()
		var deltaSequence int64
		for event := range events {
			if d.hotBuffer != nil && observability.IsEphemeralDelta(event.EventType) && event.Visibility == observability.VisibilityUserVisible {
				deltaSequence++
				if err := d.hotBuffer.Push(ctx, runID, protocol.HotDelta{DeltaSeq: deltaSequence, Content: hotDeltaContent(event), CreatedAt: event.CreatedAt}); err != nil {
					d.logger.Error(ctx, "runtime hot stream publish failed", err, observability.String("run_id", runID))
				}
			}
			if d.broker != nil {
				if err := d.broker.Publish(ctx, event); err != nil {
					d.logger.Error(ctx, "runtime event publish failed", err, observability.String("run_id", runID))
				}
			}
		}
	}()
}
