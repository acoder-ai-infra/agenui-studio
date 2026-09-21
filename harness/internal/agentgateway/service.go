package agentgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/gatewaycontract"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

type ServiceConfig struct {
	Targets         TargetResolver
	Local           LocalProvider
	Remote          RemoteA2AProvider
	Tracer          observability.TraceProvider
	IDs             observability.IDGenerator
	Logger          observability.StructuredLogger
	TerminalTimeout time.Duration
}

type Service struct {
	targets         TargetResolver
	local           LocalProvider
	remote          RemoteA2AProvider
	plugins         map[string]GatewayPlugin
	ids             observability.IDGenerator
	terminalTimeout time.Duration
}

func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Targets == nil {
		return nil, newGatewayError(CodeProviderUnavailable, "target resolver is required", nil)
	}
	if cfg.IDs == nil {
		cfg.IDs = observability.NewULIDGenerator("agent_gateway")
	}
	if cfg.TerminalTimeout <= 0 {
		cfg.TerminalTimeout = 3 * time.Second
	}
	return &Service{
		targets: cfg.Targets, local: cfg.Local, remote: cfg.Remote,
		plugins: defaultPluginCatalog(cfg.Tracer), ids: cfg.IDs,
		terminalTimeout: cfg.TerminalTimeout,
	}, nil
}

var _ agentruntime.SubAgentInvoker = (*Service)(nil)

func (s *Service) Invoke(ctx context.Context, req agentruntime.SubAgentInvocationRequest, sink agentruntime.SubAgentEventSink) (agentruntime.SubAgentInvocationResult, error) {
	if err := validateInvocationRequest(req); err != nil {
		return agentruntime.SubAgentInvocationResult{}, err
	}
	if req.Depth >= 1 {
		return agentruntime.SubAgentInvocationResult{}, s.rejectNestedInvocation(ctx, sink, req,
			newGatewayError(CodeNestingLimit, "platform child Agents cannot delegate to another platform Agent", nil))
	}
	target, err := s.targets.Resolve(ctx, ResolveTargetRequest{
		TenantID: req.TenantID, SessionID: req.SessionID, ParentRunID: req.ParentRunID,
		ParentAgentID: req.ParentAgentID, ParentAgentVersion: req.ParentAgentVersion,
		ParentConfigSnapshotRef: req.ParentConfigSnapshotRef, ParentConfigHash: req.ParentConfigHash,
		SubAgentRef: req.SubAgentRef,
	})
	if err != nil {
		if errors.Is(err, ErrNestingLimit) {
			return agentruntime.SubAgentInvocationResult{}, s.rejectNestedInvocation(ctx, sink, req, err)
		}
		return agentruntime.SubAgentInvocationResult{}, newGatewayError(CodeTargetResolveFailed, "target resolution failed", err)
	}
	if err := validateResolvedTarget(req, target); err != nil {
		return agentruntime.SubAgentInvocationResult{}, err
	}
	chain, err := s.pluginChain(target.Plugins)
	if err != nil {
		return agentruntime.SubAgentInvocationResult{}, err
	}
	authorized := AuthorizedInvocation{
		TenantID: req.TenantID, SessionID: req.SessionID, ParentRunID: req.ParentRunID,
		ParentAgentID: req.ParentAgentID, SubAgentRef: req.SubAgentRef, ProviderKind: target.Provider,
		InvocationID: req.TaskID, AttemptID: s.ids.NewRequestID(), Description: req.Description,
		InputParts: append([]contextpkg.ContentPart(nil), req.InputParts...),
		Trace:      req.Trace, Binding: target.Binding,
	}
	if err = validateTaskInput(req.Description); err != nil {
		return agentruntime.SubAgentInvocationResult{}, s.emitStartedThenFailed(ctx, sink, authorized, err)
	}
	if err = s.emit(ctx, sink, authorized, observability.EventSubAgentStarted, nil, nil); err != nil {
		return agentruntime.SubAgentInvocationResult{}, err
	}
	facts := newInvocationFacts()
	projectedScopedData := projectScopedData(req.ScopedData, target.Definition)
	chain.provider = s.provider(sink, target, projectedScopedData, req.Depth, authorized, facts)
	result, err := chain.Invoke(ctx, authorized, facts)
	facts.seal()
	if err == nil && result.ResultType != agentruntime.SubAgentResultAskUserType {
		err = validateTaskOutput(result)
	}
	if err != nil {
		return agentruntime.SubAgentInvocationResult{}, s.fail(ctx, sink, authorized, facts, err)
	}
	if result.ResultType == agentruntime.SubAgentResultAskUserType {
		// 子 Agent 在等待用户输入：以 progress(proposal) 收尾本次 attempt，
		// 不发 terminal——ControlRequest 由父 Run 创建（方案 §6.5）。
		if err := s.emit(ctx, sink, authorized, observability.EventSubAgentProgress, askUserProposalPayload(result), nil); err != nil {
			return agentruntime.SubAgentInvocationResult{}, err
		}
		return result, nil
	}
	if err := s.emit(ctx, sink, authorized, observability.EventSubAgentCompleted, resultPayload(result, facts), nil); err != nil {
		return agentruntime.SubAgentInvocationResult{}, err
	}
	return result, nil
}

