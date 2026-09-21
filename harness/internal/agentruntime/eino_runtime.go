package agentruntime

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

var (
	ErrEinoAgentFactoryMissing       = errors.New("eino managed agent factory missing")
	ErrEinoCheckpointResolverMissing = errors.New("eino checkpoint resolver missing")
	ErrEinoCheckpointStoreMissing    = errors.New("eino checkpoint store missing")
	ErrEinoControlFactoryMissing     = errors.New("eino control request factory missing")
	ErrEinoExecutionFactoryMissing   = errors.New("eino execution factory missing")
	ErrEinoModeUnsupported           = errors.New("eino runtime mode unsupported")
	ErrEinoPackageMismatch           = errors.New("eino model context package mismatch")
	ErrEinoInterruptBindingMissing   = errors.New("eino interrupt binding missing")
	ErrEinoResumePayloadInvalid      = errors.New("eino resume payload invalid")
	ErrEinoMessageRoleInvalid        = errors.New("eino model context message role invalid")
)

const (
	EinoRuntimeVersion   = "v0.9.12"
	EinoAdapterVersion   = "v1"
	EinoCheckpointFormat = "eino.adk.gob"
)

type EinoRuntime struct {
	Agents      EinoManagedAgentFactory
	Environment RuntimeEnvironment
	// ToolInfos is used only by legacy/local packages without Tool or MCP snapshots.
	// Production schemas are consumed only from the frozen ModelContextPackage.
	ToolInfos        EinoToolInfoResolver
	PreModelHandlers EinoPreModelHandlerResolver
	Controls         EinoControlRequestFactory
	ResumeMap        EinoResumeMapper
	Executions       EinoExecutionFactory
	IDs              observability.IDGenerator
	Clock            func() time.Time

	activeMu sync.Mutex
	active   map[string]adk.AgentCancelFunc

	interruptMu sync.Mutex
	interrupts  map[string]WaitingControlRequest
}

var (
	_ AgentRuntime                    = (*EinoRuntime)(nil)
	_ RuntimeInterruptBindingProvider = (*EinoRuntime)(nil)
)

func NewEinoRuntime(agents EinoManagedAgentFactory, environment RuntimeEnvironment, controls EinoControlRequestFactory) *EinoRuntime {
	if environment.ContextCompactors == nil && environment.PreModelCompactor == nil {
		environment.ContextCompactors = NewDefaultContextCompactorProvider(nil)
	}
	environment.Models = environment.GovernedModelInvoker()
	return &EinoRuntime{
		Agents:           agents,
		Environment:      environment,
		PreModelHandlers: DefaultEinoPreModelHandlerResolver{},
		Controls:         controls,
		ResumeMap:        DefaultEinoResumeMapper{},
		Executions:       ADKEinoExecutionFactory{},
		IDs:              observability.NewULIDGenerator("eino"),
		Clock:            time.Now,
		active:           make(map[string]adk.AgentCancelFunc),
		interrupts:       make(map[string]WaitingControlRequest),
	}
}

func (*EinoRuntime) Name() string { return string(RuntimeTypeEino) }

func (r *EinoRuntime) Descriptor(ctx context.Context) RuntimeDescriptor {
	return RuntimeDescriptor{
		Name:             RuntimeTypeEino,
		RuntimeVersion:   EinoRuntimeVersion,
		AdapterVersion:   EinoAdapterVersion,
		Governance:       RuntimeGovernanceBridged,
		CheckpointFormat: EinoCheckpointFormat,
		Capabilities:     r.Capabilities(ctx),
	}
}

func (r *EinoRuntime) Capabilities(context.Context) RuntimeCapabilities {
	ready := r != nil && r.Environment.Checkpoints != nil && r.Controls != nil
	return RuntimeCapabilities{
		Streaming:      true,
		Resume:         ready,
		ToolCall:       r != nil && r.Environment.Tools != nil,
		SubAgent:       r != nil && r.Environment.SubAgents != nil,
		Checkpoint:     ready,
		ControlRequest: ready,
		Workflow:       true,
		DeepAgent:      true,
		Cancellation:   true,
		A2A:            r != nil && r.Environment.SubAgents != nil,
		MCP:            r != nil && r.Environment.Tools != nil,
	}
}

func (r *EinoRuntime) ValidateConfig(_ context.Context, def AgentDefinition) error {
	if r == nil || r.Agents == nil {
		return ErrEinoAgentFactoryMissing
	}
	if r.Environment.Checkpoints == nil {
		return ErrEinoCheckpointResolverMissing
	}
	if r.Controls == nil {
		return ErrEinoControlFactoryMissing
	}
	if r.Executions == nil {
		return ErrEinoExecutionFactoryMissing
	}
	if def.AgentID == "" {
		return errors.New("agent_id required")
	}
	if def.Version == "" {
		return errors.New("agent version required")
	}
	policy, err := NormalizeContextCompactionPolicy(def.ContextCompaction)
	if err != nil {
		return err
	}
	if r.PreModelHandlers == nil && policy.SemanticSummary != SemanticSummaryDisabled {
		return ErrRuntimePreModelCompactorMissing
	}
	switch def.Runtime.Mode {
	case RuntimeModeReact, RuntimeModeDeepAgent, RuntimeModePlanExecute:
		return nil
	case RuntimeModeWorkflow:
		if def.Workflow == nil || def.Workflow.EntryNode == "" || len(def.Workflow.Nodes) == 0 {
			return errors.New("workflow definition required")
		}
		return nil
	case RuntimeModeGraph:
		if def.Graph == nil || def.Graph.EntryNode == "" || len(def.Graph.Nodes) == 0 {
			return errors.New("graph definition required")
		}
		return nil
	default:
		return fmt.Errorf("%w: %s", ErrEinoModeUnsupported, def.Runtime.Mode)
	}
}

func (r *EinoRuntime) Build(ctx context.Context, def AgentDefinition) (AgentHandle, error) {
	if err := r.ValidateConfig(ctx, def); err != nil {
		return AgentHandle{}, err
	}
	clock := r.Clock
	if clock == nil {
		clock = time.Now
	}
	binding, err := newRuntimeBinding(ctx, r, def, runtimeBindingFacts{})
	if err != nil {
		return AgentHandle{}, err
	}
	return AgentHandle{Definition: def, Runtime: RuntimeTypeEino, Binding: binding, BuiltAt: clock()}, nil
}

func (r *EinoRuntime) Run(ctx context.Context, req RunRequest) (<-chan observability.AgentEvent, error) {
	pkg, ok := ModelContextPackageFrom(ctx)
	if !ok {
		return nil, ErrModelContextPackageMissing
	}
	if err := r.validatePackageAndConfig(ctx, req, pkg); err != nil {
		return nil, err
	}
	execution, err := r.resolveExecution(ctx, req, pkg)
	if err != nil {
		return nil, err
	}
	executionCtx, stop := context.WithCancel(ctx)
	bridge := newRuntimeEventBridge(64)
	executionCtx = WithRuntimeEventEmitter(executionCtx, bridge)
	checkpointID := einoCheckpointID(req.RunID)
	iterator, cancel, err := execution.Run(executionCtx, einoMessages(pkg), checkpointID)
	if err != nil {
		stop()
		return nil, err
	}
	if iterator == nil {
		stop()
		return nil, errors.New("eino runner returned nil iterator")
	}
	r.setActive(req.RunID, cancel)
	out := make(chan observability.AgentEvent, 16)
	go r.pipe(executionCtx, stop, req, checkpointID, iterator, bridge, out)
	return out, nil
}

