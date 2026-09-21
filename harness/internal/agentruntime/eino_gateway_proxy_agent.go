package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

var (
	ErrGatewayProxyAgentNameMissing    = errors.New("gateway proxy agent name missing")
	ErrGatewayProxyAgentInvokerMissing = errors.New("gateway proxy agent invoker missing")
	ErrGatewayProxyAgentInputInvalid   = errors.New("gateway proxy agent input invalid")
	ErrGatewayProxyAgentInvokeFailed   = errors.New("gateway proxy agent invocation failed")
	ErrGatewayProxyAgentResultFailed   = errors.New("gateway proxy agent result failed")
	ErrGatewayProxyAgentEventInvalid   = errors.New("gateway proxy agent event lifecycle invalid")
)

type GatewayProxyAgentConfig struct {
	Name        string
	Description string
	Invoker     SubAgentInvoker
	Request     SubAgentInvocationRequest
	IDs         observability.IDGenerator
}

// GatewayProxyAgent exposes a registered platform Agent as an Eino sub-agent.
// It never executes that Agent locally; every invocation crosses SubAgentInvoker.
type GatewayProxyAgent struct {
	name        string
	description string
	invoker     SubAgentInvoker
	request     SubAgentInvocationRequest
	ids         observability.IDGenerator
}

var _ adk.Agent = (*GatewayProxyAgent)(nil)
var _ adk.ResumableAgent = (*GatewayProxyAgent)(nil)

// gatewayProxyResumeStateSchemaVersion 固定 resume state 的形状版本。
const gatewayProxyResumeStateSchemaVersion = "harness.gateway_proxy.resume_state.v1"

// gatewayProxyResumeState 是 GatewayProxyAgent 中断时随 checkpoint 保存的
// 恢复绑定；以 JSON 字符串形式进入 eino interrupt state（string 在任何
// checkpoint 序列化协议下均稳定）。
type gatewayProxyResumeState struct {
	SchemaVersion string               `json:"schema_version"`
	SubAgentRef   string               `json:"sub_agent_ref"`
	TaskID        string               `json:"task_id"`
	Control       SubAgentChildControl `json:"control"`
}

func NewGatewayProxyAgent(config GatewayProxyAgentConfig) (*GatewayProxyAgent, error) {
	if strings.TrimSpace(config.Name) == "" {
		return nil, ErrGatewayProxyAgentNameMissing
	}
	if config.Invoker == nil {
		return nil, ErrGatewayProxyAgentInvokerMissing
	}
	ids := config.IDs
	if ids == nil {
		ids = observability.NewULIDGenerator("subagent_task")
	}
	request := config.Request
	request.Scope = SubAgentScopePlatformChildRun
	request.SubAgentRef = config.Name
	return &GatewayProxyAgent{name: config.Name, description: config.Description, invoker: config.Invoker, request: request, ids: ids}, nil
}

func (a *GatewayProxyAgent) Name(context.Context) string { return a.name }

func (a *GatewayProxyAgent) Description(context.Context) string { return a.description }

func (a *GatewayProxyAgent) Run(ctx context.Context, input *adk.AgentInput, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iterator, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		defer generator.Close()
		result, err := a.invoke(ctx, input)
		if err != nil {
			generator.Send(&adk.AgentEvent{AgentName: a.name, Err: err})
			return
		}
		a.finish(ctx, generator, result)
	}()
	return iterator
}

// Resume 实现 adk.ResumableAgent：父 Run 恢复时框架把控制权路由回本代理，
// 代理凭 interrupt state 里的 child control 绑定经 Agent Gateway 恢复子 Run。
func (a *GatewayProxyAgent) Resume(ctx context.Context, info *adk.ResumeInfo, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iterator, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		defer generator.Close()
		result, err := a.resumeChild(ctx, info)
		if err != nil {
			generator.Send(&adk.AgentEvent{AgentName: a.name, Err: err})
			return
		}
		a.finish(ctx, generator, result)
	}()
	return iterator
}

// GatewayProxyInterruptInfo 让子 Agent proposal 以 checkpoint 安全的形状进入
// eino interrupt 链（动态 map 无法过 gob；RawMessage 是 []byte 安全的）。
// MarshalJSON 输出 payload 原文，供 canonical control payload 构造侧按 JSON
// roundtrip 读取业务字段。
type GatewayProxyInterruptInfo struct {
	Payload json.RawMessage
}

func (i GatewayProxyInterruptInfo) MarshalJSON() ([]byte, error) {
	if len(i.Payload) == 0 {
		return []byte("null"), nil
	}
	return i.Payload, nil
}