var _ agentruntime.SubAgentResumer = (*Service)(nil)

// ResumeChild 把父 Run 收到的用户答复路由回等待中的子 Run：凭 child control
// 绑定走 canonical Resume 正门，等待子 Run 终态并回传结果。
func (s *Service) ResumeChild(ctx context.Context, req agentruntime.SubAgentResumeRequest, sink agentruntime.SubAgentEventSink) (agentruntime.SubAgentInvocationResult, error) {
	if req.SessionID == "" || req.ParentRunID == "" || req.SubAgentRef == "" {
		return agentruntime.SubAgentInvocationResult{}, newGatewayError(CodeTargetResolveFailed, "resume request identity is incomplete", nil)
	}
	if req.Control.ChildRunID == "" || req.Control.RequestID == "" || req.Control.ControlTicket == "" {
		return agentruntime.SubAgentInvocationResult{}, newGatewayError(CodeTargetResolveFailed, "resume request child control binding is incomplete", nil)
	}
	resumer, ok := s.local.(LocalChildResumerProvider)
	if !ok {
		return agentruntime.SubAgentInvocationResult{}, newGatewayError(CodeProviderUnavailable, "local provider does not support child resume", nil)
	}
	authorized := AuthorizedInvocation{
		TenantID: req.TenantID, SessionID: req.SessionID, ParentRunID: req.ParentRunID,
		SubAgentRef: req.SubAgentRef, ProviderKind: gatewaycontract.SubAgentProviderLocalAgent,
		InvocationID: req.TaskID, AttemptID: s.ids.NewRequestID(),
		Description: "resume child run " + req.Control.ChildRunID, Trace: req.Trace,
	}
	if err := s.emit(ctx, sink, authorized, observability.EventSubAgentStarted, nil, nil); err != nil {
		return agentruntime.SubAgentInvocationResult{}, err
	}
	result, err := resumer.Resume(ctx, LocalRunResumeRequest{
		TaskID: req.TaskID, TenantID: req.TenantID, SessionID: req.SessionID,
		ParentRunID: req.ParentRunID, Control: req.Control,
		ResponsePayload: append([]byte(nil), req.ResponsePayload...), Trace: req.Trace,
	})
	if err == nil && result.ResultType != agentruntime.SubAgentResultAskUserType {
		err = validateTaskOutput(result)
	}
	if err != nil {
		return agentruntime.SubAgentInvocationResult{}, s.fail(ctx, sink, authorized, nil, err)
	}
	if result.ResultType == agentruntime.SubAgentResultAskUserType {
		if err := s.emit(ctx, sink, authorized, observability.EventSubAgentProgress, askUserProposalPayload(result), nil); err != nil {
			return agentruntime.SubAgentInvocationResult{}, err
		}
		return result, nil
	}
	if err := s.emit(ctx, sink, authorized, observability.EventSubAgentCompleted, resultPayload(result, nil), nil); err != nil {
		return agentruntime.SubAgentInvocationResult{}, err
	}
	return result, nil
}

// askUserProposalPayload 构造 progress 事件的 proposal payload（形状与
// ExtractInteractionProposal 对齐）。
func askUserProposalPayload(result agentruntime.SubAgentInvocationResult) map[string]any {
	details := map[string]any{"child_run_id": result.ChildRunID, "result_type": result.ResultType}
	var proposal map[string]any
	if len(result.Proposal) > 0 && json.Unmarshal(result.Proposal, &proposal) == nil {
		for key, value := range proposal {
			details[key] = value
		}
	}
	return details
}

func (s *Service) rejectNestedInvocation(ctx context.Context, sink agentruntime.SubAgentEventSink, req agentruntime.SubAgentInvocationRequest, cause error) error {
	blocked := AuthorizedInvocation{
		TenantID: req.TenantID, SessionID: req.SessionID, ParentRunID: req.ParentRunID,
		ParentAgentID: req.ParentAgentID, SubAgentRef: req.SubAgentRef,
		InvocationID: req.TaskID, AttemptID: s.ids.NewRequestID(), Description: req.Description, Trace: req.Trace,
	}
	return s.emitStartedThenFailed(ctx, sink, blocked, cause)
}