func (r *EinoRuntime) Resume(ctx context.Context, req ResumeRequest) (<-chan observability.AgentEvent, error) {
	runReq := runRequestFromResume(req)
	pkg, ok := ModelContextPackageFrom(ctx)
	if !ok {
		return nil, ErrModelContextPackageMissing
	}
	if err := r.validatePackageAndConfig(ctx, runReq, pkg); err != nil {
		return nil, err
	}
	execution, err := r.resolveExecution(ctx, runReq, pkg)
	if err != nil {
		return nil, err
	}
	var params *adk.ResumeParams
	if r.ResumeMap != nil {
		params, err = r.ResumeMap.Map(ctx, req)
		if err != nil {
			return nil, err
		}
	}
	executionCtx, stop := context.WithCancel(ctx)
	executionCtx = withEinoTargetedResume(executionCtx, params != nil && len(params.Targets) > 0)
	executionCtx = withEinoGlobalResumePayload(executionCtx, req.ControlPayload)
	bridge := newRuntimeEventBridge(64)
	executionCtx = WithRuntimeEventEmitter(executionCtx, bridge)
	iterator, cancel, err := execution.Resume(executionCtx, req.CheckpointID, params)
	if err != nil {
		stop()
		return nil, err
	}
	if iterator == nil {
		stop()
		return nil, errors.New("eino runner returned nil iterator")
	}
	r.setActive(req.RunID, cancel)
	out := make(chan observability.AgentEvent, 16)
	go r.pipe(executionCtx, stop, runReq, req.CheckpointID, iterator, bridge, out)
	return out, nil
}

func (r *EinoRuntime) Cancel(_ context.Context, req CancelRequest) error {
	r.activeMu.Lock()
	cancel := r.active[req.RunID]
	r.activeMu.Unlock()
	if cancel == nil {
		return nil
	}
	_, _ = cancel(adk.WithAgentCancelMode(adk.CancelImmediate), adk.WithRecursive())
	return nil
}

func (r *EinoRuntime) Health(context.Context) RuntimeHealth {
	var reason string
	switch {
	case r == nil || r.Agents == nil:
		reason = ErrEinoAgentFactoryMissing.Error()
	case r.Environment.Checkpoints == nil:
		reason = ErrEinoCheckpointResolverMissing.Error()
	case r.Controls == nil:
		reason = ErrEinoControlFactoryMissing.Error()
	case r.Executions == nil:
		reason = ErrEinoExecutionFactoryMissing.Error()
	}
	return RuntimeHealth{Available: reason == "", Reason: reason, CheckedAt: time.Now()}
}

func (r *EinoRuntime) Shutdown(context.Context) error {
	r.activeMu.Lock()
	cancels := make([]adk.AgentCancelFunc, 0, len(r.active))
	for _, cancel := range r.active {
		cancels = append(cancels, cancel)
	}
	r.activeMu.Unlock()
	for _, cancel := range cancels {
		if cancel != nil {
			_, _ = cancel(adk.WithAgentCancelMode(adk.CancelImmediate), adk.WithRecursive())
		}
	}
	r.interruptMu.Lock()
	clear(r.interrupts)
	r.interruptMu.Unlock()
	return nil
}

func (r *EinoRuntime) TakeInterruptBinding(_ context.Context, event observability.AgentEvent) (WaitingControlRequest, error) {
	key := interruptBindingKey(event.RunID, controlRequestID(event.Payload))
	r.interruptMu.Lock()
	defer r.interruptMu.Unlock()
	binding, ok := r.interrupts[key]
	if !ok {
		return WaitingControlRequest{}, ErrEinoInterruptBindingMissing
	}
	delete(r.interrupts, key)
	return binding, nil
}

func (r *EinoRuntime) validatePackageAndConfig(ctx context.Context, req RunRequest, pkg ModelContextPackage) error {
	if err := r.ValidateConfig(ctx, req.Definition); err != nil {
		return err
	}
	if pkg.SchemaVersion != ModelContextPackageSchemaVersion || pkg.PackageID == "" || validateModelContextIntegrity(pkg) != nil || pkg.Run.RunID != req.RunID || pkg.Run.SessionID != req.SessionID {
		return fmt.Errorf("%w: run_id=%s package_run_id=%s", ErrEinoPackageMismatch, req.RunID, pkg.Run.RunID)
	}
	if err := validateContextCompactionPolicyBinding(req.Definition, pkg); err != nil {
		return fmt.Errorf("%w: %v", ErrEinoPackageMismatch, err)
	}
	if req.ParentRunID != "" && len(pkg.Capabilities.SubAgents) > 0 {
		return fmt.Errorf("%w: %w", ErrEinoPackageMismatch, ErrNestedPlatformSubAgentForbidden)
	}
	for _, message := range pkg.Messages.ConversationWindow {
		switch schema.RoleType(message.Role) {
		case schema.System, schema.User, schema.Assistant, schema.Tool:
		default:
			return NewRuntimeError(ErrorSchemaValidation, "EINO_MESSAGE_ROLE_INVALID", "model context contains an unsupported message role").WithCause(fmt.Errorf("%w: %s", ErrEinoMessageRoleInvalid, message.Role))
		}
	}
	return nil
}

