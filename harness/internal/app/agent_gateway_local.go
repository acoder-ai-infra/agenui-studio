package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/AGenUI/agenui-studio/harness/internal/agentbinding"
	"github.com/AGenUI/agenui-studio/harness/internal/agentgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/control"
	"github.com/AGenUI/agenui-studio/harness/internal/dispatcher"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/orchestrator"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

type localChildRunExecutor struct {
	orchestrator orchestrator.Orchestrator
	runs         storage.RunStore
	snapshots    contextpkg.SnapshotManager
	ids          observability.IDGenerator
	// broker fans child-run events onto the session channel so the parent
	// run-tree SSE/WS stream can pick them up. The child's inline event channel
	// is otherwise consumed only for the return value (collectChildResult), so
	// without this its runtime events would be persisted but never streamed.
	broker protocol.EventBroker
	// resume 桥接 formalRunDispatcher 的受托子 Run 恢复入口；dispatcher 在
	// gateway 之后构造，Composition Root 通过 bridge 回填。
	resume *childResumeBridge
}

// childResumeBridge 是 gateway → dispatcher 的后置绑定点。
type childResumeBridge struct {
	dispatcher atomic.Pointer[formalRunDispatcher]
}

func (e localChildRunExecutor) ExecuteChild(ctx context.Context, req agentgateway.LocalRunExecutionRequest) (agentruntime.SubAgentInvocationResult, error) {
	result, err := e.executeChildAttempt(ctx, req)
	var retry *agentruntime.OutputRetryError
	if err == nil || !errors.As(err, &retry) || retry.RetryBudget <= 0 {
		return result, err
	}
	for attempt := 0; attempt < retry.RetryBudget; attempt++ {
		repair := req
		repair.Description = appendOutputRepairFeedback(req.Description, retry.RepairFeedback)
		result, err = e.executeChildAttempt(ctx, repair)
		if err == nil {
			return result, nil
		}
		if !errors.As(err, &retry) {
			return result, err
		}
	}
	return result, err
}

func (e localChildRunExecutor) executeChildAttempt(ctx context.Context, req agentgateway.LocalRunExecutionRequest) (agentruntime.SubAgentInvocationResult, error) {
	if e.orchestrator == nil || e.runs == nil || e.ids == nil {
		return agentruntime.SubAgentInvocationResult{}, agentgateway.ErrProviderUnavailable
	}
	childRunID := e.ids.NewRequestID()
	if childRunID == "" {
		return agentruntime.SubAgentInvocationResult{}, errors.New("generate child run id")
	}
	trace := req.Trace
	trace.TenantID, trace.SessionID, trace.RunID = req.TenantID, req.SessionID, childRunID
	trace.AgentID, trace.AgentVersion = req.Target.Definition.AgentID, req.Target.Definition.Version
	ctx = observability.WithTraceContext(ctx, trace)
	parent, err := e.runs.Get(ctx, req.ParentRunID)
	if err != nil {
		return agentruntime.SubAgentInvocationResult{}, fmt.Errorf("load parent run: %w", err)
	}
	if parent.TenantID != req.TenantID || parent.SessionID != req.SessionID {
		return agentruntime.SubAgentInvocationResult{}, agentgateway.ErrTargetUnauthorized
	}
	snapshot, err := e.snapshots.MaterializeDerived(ctx, req.SessionID, childRunID, "agent_gateway_child", nil)
	if err != nil {
		return agentruntime.SubAgentInvocationResult{}, fmt.Errorf("create child context snapshot: %w", err)
	}
	run := &storage.Run{
		RunID: childRunID, SessionID: req.SessionID, ParentRunID: req.ParentRunID,
		TenantID: req.TenantID, TurnID: parent.TurnID, AgentID: req.Target.Definition.AgentID,
		Runtime: string(req.Target.Definition.Runtime.Type), Status: storage.RunStatusCreated,
		TraceID: trace.TraceID, ContextSnapshotRef: snapshot.ID,
	}
	if err := e.runs.Create(ctx, run); err != nil {
		return agentruntime.SubAgentInvocationResult{}, fmt.Errorf("create child run: %w", err)
	}
	result, err := e.orchestrator.Run(ctx, orchestrator.RunRequest{
		BindingRequest: agentbinding.BindingRequest{
			SchemaVersion: agentbinding.BindingSchemaVersion, BindingID: e.ids.NewRequestID(),
			SessionID: req.SessionID, RunID: childRunID,
			Request: &agentbinding.Selection{AgentID: req.Target.Definition.AgentID, AgentVersion: req.Target.Definition.Version},
		},
		ParentRunID: req.ParentRunID,
		Input: []agentruntime.Message{{
			Role: "user", Content: req.Description,
			Parts: childInputParts(req.Description, req.InputParts),
		}},
		ContextSnapshotRef: snapshot.ID, ScopedData: req.ScopedData,
		UserID: trace.UserID, TenantID: req.TenantID, Trace: trace,
		ResultVisibility: observability.VisibilityInternal,
		Metadata: map[string]string{
			"agent_gateway.task_id": req.TaskID, "agent_gateway.attempt_id": req.AttemptID,
			"agent_gateway.depth": fmt.Sprint(req.Depth), "agent_gateway.parent_run_id": req.ParentRunID,
		},
		Policy: dispatcher.DispatchPolicy{EstimatedDuration: 0},
	})
	if err != nil {
		return agentruntime.SubAgentInvocationResult{ChildRunID: childRunID, IsError: true}, err
	}
	if result == nil || result.Mode != dispatcher.ModeInline || result.Events == nil {
		return agentruntime.SubAgentInvocationResult{ChildRunID: childRunID, IsError: true}, errors.New("local Agent Gateway requires an inline child executor")
	}
	return collectChildResult(ctx, childRunID, result.Events, e.broker)
}

