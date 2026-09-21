package agentruntime

import (
	"context"
	"encoding/json"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// EinoManagedAgentFactory materializes an Eino agent from a runtime-neutral
// definition. The factory must bind model, tool and sub-agent implementations
// from Environment instead of constructing unmanaged clients.
type EinoManagedAgentFactory interface {
	Build(ctx context.Context, req EinoManagedAgentBuildRequest) (adk.ResumableAgent, error)
}

type EinoManagedAgentFactoryFunc func(context.Context, EinoManagedAgentBuildRequest) (adk.ResumableAgent, error)

func (f EinoManagedAgentFactoryFunc) Build(ctx context.Context, req EinoManagedAgentBuildRequest) (adk.ResumableAgent, error) {
	return f(ctx, req)
}

type EinoManagedAgentBuildRequest struct {
	Definition  AgentDefinition
	Package     ModelContextPackage
	Environment RuntimeEnvironment
	// ChatModel is created by EinoRuntime and always delegates to
	// Environment.Models. Factories must use it for model-backed modes.
	ChatModel model.ToolCallingChatModel
	// PlatformSubAgents contains only authorized Harness Agent proxies. Runtime
	// internal planner/replanner agents are not included in this collection.
	PlatformSubAgents []adk.Agent
	// PlatformTools contains only authorized Harness Tool Gateway proxies.
	// Factories must not add native tools outside this collection.
	PlatformTools []tool.BaseTool
	// PreModelHandlers are adapter-native handlers resolved from Harness context
	// policy. They run before every Eino model round and are the correct place
	// for stateful summarization/reduction. Final provider safety remains owned
	// by GovernedModelInvoker.
	PreModelHandlers []adk.ChatModelAgentMiddleware
	// SystemPrompt 是 Assembler 经 SystemPromptResolver 冻结解析的受信任
	// system prompt（来自 ConversationWindow 头部的 system_prompt 消息）。
	// 工厂应把它作为 agent Instruction；为空表示 Agent 未声明 PromptRef，
	// 工厂回落自身默认指令。版本冻结与 hash 校验由 Assembler 链路承担。
	SystemPrompt string
	// InternalAgents decorates planner/replanner/critic/general helper agents as
	// nested Runtime Steps. A managed factory must use it for every internal
	// agent it assembles; platform sub-agents use PlatformSubAgents instead.
	InternalAgents EinoInternalAgentDecoratorPort
}

type EinoPreModelHandlerResolver interface {
	Resolve(ctx context.Context, req EinoPreModelHandlerRequest) ([]adk.ChatModelAgentMiddleware, error)
}

type EinoPreModelHandlerResolverFunc func(context.Context, EinoPreModelHandlerRequest) ([]adk.ChatModelAgentMiddleware, error)

func (f EinoPreModelHandlerResolverFunc) Resolve(ctx context.Context, req EinoPreModelHandlerRequest) ([]adk.ChatModelAgentMiddleware, error) {
	return f(ctx, req)
}

type EinoPreModelHandlerRequest struct {
	Definition  AgentDefinition
	Package     ModelContextPackage
	Environment RuntimeEnvironment
	// Policy is already resolved and frozen by Harness. Resolver implementations
	// only compile it into Eino-native handlers; they must not select another
	// policy or read dynamic configuration.
	Policy           ContextCompactionPolicy
	PreserveManifest PreserveManifest
	// ScopedData 是本 Run 冻结的 scoped-data 快照，resolver 只读使用（投影
	// 为 BeforeModelHook 的只读视图）；不得在 handler 内回写。
	ScopedData ScopedData
}

// EinoToolInfoResolver is the Foundation-compatible local fallback. Production
// packages with ToolSchemaSnapshot never call it and consume frozen definitions.
// Deprecated: assemble ToolSchemaSnapshot and ToolDefinitions before Runtime.Run.
type EinoToolInfoResolver interface {
	Resolve(ctx context.Context, refs []string) ([]EinoResolvedTool, error)
}

type EinoResolvedTool struct {
	Ref  string
	Info *schema.ToolInfo
}

type EinoToolInfoResolverFunc func(context.Context, []string) ([]EinoResolvedTool, error)

func (f EinoToolInfoResolverFunc) Resolve(ctx context.Context, refs []string) ([]EinoResolvedTool, error) {
	return f(ctx, refs)
}

type EinoControlRequestFactory interface {
	Create(ctx context.Context, req EinoControlRequestFactoryRequest) (EinoControlRequest, error)
}

type EinoControlRequestFactoryFunc func(context.Context, EinoControlRequestFactoryRequest) (EinoControlRequest, error)

func (f EinoControlRequestFactoryFunc) Create(ctx context.Context, req EinoControlRequestFactoryRequest) (EinoControlRequest, error) {
	return f(ctx, req)
}

type EinoControlRequestFactoryRequest struct {
	Run          RunRequest
	CheckpointID string
	Interrupt    *adk.InterruptInfo
}

type EinoControlRequest struct {
	Binding WaitingControlRequest
	Type    string
	Payload json.RawMessage
}

// EinoResumeMapper converts the platform ControlResponse payload into Eino's
// targeted resume parameters. Returning nil selects Eino's implicit-resume mode.
type EinoResumeMapper interface {
	Map(ctx context.Context, req ResumeRequest) (*adk.ResumeParams, error)
}

type EinoResumeMapperFunc func(context.Context, ResumeRequest) (*adk.ResumeParams, error)

func (f EinoResumeMapperFunc) Map(ctx context.Context, req ResumeRequest) (*adk.ResumeParams, error) {
	return f(ctx, req)
}

type EinoEventIterator interface {
	Next() (*adk.AgentEvent, bool)
}

type EinoExecution interface {
	Run(ctx context.Context, messages []*schema.Message, checkpointID string) (EinoEventIterator, adk.AgentCancelFunc, error)
	Resume(ctx context.Context, checkpointID string, params *adk.ResumeParams) (EinoEventIterator, adk.AgentCancelFunc, error)
}

type EinoExecutionFactory interface {
	New(ctx context.Context, agent adk.ResumableAgent, checkpoints adk.CheckPointStore) (EinoExecution, error)
}