func (r *EinoRuntime) resolveExecution(ctx context.Context, req RunRequest, pkg ModelContextPackage) (EinoExecution, error) {
	requirements := managedRuntimeRequirements(req.Definition, pkg)
	if err := r.Environment.Validate(requirements); err != nil {
		return nil, NewRuntimeError(ErrorDependencyUnavailable, "EINO_RUNTIME_ENVIRONMENT_INCOMPLETE", "eino managed runtime environment is incomplete").WithDependency("runtime_environment").WithCause(err)
	}
	var chatModel *EinoChatModelProxy
	if r.Environment.Models != nil {
		var err error
		chatModel, err = NewEinoChatModelProxy(r.Environment.GovernedModelInvoker(), pkg)
		if err != nil {
			return nil, NewRuntimeError(ErrorDependencyUnavailable, "EINO_CHAT_MODEL_PROXY_FAILED", "failed to create harness chat model proxy").WithDependency("model_gateway").WithCause(err)
		}
	}
	platformSubAgents, err := r.platformSubAgentProxies(req, pkg)
	if err != nil {
		return nil, NewRuntimeError(ErrorDependencyUnavailable, "EINO_SUB_AGENT_PROXY_FAILED", "failed to create harness sub-agent proxy").WithDependency("agent_gateway").WithCause(err)
	}
	platformTools, err := r.platformToolProxies(ctx, req, pkg)
	if err != nil {
		return nil, NewRuntimeError(ErrorDependencyUnavailable, "EINO_TOOL_PROXY_FAILED", "failed to create harness tool proxy").WithDependency("tool_gateway").WithCause(err)
	}
	preModelHandlers, err := r.resolvePreModelHandlers(ctx, req.Definition, pkg, req.ScopedData)
	if err != nil {
		return nil, err
	}
	agent, err := r.Agents.Build(ctx, EinoManagedAgentBuildRequest{
		Definition:        req.Definition,
		Package:           pkg,
		Environment:       r.Environment,
		ChatModel:         chatModel,
		PlatformTools:     platformTools,
		PreModelHandlers:  append([]adk.ChatModelAgentMiddleware(nil), preModelHandlers...),
		SystemPrompt:      einoSystemPrompt(pkg),
		PlatformSubAgents: platformSubAgents,
		InternalAgents:    EinoInternalAgentDecorator{},
	})
	if err != nil {
		return nil, NewRuntimeError(ErrorDependencyUnavailable, "EINO_AGENT_BUILD_FAILED", "failed to build managed eino agent").WithRetryable(true).WithDependency("agent_factory").WithCause(err)
	}
	if agent == nil {
		return nil, NewRuntimeError(ErrorSchemaValidation, "EINO_AGENT_MISSING", "eino managed agent factory returned no agent").WithCause(ErrEinoAgentMissing)
	}
	scope := RuntimeCheckpointScope{
		TenantID:       req.TenantID,
		SessionID:      req.SessionID,
		RunID:          req.RunID,
		Runtime:        RuntimeTypeEino,
		RuntimeVersion: EinoRuntimeVersion,
		AdapterVersion: EinoAdapterVersion,
	}
	store, err := r.Environment.Checkpoints.Resolve(ctx, scope)
	if err != nil {
		return nil, NewRuntimeError(ErrorDependencyUnavailable, "EINO_CHECKPOINT_RESOLVE_FAILED", "failed to resolve eino checkpoint store").WithRetryable(true).WithDependency("checkpoint_store").WithCause(err)
	}
	if store == nil {
		return nil, NewRuntimeError(ErrorDependencyUnavailable, "EINO_CHECKPOINT_STORE_MISSING", "eino checkpoint store is unavailable").WithRetryable(true).WithDependency("checkpoint_store").WithCause(ErrEinoCheckpointStoreMissing)
	}
	einoStore, err := newEinoCheckpointStoreProxy(store, scope)
	if err != nil {
		return nil, NewRuntimeError(ErrorCheckpoint, "EINO_CHECKPOINT_PROXY_FAILED", "failed to create harness checkpoint proxy").WithCause(err)
	}
	execution, err := r.Executions.New(ctx, agent, einoStore)
	if err != nil {
		return nil, NewRuntimeError(ErrorRuntime, "EINO_RUNNER_BUILD_FAILED", "failed to build eino runner").WithCause(err)
	}
	return execution, nil
}

func (r *EinoRuntime) resolvePreModelHandlers(ctx context.Context, definition AgentDefinition, pkg ModelContextPackage, scoped ScopedData) ([]adk.ChatModelAgentMiddleware, error) {
	policy, err := NormalizeContextCompactionPolicy(pkg.RuntimeConstraints.CompactionPolicy)
	if err != nil {
		return nil, NewRuntimeError(ErrorSchemaValidation, "EINO_CONTEXT_COMPACTION_POLICY_INVALID", "invalid context compaction policy").WithCause(err)
	}
	baseManifest, err := BuildPreserveManifest(ModelInvokeRequest{Package: pkg})
	if err != nil {
		return nil, NewRuntimeError(ErrorSchemaValidation, "EINO_PRESERVE_MANIFEST_INVALID", "failed to build preserve manifest").WithCause(err)
	}
	var handlers []adk.ChatModelAgentMiddleware
	if r.PreModelHandlers != nil {
		handlers, err = r.PreModelHandlers.Resolve(ctx, EinoPreModelHandlerRequest{
			Definition: definition, Package: pkg, Environment: r.Environment,
			Policy: policy, PreserveManifest: baseManifest,
			// 本 Run 冻结的 scoped-data 快照随 resolver 下传，由扩展投影层转为
			// BeforeModelHook 的只读视图。
			ScopedData: cloneScopedData(scoped),
		})
		if err != nil {
			return nil, NewRuntimeError(ErrorDependencyUnavailable, "EINO_PRE_MODEL_HANDLER_RESOLVE_FAILED", "failed to resolve eino pre-model context handlers").WithDependency("context_engine").WithCause(err)
		}
		for _, handler := range handlers {
			if handler == nil {
				return nil, NewRuntimeError(ErrorSchemaValidation, "EINO_PRE_MODEL_HANDLER_INVALID", "eino pre-model context handler is nil")
			}
		}
	}
	if len(handlers) == 0 {
		return nil, NewRuntimeError(ErrorDependencyUnavailable, "EINO_PRE_MODEL_COMPACTOR_MISSING", "eino pre-model compactor is missing").WithDependency("context_engine").WithCause(ErrRuntimePreModelCompactorMissing)
	}
	return wrapEinoPreModelHandlers(pkg, policy, handlers), nil
}

func (r *EinoRuntime) platformToolProxies(ctx context.Context, req RunRequest, pkg ModelContextPackage) ([]tool.BaseTool, error) {
	refs := append([]string(nil), pkg.Capabilities.Tools...)
	if pkg.Capabilities.ToolSnapshot == nil {
		refs = uniqueStrings(refs)
	}
	if len(refs) == 0 && len(pkg.Capabilities.MCPSnapshots) == 0 {
		return nil, nil
	}
	if r.Environment.Tools == nil {
		return nil, ErrRuntimeEnvironmentDependencyMissing
	}
	var legacyInfos map[string]*schema.ToolInfo
	if len(refs) > 0 {
		if pkg.Capabilities.ToolSnapshot != nil {
			registryDefinitions, err := governedDefinitionsForRefs(refs, pkg.Capabilities.ToolDefinitions)
			if err != nil {
				return nil, err
			}
			if err := validateGovernedToolDefinitions(
				*pkg.Capabilities.ToolSnapshot,
				refs,
				registryDefinitions,
			); err != nil {
				return nil, err
			}
		} else {
			// 兼容 Foundation 的本地装配；生产 assembler 必须提供 ToolSnapshot。
			if r.ToolInfos == nil {
				return nil, ErrRuntimeEnvironmentDependencyMissing
			}
			resolved, err := r.ToolInfos.Resolve(ctx, append([]string(nil), refs...))
			if err != nil {
				return nil, err
			}
			legacyInfos, err = validatedLegacyEinoToolInfos(refs, resolved)
			if err != nil {
				return nil, err
			}
		}
	}
	definitions := make(map[string]ModelToolDefinition, len(pkg.Capabilities.ToolDefinitions))
	for _, definition := range pkg.Capabilities.ToolDefinitions {
		definitions[definition.Name] = definition
	}
	proxies := make([]tool.BaseTool, 0, len(refs)+len(pkg.Capabilities.MCPSnapshots))
	modelNames := make(map[string]struct{})
	for _, ref := range refs {
		name, version := splitVersionedRef(ref)
		if pkg.Capabilities.ToolSnapshot != nil && version == "" {
			return nil, fmt.Errorf("%w: exact tool ref required=%s", ErrEinoToolProxyInfoMissing, ref)
		}
		var info *schema.ToolInfo
		if pkg.Capabilities.ToolSnapshot == nil {
			info = legacyInfos[ref]
			if info == nil {
				return nil, fmt.Errorf("%w: unresolved ref=%s", ErrEinoToolProxyInfoMissing, ref)
			}
		} else {
			definition, ok := definitions[name]
			if !ok {
				return nil, fmt.Errorf("%w: unresolved ref=%s", ErrEinoToolProxyInfoMissing, ref)
			}
			var err error
			info, err = einoToolInfoFromFrozenDefinition(definition, map[string]any{
				"harness_source": "registry", "source_ref": ref, "snapshot_id": pkg.Capabilities.ToolSnapshot.SnapshotID,
			})
			if err != nil {
				return nil, err
			}
		}
		if _, duplicate := modelNames[info.Name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate model tool name=%s", ErrEinoToolProxyInfoMissing, info.Name)
		}
		modelNames[info.Name] = struct{}{}
		proxy, err := NewEinoToolProxy(info, r.Environment.Tools, ToolInvocationRequest{
			SessionID: req.SessionID, RunID: req.RunID, AgentID: req.Definition.AgentID, ToolVersion: version,
			Source: ToolSourceRegistry, SourceRef: ref, ProcessStage: req.Definition.ProcessPresentation.Stage,
		}, r.IDs)
		if err != nil {
			return nil, err
		}
		proxies = append(proxies, proxy)
	}
	for _, snapshot := range pkg.Capabilities.MCPSnapshots {
		mcpProxies, err := r.mcpToolProxies(req, snapshot, modelNames)
		if err != nil {
			return nil, err
		}
		proxies = append(proxies, mcpProxies...)
	}
	for _, snapshot := range pkg.Capabilities.HTTPToolSnapshots {
		proxy, err := r.httpToolProxy(req, snapshot, modelNames)
		if err != nil {
			return nil, err
		}
		proxies = append(proxies, proxy)
	}
	return proxies, nil
}