// childInputParts keeps a task description visible when the child receives a
// multimodal attachment. Eino projects any non-empty Parts list as the model's
// authoritative input and intentionally clears Message.Content; without this
// leading text part an inherited image would replace the Host-composed task,
// contracts, and edit instructions.
func childInputParts(description string, inherited []contextpkg.ContentPart) []contextpkg.ContentPart {
	if len(inherited) == 0 {
		return nil
	}
	parts := make([]contextpkg.ContentPart, 0, len(inherited)+1)
	if text := strings.TrimSpace(description); text != "" {
		parts = append(parts, contextpkg.ContentPart{Kind: "text", Text: text})
	}
	parts = append(parts, inherited...)
	return parts
}

func appendOutputRepairFeedback(description, feedback string) string {
	const maxFeedbackRunes = 1200
	feedback = strings.TrimSpace(feedback)
	if feedback == "" {
		feedback = "The previous response did not satisfy the output contract. Return a complete corrected response and preserve unrelated requested content."
	}
	runes := []rune(feedback)
	if len(runes) > maxFeedbackRunes {
		feedback = string(runes[:maxFeedbackRunes]) + "…"
	}
	return description + "\n\n<output-repair>\n" + feedback + "\n</output-repair>"
}

// ResumeChild 凭 child control 绑定走 canonical Resume 正门恢复子 Run，并
// inline 收集其后续事件直至终态（或下一次 ask_user 上交）。
func (e localChildRunExecutor) ResumeChild(ctx context.Context, req agentgateway.LocalRunResumeRequest) (agentruntime.SubAgentInvocationResult, error) {
	if e.resume == nil {
		return agentruntime.SubAgentInvocationResult{}, agentgateway.ErrProviderUnavailable
	}
	dispatcher := e.resume.dispatcher.Load()
	if dispatcher == nil {
		return agentruntime.SubAgentInvocationResult{}, agentgateway.ErrProviderUnavailable
	}
	childRunID := req.Control.ChildRunID
	child, err := e.runs.Get(ctx, childRunID)
	if err != nil {
		return agentruntime.SubAgentInvocationResult{}, fmt.Errorf("load child run: %w", err)
	}
	if child.ParentRunID != req.ParentRunID || child.TenantID != req.TenantID || child.SessionID != req.SessionID {
		return agentruntime.SubAgentInvocationResult{}, agentgateway.ErrTargetUnauthorized
	}
	trace := req.Trace
	trace.TenantID, trace.SessionID, trace.RunID = req.TenantID, req.SessionID, childRunID
	ctx = observability.WithTraceContext(ctx, trace)
	events, release, err := dispatcher.ResumeChildInline(ctx, control.ResumeRequest{
		RunID:        childRunID,
		RequestID:    req.Control.RequestID,
		CheckpointID: req.Control.CheckpointID,
		ResumeToken:  req.Control.ControlTicket,
		Response:     json.RawMessage(req.ResponsePayload),
	})
	if err != nil {
		return agentruntime.SubAgentInvocationResult{}, fmt.Errorf("resume child run: %w", err)
	}
	defer release()
	return collectChildResult(ctx, childRunID, events, e.broker)
}

var _ agentgateway.LocalChildResumer = localChildRunExecutor{}