func (s *Service) provider(
	sink agentruntime.SubAgentEventSink,
	target ResolvedTarget,
	scoped agentruntime.ScopedData,
	depth int,
	req AuthorizedInvocation,
	state *invocationFacts,
) GatewayNext {
	return func(ctx context.Context) (result agentruntime.SubAgentInvocationResult, err error) {
		state.markEntered()
		defer func() {
			if recovered := recover(); recovered != nil {
				state.setOutcome(GatewayProviderOutcomeKnown, false, string(CodePluginFailed))
				result = agentruntime.SubAgentInvocationResult{}
				err = newGatewayError(CodePluginFailed, "provider panicked", nil)
			}
		}()
		switch target.Provider {
		case gatewaycontract.SubAgentProviderLocalAgent:
			if s.local == nil {
				state.setOutcome(GatewayProviderNotStarted, false, string(CodeProviderUnavailable))
				return agentruntime.SubAgentInvocationResult{}, ErrProviderUnavailable
			}
			result, err := s.local.Invoke(ctx, LocalInvocationRequest{Invocation: req, Target: target, ScopedData: scoped, Depth: depth + 1})
			state.setOutcome(GatewayProviderOutcomeKnown, err == nil, SafeErrorCode(err))
			return result, err
		case gatewaycontract.SubAgentProviderRemoteA2A:
			if s.remote == nil || target.Remote == nil {
				state.setOutcome(GatewayProviderNotStarted, false, string(CodeProviderUnavailable))
				return agentruntime.SubAgentInvocationResult{}, ErrProviderUnavailable
			}
			state.setPhase(GatewayProviderRequestWritten)
			updates := &remoteUpdateSink{service: s, sink: sink, invocation: req}
			remoteResult, err := s.remote.Invoke(ctx, *target.Remote, RemoteInvocationRequest{InvocationID: req.InvocationID, Description: req.Description, Trace: req.Trace}, updates)
			state.setOutcome(GatewayProviderOutcomeKnown, err == nil, SafeErrorCode(err))
			if err != nil {
				return agentruntime.SubAgentInvocationResult{}, err
			}
			return agentruntime.SubAgentInvocationResult{Content: remoteResult.Content}, nil
		default:
			return agentruntime.SubAgentInvocationResult{}, ErrProviderUnsupported
		}
	}
}

func validateInvocationRequest(req agentruntime.SubAgentInvocationRequest) error {
	if req.Scope != agentruntime.SubAgentScopePlatformChildRun || req.TenantID == "" || req.SessionID == "" ||
		req.ParentRunID == "" || req.ParentAgentID == "" || req.ParentAgentVersion == "" || req.SubAgentRef == "" ||
		req.TaskID == "" || req.ParentConfigSnapshotRef == "" || req.ParentConfigHash == "" || req.Description == "" {
		return newGatewayError(CodeTargetUnauthorized, "incomplete or untrusted sub-agent invocation", nil)
	}
	return nil
}

func validateResolvedTarget(req agentruntime.SubAgentInvocationRequest, target ResolvedTarget) error {
	if !target.Provider.IsGatewayRouted() || target.Definition.AgentID != req.SubAgentRef || target.Definition.Version == "" ||
		target.Binding.SchemaVersion != ProviderBindingSchemaV2 || target.Binding.BindingID == "" ||
		target.Binding.TargetAgentID != target.Definition.AgentID || target.Binding.TargetVersion != target.Definition.Version ||
		target.Binding.ProviderKind != target.Provider || target.Binding.ConfigSnapshotRef == "" || target.Binding.ConfigHash == "" {
		return newGatewayError(CodeTargetResolveFailed, "resolved target facts are incomplete or inconsistent", nil)
	}
	return nil
}

func validateTaskInput(description string) error {
	if !utf8.ValidString(description) {
		return newGatewayError(CodeTaskInputInvalid, "task input is not valid utf-8", nil)
	}
	return nil
}

func validateTaskOutput(result agentruntime.SubAgentInvocationResult) error {
	if result.Content == "" && result.ContentRef == "" {
		return newGatewayError(CodeTaskOutputInvalid, "task output has neither content nor content_ref", nil)
	}
	if !utf8.ValidString(result.Content) || !utf8.ValidString(result.ContentRef) {
		return newGatewayError(CodeTaskOutputInvalid, "task output is not valid utf-8", nil)
	}
	return nil
}

func (s *Service) emitStartedThenFailed(ctx context.Context, sink agentruntime.SubAgentEventSink, req AuthorizedInvocation, cause error) error {
	if err := s.emit(ctx, sink, req, observability.EventSubAgentStarted, nil, nil); err != nil {
		return errors.Join(cause, err)
	}
	return s.fail(ctx, sink, req, nil, cause)
}

func (s *Service) fail(ctx context.Context, sink agentruntime.SubAgentEventSink, req AuthorizedInvocation, facts *invocationFacts, cause error) error {
	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.terminalTimeout)
	defer cancel()
	eventErr := &observability.EventError{Code: SafeErrorCode(cause), Type: observability.EventErrorInternal, Message: "sub-agent invocation failed"}
	if err := s.emit(flushCtx, sink, req, observability.EventSubAgentFailed, failurePayload(cause, facts), eventErr); err != nil {
		return errors.Join(cause, fmt.Errorf("persist sub-agent terminal: %w", err))
	}
	return cause
}

type remoteUpdateSink struct {
	service    *Service
	sink       agentruntime.SubAgentEventSink
	invocation AuthorizedInvocation
}

func (s *remoteUpdateSink) Emit(ctx context.Context, update RemoteTaskUpdate) error {
	eventType := a2aEventType(update)
	if eventType == "" {
		return nil
	}
	return s.service.emit(ctx, s.sink, s.invocation, eventType, map[string]any{
		"provider_task_id": update.TaskID, "provider_context_id": update.ContextID,
		"provider_state": update.State, "terminal_outcome": update.Terminal,
	}, nil)
}