func (r *EinoRuntime) httpToolProxy(req RunRequest, snapshot HTTPToolSnapshot, modelNames map[string]struct{}) (tool.BaseTool, error) {
	if snapshot.Name == "" || snapshot.DefinitionHash == "" {
		return nil, ErrEinoToolProxyInfoMissing
	}
	if _, duplicate := modelNames[snapshot.Name]; duplicate {
		return nil, fmt.Errorf("%w: duplicate model tool name=%s", ErrEinoToolProxyInfoMissing, snapshot.Name)
	}
	info := &schema.ToolInfo{Name: snapshot.Name, Desc: snapshot.Description, Extra: map[string]any{
		"harness_source": "http_tool", "tool_name": snapshot.Name, "definition_hash": snapshot.DefinitionHash,
	}}
	if len(snapshot.InputSchema) > 0 {
		var inputSchema jsonschema.Schema
		if err := json.Unmarshal(snapshot.InputSchema, &inputSchema); err != nil {
			return nil, fmt.Errorf("%w: invalid http tool schema tool=%s: %v", ErrEinoToolProxyInfoMissing, snapshot.Name, err)
		}
		info.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(&inputSchema)
	}
	proxy, err := NewEinoToolProxy(info, r.Environment.Tools, ToolInvocationRequest{
		SessionID: req.SessionID, RunID: req.RunID, AgentID: req.Definition.AgentID,
		Source: ToolSourceHTTPTool, SourceRef: snapshot.Name, SnapshotRef: snapshot.DefinitionHash,
		ProcessStage: req.Definition.ProcessPresentation.Stage,
	}, r.IDs)
	if err != nil {
		return nil, err
	}
	modelNames[snapshot.Name] = struct{}{}
	return proxy, nil
}

func (r *EinoRuntime) mcpToolProxies(req RunRequest, snapshot mcp.CapabilitySnapshot, modelNames map[string]struct{}) ([]tool.BaseTool, error) {
	if snapshot.ID == "" || snapshot.ServerID == "" || snapshot.CapabilityHash == "" {
		return nil, ErrEinoToolProxyInfoMissing
	}
	proxies := make([]tool.BaseTool, 0, len(snapshot.Tools))
	for _, mcpTool := range snapshot.Tools {
		if mcpTool.Name == "" {
			return nil, ErrEinoToolProxyInfoMissing
		}
		if _, duplicate := modelNames[mcpTool.Name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate model tool name=%s", ErrEinoToolProxyInfoMissing, mcpTool.Name)
		}
		info := &schema.ToolInfo{Name: mcpTool.Name, Desc: mcpTool.Description, Extra: map[string]any{
			"harness_source": "mcp", "server_id": snapshot.ServerID, "snapshot_id": snapshot.ID,
		}}
		if len(mcpTool.InputSchema) > 0 {
			var inputSchema jsonschema.Schema
			if err := json.Unmarshal(mcpTool.InputSchema, &inputSchema); err != nil {
				return nil, fmt.Errorf("%w: invalid mcp schema server=%s tool=%s: %v", ErrEinoToolProxyInfoMissing, snapshot.ServerID, mcpTool.Name, err)
			}
			info.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(&inputSchema)
		}
		proxy, err := NewEinoToolProxy(info, r.Environment.Tools, ToolInvocationRequest{
			SessionID: req.SessionID, RunID: req.RunID, AgentID: req.Definition.AgentID,
			Source: ToolSourceMCP, SourceRef: snapshot.ServerID, SnapshotRef: snapshot.ID,
			ProcessStage: req.Definition.ProcessPresentation.Stage,
		}, r.IDs)
		if err != nil {
			return nil, err
		}
		modelNames[mcpTool.Name] = struct{}{}
		proxies = append(proxies, proxy)
	}
	return proxies, nil
}