// finish 把子 Agent 结果转成 eino 事件：ask_user 上交转为 StatefulInterrupt
// （父 Run 由此创建用户可见的 ControlRequest），常规结果转为 assistant 消息。
func (a *GatewayProxyAgent) finish(ctx context.Context, generator *adk.AsyncGenerator[*adk.AgentEvent], result SubAgentInvocationResult) {
	if result.ResultType == SubAgentResultAskUserType && result.ChildControl != nil {
		info := map[string]any{
			gatewayProxyProposalMarker: true,
			"sub_agent_ref":            a.name,
			"task_id":                  result.ChildControl.ChildRunID,
		}
		var proposal map[string]any
		if len(result.Proposal) > 0 && json.Unmarshal(result.Proposal, &proposal) == nil {
			for key, value := range proposal {
				info[key] = value
			}
		}
		infoRaw, err := json.Marshal(info)
		if err != nil {
			generator.Send(&adk.AgentEvent{AgentName: a.name, Err: fmt.Errorf("marshal gateway proxy interrupt info: %w", err)})
			return
		}
		state, err := json.Marshal(gatewayProxyResumeState{
			SchemaVersion: gatewayProxyResumeStateSchemaVersion,
			SubAgentRef:   a.name,
			TaskID:        result.ChildControl.ChildRunID,
			Control:       *result.ChildControl,
		})
		if err != nil {
			generator.Send(&adk.AgentEvent{AgentName: a.name, Err: fmt.Errorf("marshal gateway proxy resume state: %w", err)})
			return
		}
		generator.Send(adk.StatefulInterrupt(ctx, GatewayProxyInterruptInfo{Payload: infoRaw}, string(state)))
		return
	}
	content := result.Content
	if content == "" && result.ContentRef != "" {
		payload, err := json.Marshal(struct {
			ContentRef string `json:"content_ref"`
			ChildRunID string `json:"child_run_id,omitempty"`
		}{ContentRef: result.ContentRef, ChildRunID: result.ChildRunID})
		if err != nil {
			generator.Send(&adk.AgentEvent{AgentName: a.name, Err: fmt.Errorf("marshal gateway proxy result: %w", err)})
			return
		}
		content = string(payload)
	}
	generator.Send(adk.EventFromMessage(schema.AssistantMessage(content, nil), nil, schema.Assistant, ""))
}

func (a *GatewayProxyAgent) resumeChild(ctx context.Context, info *adk.ResumeInfo) (SubAgentInvocationResult, error) {
	if a == nil || a.invoker == nil {
		return SubAgentInvocationResult{}, ErrGatewayProxyAgentInvokerMissing
	}
	resumer, ok := a.invoker.(SubAgentResumer)
	if !ok {
		return SubAgentInvocationResult{}, fmt.Errorf("%w: invoker does not support child resume", ErrGatewayProxyAgentInvokeFailed)
	}
	if info == nil || !info.WasInterrupted {
		return SubAgentInvocationResult{}, fmt.Errorf("%w: resume info missing interrupt state", ErrGatewayProxyAgentInvokeFailed)
	}
	stateRaw, _ := info.InterruptState.(string)
	var state gatewayProxyResumeState
	if stateRaw == "" || json.Unmarshal([]byte(stateRaw), &state) != nil || state.SchemaVersion != gatewayProxyResumeStateSchemaVersion || state.Control.ControlTicket == "" {
		return SubAgentInvocationResult{}, fmt.Errorf("%w: gateway proxy resume state invalid", ErrGatewayProxyAgentInvokeFailed)
	}
	payload := einoGlobalResumePayload(ctx)
	if info.ResumeData != nil {
		if data, err := json.Marshal(info.ResumeData); err == nil {
			payload = data
		}
	}
	emitter, ok := RuntimeEventEmitterFrom(ctx)
	if !ok {
		return SubAgentInvocationResult{}, ErrRuntimeEventEmitterMissing
	}
	sink := newGatewayProxyEventSink(ctx, emitter)
	result, err := resumer.ResumeChild(ctx, SubAgentResumeRequest{
		Trace:           observability.MustTraceContext(ctx),
		TenantID:        a.request.TenantID,
		SessionID:       a.request.SessionID,
		ParentRunID:     a.request.ParentRunID,
		SubAgentRef:     a.name,
		TaskID:          state.TaskID,
		Control:         state.Control,
		ResponsePayload: payload,
	}, sink)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return SubAgentInvocationResult{}, ctxErr
		}
		return SubAgentInvocationResult{}, fmt.Errorf("%w: %v", ErrGatewayProxyAgentInvokeFailed, err)
	}
	if result.ResultType == SubAgentResultAskUserType {
		if err := sink.ValidateAskUser(); err != nil {
			return SubAgentInvocationResult{}, err
		}
		return result, nil
	}
	if err := sink.ValidateComplete(result.IsError); err != nil {
		return SubAgentInvocationResult{}, err
	}
	if result.IsError {
		return SubAgentInvocationResult{}, ErrGatewayProxyAgentResultFailed
	}
	return result, nil
}

