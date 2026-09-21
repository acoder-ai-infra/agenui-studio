package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/prebuilt/deep"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
)

const (
	defaultEinoDeepAgentMaxIterations  = 20
	maxEinoDeepAgentMaxIterations      = 1000
	defaultHarnessDeepAgentInstruction = "You are a managed DeepAgent running inside Harness. Use the provided tools for actions, use write_todos for agenuinely multi-step work, delegate isolated complex work through task when an authorized subagent is available, and return a concise synthesized result."
)

var (
	ErrEinoManagedChatModelMissing       = errors.New("eino managed chat model missing")
	ErrEinoManagedModeUnsupported        = errors.New("eino managed mode unsupported")
	ErrEinoManagedConfigInvalid          = errors.New("eino managed agent config invalid")
	ErrEinoInternalAgentDecoratorMissing = errors.New("eino internal agent decorator missing")
)

type einoDeepAgentBuildFunc func(context.Context, *deep.Config) (adk.ResumableAgent, error)

// EinoDeepAgentFactory is the restricted production assembly path for Eino
// DeepAgent. It only consumes Harness-created proxies from the build request.
type EinoDeepAgentFactory struct {
	build einoDeepAgentBuildFunc
}

var _ EinoManagedAgentFactory = (*EinoDeepAgentFactory)(nil)

func NewEinoDeepAgentFactory() *EinoDeepAgentFactory {
	return newEinoDeepAgentFactory(deep.New)
}

func newEinoDeepAgentFactory(build einoDeepAgentBuildFunc) *EinoDeepAgentFactory {
	return &EinoDeepAgentFactory{build: build}
}

func (f *EinoDeepAgentFactory) Build(ctx context.Context, req EinoManagedAgentBuildRequest) (adk.ResumableAgent, error) {
	if req.Definition.Runtime.Mode != RuntimeModeDeepAgent {
		return nil, fmt.Errorf("%w: %s", ErrEinoManagedModeUnsupported, req.Definition.Runtime.Mode)
	}
	if req.ChatModel == nil {
		return nil, ErrEinoManagedChatModelMissing
	}
	if req.InternalAgents == nil {
		return nil, ErrEinoInternalAgentDecoratorMissing
	}
	if f == nil || f.build == nil {
		return nil, errors.New("eino deep agent builder missing")
	}
	maxIterations, err := einoDeepAgentMaxIterations(req.Definition)
	if err != nil {
		return nil, err
	}
	description := req.Definition.Metadata["description"]
	if description == "" {
		description = req.Definition.AgentType
	}
	// Instruction 优先使用 Registry 冻结解析的 system prompt（经 Assembler
	// hash 校验后由 EinoRuntime 提取）；Agent 未声明 PromptRef 时回落默认
	// 托管指令，行为兼容。
	instruction := req.SystemPrompt
	if instruction == "" {
		instruction = defaultHarnessDeepAgentInstruction
	}
	return f.build(ctx, &deep.Config{
		Name:        req.Definition.AgentID,
		Description: description,
		Instruction: instruction,
		ChatModel:   req.ChatModel,
		SubAgents:   append([]adk.Agent(nil), req.PlatformSubAgents...),
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: append([]tool.BaseTool(nil), req.PlatformTools...),
				// tool_policy.execution=parallel 时并发执行本轮 N 个工具调用；
				// 缺省（sequential）按模型返回顺序依次执行，避免未显式配置的
				// Agent 出现新的并发副作用（ADR-014）。缺省归一在
				// agentregistry.ToAgentDefinition 完成，此处反向判定作为双保险。
				ExecuteSequentially: req.Definition.ToolExecution != ToolExecutionParallel,
			},
			EmitInternalEvents: true,
		},
		Handlers:               append([]adk.ChatModelAgentMiddleware(nil), req.PreModelHandlers...),
		MaxIteration:           maxIterations,
		WithoutGeneralSubAgent: true,
	})
}

func einoDeepAgentMaxIterations(def AgentDefinition) (int, error) {
	raw := def.Metadata["max_iterations"]
	if raw == "" {
		return defaultEinoDeepAgentMaxIterations, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 || value > maxEinoDeepAgentMaxIterations {
		return 0, fmt.Errorf("%w: max_iterations=%q", ErrEinoManagedConfigInvalid, raw)
	}
	return value, nil
}