func uniqueStrings(input []string) []string {
	seen := make(map[string]struct{}, len(input))
	result := make([]string, 0, len(input))
	for _, value := range input {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func stringSet(input []string) map[string]struct{} {
	result := make(map[string]struct{}, len(input))
	for _, value := range input {
		result[value] = struct{}{}
	}
	return result
}

func splitVersionedRef(ref string) (string, string) {
	index := strings.LastIndex(ref, "@")
	if index <= 0 || index == len(ref)-1 {
		return ref, ""
	}
	return ref[:index], ref[index+1:]
}

func (r *EinoRuntime) platformSubAgentProxies(req RunRequest, pkg ModelContextPackage) ([]adk.Agent, error) {
	if req.ParentRunID != "" {
		if len(pkg.Capabilities.SubAgents) > 0 {
			return nil, ErrNestedPlatformSubAgentForbidden
		}
		return nil, nil
	}
	if len(pkg.Capabilities.SubAgents) == 0 {
		return nil, nil
	}
	if r.Environment.SubAgents == nil {
		return nil, ErrRuntimeEnvironmentDependencyMissing
	}
	proxies := make([]adk.Agent, 0, len(pkg.Capabilities.SubAgents))
	for _, ref := range pkg.Capabilities.SubAgents {
		proxy, err := NewGatewayProxyAgent(GatewayProxyAgentConfig{
			Name:        ref,
			Description: "Harness platform Agent " + ref,
			Invoker:     r.Environment.SubAgents,
			Request: SubAgentInvocationRequest{
				TenantID:                req.TenantID,
				SessionID:               req.SessionID,
				ParentRunID:             req.RunID,
				ParentAgentID:           req.Definition.AgentID,
				ParentAgentVersion:      req.Definition.Version,
				ParentConfigSnapshotRef: req.ConfigSnapshotRef,
				ParentConfigHash:        req.ConfigHash,
				InputParts:              inheritableTaskInputParts(req.Input),
				ScopedData:              cloneScopedData(req.ScopedData),
				Depth:                   subAgentDepth(req.Metadata),
			},
			IDs: r.IDs,
		})
		if err != nil {
			return nil, err
		}
		proxies = append(proxies, proxy)
	}
	return proxies, nil
}

// inheritableTaskInputParts forwards image references from the current Run
// input without inheriting conversation history or copying binary payloads.
func inheritableTaskInputParts(input []Message) []contextpkg.ContentPart {
	var result []contextpkg.ContentPart
	for _, message := range input {
		if message.Role != "user" {
			continue
		}
		for _, part := range message.Parts {
			if part.Kind != "image_ref" || strings.TrimSpace(part.ArtifactRef) == "" {
				continue
			}
			result = append(result, part)
		}
	}
	return result
}

func subAgentDepth(metadata map[string]string) int {
	if metadata == nil {
		return 0
	}
	value, err := strconv.Atoi(metadata["agent_gateway.depth"])
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func (r *EinoRuntime) pipe(ctx context.Context, stop context.CancelFunc, req RunRequest, checkpointID string, iterator EinoEventIterator, bridge *runtimeEventBridge, out chan<- observability.AgentEvent) {
	defer close(out)
	defer stop()
	defer r.clearActive(req.RunID)
	defer bridge.Close()
	if !sendEinoEvent(ctx, out, observability.AgentEvent{EventType: observability.EventAgentStarted, Visibility: observability.VisibilityDebug}) {
		return
	}
	userOutput := newEinoUserOutputProjection(req.Definition.AgentID)
	nativeEvents := make(chan *adk.AgentEvent)
	go func() {
		defer close(nativeEvents)
		for {
			event, ok := iterator.Next()
			if !ok {
				return
			}
			select {
			case <-ctx.Done():
				return
			case nativeEvents <- event:
			}
		}
	}()
	for {
		select {
		case event := <-bridge.events:
			if !sendEinoBridgeEvent(ctx, out, event, userOutput) {
				return
			}
			continue
		default:
		}
		select {
		case <-ctx.Done():
			return
		case event := <-bridge.events:
			if !sendEinoBridgeEvent(ctx, out, event, userOutput) {
				return
			}
		case event, ok := <-nativeEvents:
			if !ok {
				drainRuntimeBridge(ctx, bridge, out, userOutput)
				if !userOutput.EmitFinal(ctx, out) {
					return
				}
				if !sendEinoEvent(ctx, out, observability.AgentEvent{EventType: observability.EventAgentCompleted, Visibility: observability.VisibilityDebug}) {
					return
				}
				_ = sendEinoEvent(ctx, out, observability.AgentEvent{EventType: observability.EventRunCompleted, Visibility: observability.VisibilityDebug})
				return
			}
			// Gateway emit completes before the native engine can publish the
			// corresponding tool/sub-agent result. Drain those causally prior
			// events even when both channels became ready in the same select.
			drainRuntimeBridge(ctx, bridge, out, userOutput)
			if ctx.Err() != nil {
				return
			}
			if r.handleEinoEvent(ctx, req, checkpointID, event, bridge, out, userOutput) {
				return
			}
		}
	}
}

func (r *EinoRuntime) handleEinoEvent(ctx context.Context, req RunRequest, checkpointID string, event *adk.AgentEvent, bridge *runtimeEventBridge, out chan<- observability.AgentEvent, outputProjection ...*einoUserOutputProjection) bool {
	if event == nil {
		return false
	}
	if event.Err != nil {
		runtimeErr := DefaultRuntimeErrorClassifier{}.Classify(event.Err, ErrorStageRuntimeAdapter)
		observability.LoggerFrom(ctx, observability.NoopLogger{}).Error(ctx, "eino runtime event failed", event.Err,
			observability.String("runtime_error_code", runtimeErr.Code),
			observability.String("runtime_error_type", string(runtimeErr.Type)),
		)
		var cancelErr *adk.CancelError
		if runtimeErr.Type == ErrorCancelled || errors.Is(event.Err, context.Canceled) || errors.Is(event.Err, adk.ErrStreamCanceled) || errors.As(event.Err, &cancelErr) {
			_ = sendEinoEvent(ctx, out, observability.AgentEvent{EventType: observability.EventRunCancelled, Visibility: observability.VisibilityDebug})
			return true
		}
		_ = sendEinoEvent(ctx, out, observability.AgentEvent{EventType: observability.EventRunFailed, Visibility: observability.VisibilityDebug, Error: runtimeErr.EventError()})
		return true
	}
	if event.Action != nil && event.Action.Interrupted != nil {
		control, err := r.Controls.Create(ctx, EinoControlRequestFactoryRequest{Run: req, CheckpointID: checkpointID, Interrupt: event.Action.Interrupted})
		if err != nil {
			runtimeErr := NewRuntimeError(ErrorCheckpoint, "EINO_CONTROL_REQUEST_FAILED", "failed to create control request").WithCause(err)
			_ = sendEinoEvent(ctx, out, observability.AgentEvent{EventType: observability.EventRunFailed, Visibility: observability.VisibilityDebug, Error: runtimeErr.EventError()})
			return true
		}
		control.Binding.SessionID = req.SessionID
		control.Binding.RunID = req.RunID
		control.Binding.CheckpointID = checkpointID
		if err := validateWaitingControl(control.Binding); err != nil {
			runtimeErr := NewRuntimeError(ErrorCheckpoint, "EINO_CONTROL_REQUEST_INVALID", "control request binding is invalid").WithCause(err)
			_ = sendEinoEvent(ctx, out, observability.AgentEvent{EventType: observability.EventRunFailed, Visibility: observability.VisibilityDebug, Error: runtimeErr.EventError()})
			return true
		}
		payload, err := canonicalEinoControlPayload(control, checkpointID, event.Action.Interrupted)
		if err != nil {
			runtimeErr := NewRuntimeError(ErrorSchemaValidation, "EINO_CONTROL_PAYLOAD_INVALID", "control request payload is invalid").WithCause(err)
			_ = sendEinoEvent(ctx, out, observability.AgentEvent{EventType: observability.EventRunFailed, Visibility: observability.VisibilityDebug, Error: runtimeErr.EventError()})
			return true
		}
		r.interruptMu.Lock()
		r.interrupts[interruptBindingKey(req.RunID, control.Binding.ControlRequestID)] = control.Binding
		r.interruptMu.Unlock()
		if !sendEinoEvent(ctx, out, observability.AgentEvent{
			EventType:  observability.EventCheckpointCreated,
			Visibility: observability.VisibilityInternal,
			Payload:    JSONPayload(map[string]string{"checkpoint_id": checkpointID}),
		}) {
			return true
		}
		_ = sendEinoEvent(ctx, out, observability.AgentEvent{EventType: observability.EventControlRequestCreated, Visibility: observability.VisibilityUserVisible, Payload: payload})
		return true
	}
	if event.Output != nil && event.Output.MessageOutput != nil {
		return !r.emitMessage(ctx, out, event.Output.MessageOutput, event.AgentName, bridge, firstEinoOutputProjection(outputProjection))
	}
	return false
}

func (r *EinoRuntime) emitMessage(ctx context.Context, out chan<- observability.AgentEvent, variant *adk.MessageVariant, agentName string, bridge *runtimeEventBridge, outputProjection ...*einoUserOutputProjection) bool {
	projection := firstEinoOutputProjection(outputProjection)
	if variant.IsStreaming {
		if variant.MessageStream == nil {
			runtimeErr := NewRuntimeError(ErrorSchemaValidation, "EINO_MESSAGE_STREAM_INVALID", "eino returned a nil message stream")
			_ = sendEinoEvent(ctx, out, observability.AgentEvent{EventType: observability.EventRunFailed, Visibility: observability.VisibilityDebug, Error: runtimeErr.EventError()})
			return false
		}
		defer variant.MessageStream.Close()
		if bridge != nil {
			bridgeDone := make(chan struct{})
			bridgeDrained := make(chan struct{})
			go func() {
				defer close(bridgeDrained)
				drainRuntimeBridgeUntil(ctx, bridge, out, bridgeDone, projection)
			}()
			defer func() {
				close(bridgeDone)
				<-bridgeDrained
			}()
		}
		reasoning := newEinoPublicReasoning()
		observation := newEinoMessageObservation()
		for {
			message, err := variant.MessageStream.Recv()
			if errors.Is(err, io.EOF) {
				if projection != nil {
					projection.Observe(observation, variant.Role, agentName)
				}
				return emitEinoReasoningSummary(ctx, out, reasoning, agentName)
			}
			if err != nil {
				// Surface the underlying cause (e.g. the model gateway's
				// budget_exceeded / provider error) in the message; the opaque
				// "failed to read eino message stream" alone hid the real reason.
				runtimeErr := NewRuntimeError(ErrorRuntime, "EINO_MESSAGE_STREAM_FAILED",
					fmt.Sprintf("failed to read eino message stream: %v", err)).WithCause(err)
				_ = sendEinoEvent(ctx, out, observability.AgentEvent{EventType: observability.EventRunFailed, Visibility: observability.VisibilityDebug, Error: runtimeErr.EventError()})
				return false
			}
			if isEinoAssistantMessage(message, variant.Role) {
				reasoning.Observe(message)
				observation.Observe(message)
			}
			if !emitEinoVisibleMessage(ctx, out, message, variant.Role, agentName) {
				return false
			}
		}
	}
	if !emitEinoMessage(ctx, out, variant.Message, variant.Role, agentName) {
		return false
	}
	if projection == nil {
		return true
	}
	observation := newEinoMessageObservation()
	if isEinoAssistantMessage(variant.Message, variant.Role) {
		observation.Observe(variant.Message)
	}
	projection.Observe(observation, variant.Role, agentName)
	return true
}

func emitEinoMessage(ctx context.Context, out chan<- observability.AgentEvent, message *schema.Message, role schema.RoleType, agentName string) bool {
	if !isEinoAssistantMessage(message, role) {
		return true
	}
	reasoning := newEinoPublicReasoning()
	reasoning.Observe(message)
	if !emitEinoVisibleMessage(ctx, out, message, role, agentName) {
		return false
	}
	return emitEinoReasoningSummary(ctx, out, reasoning, agentName)
}

func emitEinoVisibleMessage(ctx context.Context, out chan<- observability.AgentEvent, message *schema.Message, role schema.RoleType, agentName string) bool {
	if !isEinoAssistantMessage(message, role) {
		return true
	}
	if message.Content != "" {
		payload := einoMessagePayload(message.Content, agentName)
		if !sendEinoEvent(ctx, out, observability.AgentEvent{
			EventType: observability.EventAgentTextDelta, Visibility: observability.VisibilityUserVisible,
			Payload: payload, PayloadPreview: append(json.RawMessage(nil), payload...),
		}) {
			return false
		}
	}
	for _, part := range message.AssistantGenMultiContent {
		if part.Text != "" {
			payload := einoMessagePayload(part.Text, agentName)
			if !sendEinoEvent(ctx, out, observability.AgentEvent{
				EventType: observability.EventAgentTextDelta, Visibility: observability.VisibilityUserVisible,
				Payload: payload, PayloadPreview: append(json.RawMessage(nil), payload...),
			}) {
				return false
			}
		}
	}
	return true
}

func isEinoAssistantMessage(message *schema.Message, role schema.RoleType) bool {
	if message == nil {
		return false
	}
	if role == "" {
		role = message.Role
	}
	return role == schema.Assistant
}

const (
	maxTaskAcknowledgementRunes = 160
	defaultTaskAcknowledgement  = "已收到你的需求，正在分析并准备执行。"
)

// einoMessageObservation collects only the public assistant text and whether
// the completed model turn requested tools. It deliberately excludes reasoning
// content and tool arguments: those remain observability facts, not chat prose.
type einoMessageObservation struct {
	text        strings.Builder
	hasToolCall bool
	role        schema.RoleType
}

func newEinoMessageObservation() *einoMessageObservation {
	return &einoMessageObservation{}
}

func (o *einoMessageObservation) Observe(message *schema.Message) {
	if o == nil || message == nil {
		return
	}
	if len(message.ToolCalls) > 0 {
		o.hasToolCall = true
	}
	if o.role == "" {
		o.role = message.Role
	}
	if message.Content != "" {
		o.text.WriteString(message.Content)
	}
	for _, part := range message.AssistantGenMultiContent {
		if part.Text != "" {
			o.text.WriteString(part.Text)
		}
	}
}

func firstEinoOutputProjection(values []*einoUserOutputProjection) *einoUserOutputProjection {
	if len(values) == 0 {
		return nil
	}
	return values[0]
}

// einoUserOutputProjection adds semantics at the runtime boundary without
// changing Eino's execution loop. Raw deltas continue to flow for existing
// Harness consumers; Agent Chat can instead consume one acknowledgement and
// the explicit final response emitted by this projection.
type einoUserOutputProjection struct {
	mu                       sync.Mutex
	rootAgentName            string
	acknowledgementEmitted   bool
	acknowledgementAttached  bool
	acknowledgement          string
	acknowledgementTruncated bool
	finalText                string
}

func newEinoUserOutputProjection(rootAgentName string) *einoUserOutputProjection {
	return &einoUserOutputProjection{rootAgentName: strings.TrimSpace(rootAgentName)}
}

func (p *einoUserOutputProjection) Observe(observation *einoMessageObservation, role schema.RoleType, agentName string) {
	if observation != nil && role == "" {
		role = observation.role
	}
	if p == nil || observation == nil || role != schema.Assistant || !p.isRootAgent(agentName) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	text := strings.TrimSpace(observation.text.String())
	if observation.hasToolCall {
		// A later tool round invalidates any earlier candidate; only the last
		// root, text-only turn may become the final response.
		p.finalText = ""
		if p.acknowledgementEmitted {
			return
		}
		p.acknowledgement, p.acknowledgementTruncated = normalizeEinoTaskAcknowledgement(text)
		p.acknowledgementEmitted = true
		return
	}
	if text != "" {
		p.finalText = text
	}
}

func (p *einoUserOutputProjection) EmitFinal(ctx context.Context, out chan<- observability.AgentEvent) bool {
	if p == nil {
		return true
	}
	p.mu.Lock()
	finalText := p.finalText
	p.mu.Unlock()
	if strings.TrimSpace(finalText) == "" {
		return true
	}
	payload := map[string]any{"content": finalText}
	if p.rootAgentName != "" {
		payload["runtime_agent_name"] = p.rootAgentName
	}
	return sendEinoEvent(ctx, out, observability.AgentEvent{
		EventType: observability.EventFinalResponse, Visibility: observability.VisibilityUserVisible,
		Payload: JSONPayload(payload),
	})
}

func (p *einoUserOutputProjection) Decorate(event observability.AgentEvent) observability.AgentEvent {
	if p == nil || event.EventType != observability.EventToolCallStarted {
		return event
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.acknowledgementEmitted || p.acknowledgementAttached || p.acknowledgement == "" {
		return event
	}
	var preview map[string]any
	_ = json.Unmarshal(event.PayloadPreview, &preview)
	if preview == nil {
		preview = make(map[string]any)
	}
	preview["task_acknowledgement"] = p.acknowledgement
	if p.acknowledgementTruncated {
		preview["task_acknowledgement_truncated"] = true
	}
	event.PayloadPreview = JSONPayload(preview)
	p.acknowledgementAttached = true
	return event
}

func (p *einoUserOutputProjection) isRootAgent(agentName string) bool {
	name := strings.TrimSpace(agentName)
	return p.rootAgentName == "" || name == "" || name == p.rootAgentName
}

func normalizeEinoTaskAcknowledgement(value string) (string, bool) {
	value = strings.Join(strings.Fields(value), " ")
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultTaskAcknowledgement, false
	}
	runes := []rune(value)
	if len(runes) <= maxTaskAcknowledgementRunes {
		return value, false
	}
	return strings.TrimSpace(string(runes[:maxTaskAcknowledgementRunes])), true
}

const (
	maxPublicReasoningRunes     = 1000
	reasoningScopeAnswerSummary = "answer_summary"
)

type einoPublicReasoning struct {
	runes       []rune
	truncated   bool
	hasToolCall bool
}

func newEinoPublicReasoning() *einoPublicReasoning {
	return &einoPublicReasoning{runes: make([]rune, 0, maxPublicReasoningRunes)}
}

func (r *einoPublicReasoning) Observe(message *schema.Message) {
	if r == nil || message == nil {
		return
	}
	if len(message.ToolCalls) > 0 {
		r.hasToolCall = true
		r.runes = nil
		return
	}
	if r.hasToolCall {
		return
	}
	foundPart := false
	for _, part := range message.AssistantGenMultiContent {
		if part.Reasoning == nil || part.Reasoning.Text == "" {
			continue
		}
		foundPart = true
		r.append(part.Reasoning.Text)
	}
	if !foundPart && message.ReasoningContent != "" {
		r.append(message.ReasoningContent)
	}
}

func (r *einoPublicReasoning) append(text string) {
	for _, item := range text {
		if len(r.runes) >= maxPublicReasoningRunes {
			r.truncated = true
			return
		}
		r.runes = append(r.runes, item)
	}
}

func emitEinoReasoningSummary(ctx context.Context, out chan<- observability.AgentEvent, reasoning *einoPublicReasoning, agentName string) bool {
	if reasoning == nil || reasoning.hasToolCall {
		return true
	}
	text := strings.TrimSpace(string(reasoning.runes))
	if text == "" {
		return true
	}
	payload := map[string]any{"text": text, "reasoning_scope": reasoningScopeAnswerSummary}
	if agentName != "" {
		payload["runtime_agent_name"] = agentName
	}
	if reasoning.truncated {
		payload["truncated"] = true
	}
	encoded := JSONPayload(payload)
	return sendEinoEvent(ctx, out, observability.AgentEvent{
		EventType: observability.EventReasoningSummary, Visibility: observability.VisibilityUserVisible,
		Payload: encoded, PayloadPreview: append(json.RawMessage(nil), encoded...),
	})
}

func einoMessagePayload(text, runtimeAgentName string) json.RawMessage {
	payload := map[string]string{"text": text}
	if runtimeAgentName != "" {
		payload["runtime_agent_name"] = runtimeAgentName
	}
	return JSONPayload(payload)
}

// systemPromptMessageID 是 Assembler prependSystemPrompt 写入 ConversationWindow
// 的受信任 system prompt 消息 ID（见 model_context.go）。
const systemPromptMessageID = "system_prompt"

// isFrozenSystemPromptMessage 报告一条 window 消息是否是 Registry 冻结解析的
// system prompt。
func isFrozenSystemPromptMessage(message ModelContextMessage) bool {
	return message.ID == systemPromptMessageID && schema.RoleType(message.Role) == schema.System
}

// einoSystemPrompt 提取 ConversationWindow 中 Registry 冻结解析的 system prompt
// 内容；未声明 PromptRef 时返回空串。
func einoSystemPrompt(pkg ModelContextPackage) string {
	for _, message := range pkg.Messages.ConversationWindow {
		if isFrozenSystemPromptMessage(message) {
			return message.Content
		}
	}
	return ""
}

// einoInputParts 把治理后的 ModelContentPart 投影为 eino 多模态输入 Part，
// 与 eino_chat_model_proxy.toModelContentParts 互为反向，保证循环内外
// 形状一致。未知类型降级为文本 Part，不丢事实。
func einoInputParts(parts []ModelContentPart) []schema.MessageInputPart {
	out := make([]schema.MessageInputPart, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "image_url":
			image := &schema.MessageInputImage{}
			image.MIMEType = part.MIMEType
			if part.URL != "" {
				url := part.URL
				image.URL = &url
			}
			if part.Base64Data != "" {
				data := part.Base64Data
				image.Base64Data = &data
			}
			out = append(out, schema.MessageInputPart{Type: schema.ChatMessagePartTypeImageURL, Image: image})
		case "file":
			// 非图片文件 part（G-C）保真投影为 eino file part，不降级为
			// 文本（内容字节在 Base64Data / artifact URL 中）。
			file := &schema.MessageInputFile{Name: part.Name}
			file.MIMEType = part.MIMEType
			if part.URL != "" {
				url := part.URL
				file.URL = &url
			}
			if part.Base64Data != "" {
				data := part.Base64Data
				file.Base64Data = &data
			}
			out = append(out, schema.MessageInputPart{Type: schema.ChatMessagePartTypeFileURL, File: file})
		default:
			out = append(out, schema.MessageInputPart{Type: schema.ChatMessagePartTypeText, Text: part.Text})
		}
	}
	return out
}

func einoMessages(pkg ModelContextPackage) []*schema.Message {
	messages := make([]*schema.Message, 0, len(pkg.Messages.ConversationWindow))
	for _, message := range pkg.Messages.ConversationWindow {
		if isFrozenSystemPromptMessage(message) {
			// Registry 冻结的 system prompt 已提升为 agent Instruction
			//（EinoManagedAgentBuildRequest.SystemPrompt），此处跳过以避免
			// 双 system 消息。
			continue
		}
		converted := &schema.Message{Role: schema.RoleType(message.Role), Content: message.Content}
		if schema.RoleType(message.Role) == schema.User && len(message.ContentParts) > 0 {
			// 多 Part 用户输入投影为 eino 多模态输入；此时 Content 留空，
			// 与 eino_chat_model_proxy 的反向投影（toModelContentParts）对齐。
			converted.Content = ""
			converted.UserInputMultiContent = einoInputParts(message.ContentParts)
		}
		for _, call := range message.ToolCalls {
			arguments, _ := json.Marshal(call.Arguments)
			converted.ToolCalls = append(converted.ToolCalls, schema.ToolCall{
				ID:   call.ID,
				Type: "function",
				Function: schema.FunctionCall{
					Name:      call.Name,
					Arguments: string(arguments),
				},
			})
		}
		if message.ToolResult != nil {
			converted.ToolCallID = message.ToolResult.CallID
			converted.ToolName = message.ToolResult.Name
			if converted.Content == "" {
				converted.Content = message.ToolResult.Content
			}
		}
		messages = append(messages, converted)
	}
	return messages
}

func (r *EinoRuntime) setActive(runID string, cancel adk.AgentCancelFunc) {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	r.active[runID] = cancel
}

func (r *EinoRuntime) clearActive(runID string) {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	delete(r.active, runID)
}

func sendEinoEvent(ctx context.Context, out chan<- observability.AgentEvent, event observability.AgentEvent) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- event:
		return true
	}
}