func collectChildResult(ctx context.Context, childRunID string, events <-chan observability.AgentEvent, broker protocol.EventBroker) (agentruntime.SubAgentInvocationResult, error) {
	var content strings.Builder
	var final agentruntime.FinalResponseIndex
	for {
		select {
		case <-ctx.Done():
			return agentruntime.SubAgentInvocationResult{ChildRunID: childRunID, IsError: true}, ctx.Err()
		case event, ok := <-events:
			if ok && broker != nil {
				// Tee onto the broker (keyed by SessionID/RunID) so the parent
				// run-tree stream sees child events live. Persistence already
				// happened upstream; this is stream fan-out only.
				_ = broker.Publish(ctx, event)
			}
			if !ok {
				if final.ContentRef == "" {
					return agentruntime.SubAgentInvocationResult{ChildRunID: childRunID, IsError: true}, errors.New("child run ended without final response")
				}
				text := content.String()
				if text == "" {
					text = final.Preview
				}
				return agentruntime.SubAgentInvocationResult{ChildRunID: childRunID, Content: text, ContentRef: final.ContentRef}, nil
			}
			switch event.EventType {
			case observability.EventAgentTextDelta:
				content.WriteString(eventText(event.Payload))
			case observability.EventFinalResponse:
				if err := json.Unmarshal(event.Payload, &final); err != nil {
					return agentruntime.SubAgentInvocationResult{ChildRunID: childRunID, IsError: true}, fmt.Errorf("decode child final response: %w", err)
				}
			case observability.EventControlRequestCreated:
				// 子 Run 进入 ask_user 等待：把 proposal 与 control 绑定上交
				// 给 gateway/父 Run，本次 attempt 到此收尾（child 保持
				// waiting_control，事件流随 runtime return 关闭）。
				result, err := childAskUserResult(childRunID, event.Payload)
				if err != nil {
					return agentruntime.SubAgentInvocationResult{ChildRunID: childRunID, IsError: true}, err
				}
				drainChildEvents(ctx, events, broker)
				return result, nil
			case observability.EventRunFailed, observability.EventRunCancelled, observability.EventRunExpired:
				if retry := outputRetryFromFailureEvent(event); retry != nil {
					return agentruntime.SubAgentInvocationResult{ChildRunID: childRunID, IsError: true}, retry
				}
				message := "child run terminated"
				if event.Error != nil && event.Error.Message != "" {
					message = event.Error.Message
				}
				return agentruntime.SubAgentInvocationResult{ChildRunID: childRunID, IsError: true}, errors.New(message)
			}
		}
	}
}

func outputRetryFromFailureEvent(event observability.AgentEvent) *agentruntime.OutputRetryError {
	if event.EventType != observability.EventRunFailed || event.Error == nil || event.Error.Code != "OUTPUT_VALIDATION_RETRY" {
		return nil
	}
	var payload struct {
		RetryBudget    int    `json:"retry_budget"`
		RepairFeedback string `json:"repair_feedback"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil || payload.RetryBudget <= 0 {
		return nil
	}
	return agentruntime.NewOutputRetryError("output_validator", event.Error.Message, payload.RepairFeedback, payload.RetryBudget)
}

// drainChildEvents 在提前返回后继续把残余 child 事件 tee 到 broker，避免
// runtime 因下游停止消费而阻塞。
func drainChildEvents(ctx context.Context, events <-chan observability.AgentEvent, broker protocol.EventBroker) {
	go func() {
		for event := range events {
			if broker != nil {
				_ = broker.Publish(ctx, event)
			}
		}
	}()
}

// childAskUserResult 把 child 的 control_request_created payload 转成
// ask_user 上交结果：control 绑定进 ChildControl，业务字段（剥除票据与
// canonical 内部字段后）作为 proposal 原文；中断根因 info 里的工具业务
// 字段（如 ask_user 的 questions）一并提升。
func childAskUserResult(childRunID string, payload json.RawMessage) (agentruntime.SubAgentInvocationResult, error) {
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		return agentruntime.SubAgentInvocationResult{}, fmt.Errorf("decode child control payload: %w", err)
	}
	requestID, _ := fields["request_id"].(string)
	checkpointID, _ := fields["checkpoint_id"].(string)
	ticket, _ := fields["control_ticket"].(string)
	if requestID == "" || ticket == "" {
		return agentruntime.SubAgentInvocationResult{}, errors.New("child control payload missing request binding")
	}
	proposal := make(map[string]any, len(fields))
	for key, value := range fields {
		switch key {
		case "request_id", "checkpoint_id", "control_ticket", "resume_token", "interrupt_contexts", "required":
			continue
		}
		proposal[key] = value
	}
	// 下钻中断根因 info：ask_user 等工具的业务字段（questions 等）在
	// interrupt_contexts 里，不在 payload 顶层。
	if contexts, ok := fields["interrupt_contexts"].([]any); ok {
		for _, item := range contexts {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if rootCause, _ := entry["is_root_cause"].(bool); !rootCause {
				continue
			}
			info, ok := entry["info"].(map[string]any)
			if !ok {
				continue
			}
			for key, value := range info {
				if _, exists := proposal[key]; !exists {
					proposal[key] = value
				}
			}
			break
		}
	}
	if _, ok := proposal["kind"]; !ok {
		if controlType, _ := fields["type"].(string); controlType != "" {
			proposal["kind"] = controlType
		}
	}
	if _, ok := proposal["prompt"]; !ok {
		if questions, exists := proposal["questions"]; exists {
			if digest, err := json.Marshal(questions); err == nil {
				proposal["prompt"] = string(digest)
			}
		}
	}
	proposalRaw, err := json.Marshal(proposal)
	if err != nil {
		return agentruntime.SubAgentInvocationResult{}, fmt.Errorf("encode child proposal: %w", err)
	}
	return agentruntime.SubAgentInvocationResult{
		ChildRunID: childRunID,
		ResultType: agentruntime.SubAgentResultAskUserType,
		Proposal:   proposalRaw,
		ChildControl: &agentruntime.SubAgentChildControl{
			ChildRunID:    childRunID,
			RequestID:     requestID,
			CheckpointID:  checkpointID,
			ControlTicket: ticket,
		},
	}, nil
}

var _ agentgateway.LocalRunExecutor = localChildRunExecutor{}