func (a *GatewayProxyAgent) invoke(ctx context.Context, input *adk.AgentInput) (SubAgentInvocationResult, error) {
	if a == nil || a.invoker == nil {
		return SubAgentInvocationResult{}, ErrGatewayProxyAgentInvokerMissing
	}
	emitter, ok := RuntimeEventEmitterFrom(ctx)
	if !ok {
		return SubAgentInvocationResult{}, ErrRuntimeEventEmitterMissing
	}
	description := gatewayProxyDescription(input)
	if description == "" {
		return SubAgentInvocationResult{}, ErrGatewayProxyAgentInputInvalid
	}
	req := a.request
	req.Trace = observability.MustTraceContext(ctx)
	req.TaskID = a.ids.NewRequestID()
	req.Description = description
	sink := newGatewayProxyEventSink(ctx, emitter)
	result, err := a.invoker.Invoke(ctx, req, sink)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return SubAgentInvocationResult{}, ctxErr
		}
		return SubAgentInvocationResult{}, fmt.Errorf("%w: %v", ErrGatewayProxyAgentInvokeFailed, err)
	}
	if result.ResultType == SubAgentResultAskUserType {
		if err := sink.ValidateAskUser(); err != nil {
			return SubAgentInvocationResult{}, err
		}
		return result, nil
	}
	if err := sink.ValidateComplete(result.IsError); err != nil {
		return SubAgentInvocationResult{}, err
	}
	if result.IsError {
		return SubAgentInvocationResult{}, ErrGatewayProxyAgentResultFailed
	}
	return result, nil
}

func gatewayProxyDescription(input *adk.AgentInput) string {
	if input == nil {
		return ""
	}
	for i := len(input.Messages) - 1; i >= 0; i-- {
		message := input.Messages[i]
		if message != nil && message.Role == schema.User && strings.TrimSpace(message.Content) != "" {
			return message.Content
		}
	}
	return ""
}

type gatewayProxyEventSink struct {
	ctx      context.Context
	emitter  RuntimeEventEmitter
	mu       sync.Mutex
	started  bool
	terminal observability.EventType
	err      error
}

func newGatewayProxyEventSink(ctx context.Context, emitter RuntimeEventEmitter) *gatewayProxyEventSink {
	return &gatewayProxyEventSink{ctx: ctx, emitter: emitter}
}

func (s *gatewayProxyEventSink) Emit(_ context.Context, event observability.AgentEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.terminal != "" {
		s.err = ErrGatewayProxyAgentEventInvalid
		return s.err
	}
	switch event.EventType {
	case observability.EventSubAgentStarted:
		if s.started {
			s.err = ErrGatewayProxyAgentEventInvalid
			return s.err
		}
		s.started = true
	case observability.EventSubAgentProgress:
		if !s.started {
			s.err = ErrGatewayProxyAgentEventInvalid
			return s.err
		}
	case observability.EventA2ATaskCreated, observability.EventA2ATaskProgress,
		observability.EventA2ATaskCompleted, observability.EventA2ATaskFailed,
		observability.EventA2ATaskCancelled:
		// Remote-provider facts are nested inside the one sub_agent wrapper.
		// They never replace the wrapper terminal and are invalid before start.
		if !s.started {
			s.err = ErrGatewayProxyAgentEventInvalid
			return s.err
		}
	case observability.EventSubAgentCompleted, observability.EventSubAgentFailed:
		if !s.started {
			s.err = ErrGatewayProxyAgentEventInvalid
			return s.err
		}
		s.terminal = event.EventType
	default:
		s.err = ErrGatewayProxyAgentEventInvalid
		return s.err
	}
	if err := s.emitter.Emit(s.ctx, event); err != nil {
		s.err = err
		return err
	}
	return nil
}

func (s *gatewayProxyEventSink) ValidateComplete(resultIsError bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if !s.started || s.terminal == "" {
		return ErrGatewayProxyAgentEventInvalid
	}
	if resultIsError && s.terminal != observability.EventSubAgentFailed {
		return ErrGatewayProxyAgentEventInvalid
	}
	if !resultIsError && s.terminal != observability.EventSubAgentCompleted {
		return ErrGatewayProxyAgentEventInvalid
	}
	return nil
}

// ValidateAskUser 校验 ask_user 上交时的事件形状：started 已发出且没有
// terminal（子 Agent 的本次 attempt 以 progress(proposal) 收尾，等待恢复）。
func (s *gatewayProxyEventSink) ValidateAskUser() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if !s.started || s.terminal != "" {
		return ErrGatewayProxyAgentEventInvalid
	}
	return nil
}