func sendEinoBridgeEvent(ctx context.Context, out chan<- observability.AgentEvent, event observability.AgentEvent, projection *einoUserOutputProjection) bool {
	if projection != nil {
		event = projection.Decorate(event)
	}
	return sendEinoEvent(ctx, out, event)
}

func drainRuntimeBridge(ctx context.Context, bridge *runtimeEventBridge, out chan<- observability.AgentEvent, outputProjection ...*einoUserOutputProjection) {
	if bridge == nil {
		return
	}
	projection := firstEinoOutputProjection(outputProjection)
	for {
		select {
		case event := <-bridge.events:
			if !sendEinoBridgeEvent(ctx, out, event, projection) {
				return
			}
		default:
			return
		}
	}
}

func drainRuntimeBridgeUntil(ctx context.Context, bridge *runtimeEventBridge, out chan<- observability.AgentEvent, done <-chan struct{}, outputProjection ...*einoUserOutputProjection) {
	if bridge == nil {
		return
	}
	projection := firstEinoOutputProjection(outputProjection)
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			drainRuntimeBridge(ctx, bridge, out, projection)
			return
		case event := <-bridge.events:
			if !sendEinoBridgeEvent(ctx, out, event, projection) {
				return
			}
		}
	}
}

func einoCheckpointID(runID string) string { return "eino_ckpt_" + runID }

func interruptBindingKey(runID, controlRequestID string) string {
	return runID + "\x00" + controlRequestID
}

func controlRequestID(payload json.RawMessage) string {
	var value struct {
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal(payload, &value)
	return value.RequestID
}

func validateWaitingControl(binding WaitingControlRequest) error {
	if binding.SessionID == "" {
		return ErrSessionMissing
	}
	if binding.RunID == "" {
		return ErrRunIDMissing
	}
	if binding.CheckpointID == "" {
		return ErrCheckpointIDMissing
	}
	if binding.ControlRequestID == "" {
		return ErrControlRequestIDMissing
	}
	if binding.ResumeToken == "" {
		return ErrResumeTokenMissing
	}
	return nil
}

func canonicalEinoControlPayload(control EinoControlRequest, checkpointID string, interrupt *adk.InterruptInfo) (json.RawMessage, error) {
	payload := make(map[string]any)
	if len(control.Payload) > 0 {
		if err := json.Unmarshal(control.Payload, &payload); err != nil {
			return nil, err
		}
	}
	// 子 Agent ask_user 上交：把 GatewayProxyAgent 中断根因里的 proposal
	// 业务字段（prompt/kind/options/input 等）提升到 payload 顶层，供
	// ExtractInteractionProposal 与前端直接消费。
	mergeGatewayProxyProposal(payload, interrupt)
	controlType := control.Type
	if controlType == "" {
		controlType = "ask_user"
	}
	payload["request_id"] = control.Binding.ControlRequestID
	payload["type"] = controlType
	payload["checkpoint_id"] = checkpointID
	payload["interrupt_contexts"] = einoInterruptContexts(interrupt)
	if _, ok := payload["required"]; !ok {
		payload["required"] = true
	}
	delete(payload, "resume_token")
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// gatewayProxyProposalMarker 标记 GatewayProxyAgent 上交的 ask_user proposal。
const gatewayProxyProposalMarker = "harness_child_proposal"

// mergeGatewayProxyProposal 在中断链根因携带子 Agent proposal 时，把业务
// 字段合并进 control payload 顶层（保留 canonical 字段的最终写入权）。
func mergeGatewayProxyProposal(payload map[string]any, interrupt *adk.InterruptInfo) {
	if interrupt == nil {
		return
	}
	for _, item := range interrupt.InterruptContexts {
		if item == nil || !item.IsRootCause {
			continue
		}
		info, ok := jsonSafeValue(item.Info).(map[string]any)
		if !ok {
			continue
		}
		if marked, _ := info[gatewayProxyProposalMarker].(bool); !marked {
			continue
		}
		for key, value := range info {
			if key == gatewayProxyProposalMarker {
				continue
			}
			payload[key] = value
		}
		return
	}
}

func einoInterruptContexts(interrupt *adk.InterruptInfo) []map[string]any {
	contexts := make([]map[string]any, 0)
	if interrupt == nil {
		return contexts
	}
	for _, item := range interrupt.InterruptContexts {
		if item == nil {
			continue
		}
		contexts = append(contexts, map[string]any{
			"id":            item.ID,
			"is_root_cause": item.IsRootCause,
			"info":          jsonSafeValue(item.Info),
		})
	}
	return contexts
}

type DefaultEinoResumeMapper struct{}

func (DefaultEinoResumeMapper) Map(_ context.Context, req ResumeRequest) (*adk.ResumeParams, error) {
	if len(req.ControlPayload) == 0 {
		return nil, nil
	}
	var payload struct {
		Targets map[string]any `json:"targets"`
	}
	if err := json.Unmarshal(req.ControlPayload, &payload); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEinoResumePayloadInvalid, err)
	}
	if len(payload.Targets) == 0 {
		return nil, nil
	}
	return &adk.ResumeParams{Targets: payload.Targets}, nil
}

type DefaultEinoControlRequestFactory struct {
	IDs observability.IDGenerator
}

func (f DefaultEinoControlRequestFactory) Create(_ context.Context, req EinoControlRequestFactoryRequest) (EinoControlRequest, error) {
	ids := f.IDs
	if ids == nil {
		ids = observability.NewULIDGenerator("eino")
	}
	requestID := ids.NewRequestID()
	token, err := newEinoResumeToken()
	if err != nil {
		return EinoControlRequest{}, err
	}
	payload := JSONPayload(map[string]any{
		"request_id":         requestID,
		"type":               "ask_user",
		"checkpoint_id":      req.CheckpointID,
		"title":              "Agent requires user input",
		"input_type":         "structured",
		"required":           true,
		"interrupt_contexts": einoInterruptContexts(req.Interrupt),
	})
	return EinoControlRequest{
		Binding: WaitingControlRequest{
			SessionID:        req.Run.SessionID,
			RunID:            req.Run.RunID,
			CheckpointID:     req.CheckpointID,
			ControlRequestID: requestID,
			ResumeToken:      token,
		},
		Type:    "ask_user",
		Payload: payload,
	}, nil
}

func newEinoResumeToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func jsonSafeValue(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	var safe any
	if err := json.Unmarshal(data, &safe); err != nil {
		return fmt.Sprint(value)
	}
	return safe
}
