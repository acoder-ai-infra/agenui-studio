package agentgateway

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

const (
	// DefaultRemotePollInterval is the default non-terminal Task poll interval.
	DefaultRemotePollInterval = 500 * time.Millisecond
	// DefaultA2AMaxResultBytes is owned by the A2A adapter and bounds the
	// protocol-level aggregate result independently of Service policy.
	DefaultA2AMaxResultBytes = 64 * 1024
)

// A2AProvider implements RemoteA2AProvider using the a2a-go JSON-RPC client. All
// A2A SDK types stay inside this file/package boundary.
type A2AProvider struct {
	cards          *agentCardCache
	credentials    CredentialsProvider
	pollInterval   time.Duration
	maxResultBytes int
}

// A2AProviderConfig configures the remote provider.
type A2AProviderConfig struct {
	Credentials    CredentialsProvider
	CardTTL        time.Duration
	PollInterval   time.Duration
	MaxResultBytes int
	Now            func() time.Time
}

// NewA2AProvider constructs the remote provider.
func NewA2AProvider(cfg A2AProviderConfig) (*A2AProvider, error) {
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = DefaultRemotePollInterval
	}
	maxBytes := cfg.MaxResultBytes
	if maxBytes <= 0 {
		maxBytes = DefaultA2AMaxResultBytes
	}
	return &A2AProvider{
		cards:          newAgentCardCache(cfg.Credentials, cfg.CardTTL, cfg.Now),
		credentials:    cfg.Credentials,
		pollInterval:   poll,
		maxResultBytes: maxBytes,
	}, nil
}

var _ RemoteA2AProvider = (*A2AProvider)(nil)

// Invoke performs a single synchronous remote A2A call: discover the card, build
// a per-invocation JSON-RPC client, send the message, and converge a Task to a
// terminal via bounded polling.
func (p *A2AProvider) Invoke(ctx context.Context, target RemoteAgent, req RemoteInvocationRequest, updates RemoteTaskUpdateSink) (RemoteInvocationResult, error) {
	card, err := p.cards.Resolve(ctx, target)
	if err != nil {
		return RemoteInvocationResult{}, err
	}
	endpoint := card.SupportedInterfaces[0]
	endpointURL, err := url.Parse(endpoint.URL)
	if err != nil {
		return RemoteInvocationResult{}, newGatewayError(CodeRemoteTargetInvalid, "invalid endpoint url", err)
	}
	credentialHeader, err := p.cards.credentialHeader(ctx, target)
	if err != nil {
		return RemoteInvocationResult{}, err
	}
	httpClient := newBoundedA2AClient(requestOrigin(endpointURL), credentialHeader, jsonRPCMaxWireBytes, target.Timeout)
	client, err := a2aclient.NewFromCard(ctx, card,
		a2aclient.WithDefaultsDisabled(),
		a2aclient.WithConfig(a2aclient.Config{DisableTenantPropagation: true}),
		a2aclient.WithJSONRPCTransport(httpClient),
	)
	if err != nil {
		return RemoteInvocationResult{}, newGatewayError(CodeA2AProtocolIncompatible, "failed to build A2A client", err)
	}
	defer client.Destroy()

	historyLength := 1
	message := descriptionToMessage(req.Description)
	message.SetMeta("harness_invocation_id", req.InvocationID)
	sendReq := &a2a.SendMessageRequest{
		Message: message,
		Config: &a2a.SendMessageConfig{
			ReturnImmediately:   true,
			AcceptedOutputModes: []string{"text/plain", "application/json"},
			HistoryLength:       &historyLength,
			PushConfig:          nil,
		},
	}
	result, err := client.SendMessage(ctx, sendReq)
	if err != nil {
		return RemoteInvocationResult{}, mapTransportError(ctx, err, CodeRemoteTransportFailed)
	}
	switch value := result.(type) {
	case *a2a.Message:
		// Direct message: no Task, so no a2a_task_* updates are emitted.
		return messageResult(value, p.maxResultBytes)
	case *a2a.Task:
		return p.resolveTask(ctx, client, value, updates)
	default:
		return RemoteInvocationResult{}, newGatewayError(CodeRemoteResultUnsupported, "unexpected send message result", nil)
	}
}

func (p *A2AProvider) resolveTask(ctx context.Context, client *a2aclient.Client, task *a2a.Task, updates RemoteTaskUpdateSink) (RemoteInvocationResult, error) {
	if task.ID == "" || task.ContextID == "" {
		return RemoteInvocationResult{}, newGatewayError(CodeRemoteTaskInvalid, "initial task id/context id empty", nil)
	}
	taskID := task.ID
	contextID := task.ContextID
	if err := emitUpdate(ctx, updates, RemoteTaskUpdate{Kind: RemoteTaskCreated, TaskID: string(taskID), ContextID: contextID, State: string(task.Status.State)}); err != nil {
		p.bestEffortCancel(ctx, client, taskID)
		return RemoteInvocationResult{}, err
	}
	lastState := task.Status.State

	for {
		switch task.Status.State {
		case a2a.TaskStateCompleted:
			if err := emitUpdate(ctx, updates, terminalUpdate(taskID, contextID, task.Status.State)); err != nil {
				return RemoteInvocationResult{}, err
			}
			return completedTaskResult(task, p.maxResultBytes)
		case a2a.TaskStateFailed:
			if err := emitUpdate(ctx, updates, terminalUpdate(taskID, contextID, task.Status.State)); err != nil {
				return RemoteInvocationResult{}, err
			}
			return RemoteInvocationResult{}, newGatewayError(CodeRemoteTaskFailed, "remote task failed", nil)
		case a2a.TaskStateRejected:
			if err := emitUpdate(ctx, updates, terminalUpdate(taskID, contextID, task.Status.State)); err != nil {
				return RemoteInvocationResult{}, err
			}
			return RemoteInvocationResult{}, newGatewayError(CodeRemoteTaskRejected, "remote task rejected", nil)
		case a2a.TaskStateCanceled:
			if err := emitUpdate(ctx, updates, terminalUpdate(taskID, contextID, task.Status.State)); err != nil {
				return RemoteInvocationResult{}, err
			}
			return RemoteInvocationResult{}, newGatewayError(CodeRemoteTaskCancelled, "remote task canceled", nil)
		case a2a.TaskStateInputRequired:
			p.bestEffortCancel(ctx, client, taskID)
			return RemoteInvocationResult{}, newGatewayError(CodeRemoteInputRequired, "remote task requires input; P0 has no resume", nil)
		case a2a.TaskStateAuthRequired:
			p.bestEffortCancel(ctx, client, taskID)
			return RemoteInvocationResult{}, newGatewayError(CodeRemoteAuthRequired, "remote task requires interactive auth", nil)
		case a2a.TaskStateSubmitted, a2a.TaskStateWorking:
			// keep polling
		default:
			p.bestEffortCancel(ctx, client, taskID)
			return RemoteInvocationResult{}, newGatewayError(CodeRemoteStateUnsupported, "unsupported remote task state", nil)
		}

		if err := sleepBounded(ctx, p.pollInterval); err != nil {
			p.bestEffortCancel(ctx, client, taskID)
			return RemoteInvocationResult{}, mapTransportError(ctx, err, CodeRemoteTimeout)
		}
		next, err := getTaskWithRetry(ctx, client, taskID)
		if err != nil {
			p.bestEffortCancel(ctx, client, taskID)
			return RemoteInvocationResult{}, err
		}
		if next.ID != taskID || next.ContextID != contextID {
			return RemoteInvocationResult{}, newGatewayError(CodeRemoteTaskInvalid, "task id/context id changed across polls", nil)
		}
		task = next
		if task.Status.State != lastState {
			if err := emitUpdate(ctx, updates, RemoteTaskUpdate{Kind: RemoteTaskProgress, TaskID: string(taskID), ContextID: contextID, State: string(task.Status.State)}); err != nil {
				p.bestEffortCancel(ctx, client, taskID)
				return RemoteInvocationResult{}, err
			}
			lastState = task.Status.State
		}
	}
}

// getTaskWithRetry queries the same task id, retrying transient failures at most
// twice (200ms, 500ms) within the remaining deadline. It never re-sends the
// original message.
func getTaskWithRetry(ctx context.Context, client *a2aclient.Client, taskID a2a.TaskID) (*a2a.Task, error) {
	backoffs := []time.Duration{200 * time.Millisecond, 500 * time.Millisecond}
	historyLength := 1
	var lastErr error
	for attempt := 0; attempt <= len(backoffs); attempt++ {
		task, err := client.GetTask(ctx, &a2a.GetTaskRequest{ID: taskID, HistoryLength: &historyLength})
		if err == nil {
			return task, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, mapTransportError(ctx, err, CodeRemoteTimeout)
		}
		if attempt < len(backoffs) {
			if sleepErr := sleepBounded(ctx, backoffs[attempt]); sleepErr != nil {
				return nil, mapTransportError(ctx, sleepErr, CodeRemoteTimeout)
			}
		}
	}
	return nil, newGatewayError(CodeRemoteTransportFailed, "get task failed after retries", lastErr)
}

// bestEffortCancel issues at most one CancelTask using a detached, bounded
// context so an abandoned non-terminal task is cleaned up remotely.
func (p *A2AProvider) bestEffortCancel(ctx context.Context, client *a2aclient.Client, taskID a2a.TaskID) {
	if taskID == "" {
		return
	}
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_, _ = client.CancelTask(cancelCtx, &a2a.CancelTaskRequest{ID: taskID})
}

func terminalUpdate(taskID a2a.TaskID, contextID string, state a2a.TaskState) RemoteTaskUpdate {
	outcome := RemoteTerminalFailed
	switch state {
	case a2a.TaskStateCompleted:
		outcome = RemoteTerminalCompleted
	case a2a.TaskStateCanceled:
		outcome = RemoteTerminalCancelled
	}
	return RemoteTaskUpdate{Kind: RemoteTaskTerminal, TaskID: string(taskID), ContextID: contextID, State: string(state), Terminal: outcome}
}

func emitUpdate(ctx context.Context, updates RemoteTaskUpdateSink, update RemoteTaskUpdate) error {
	if updates == nil {
		return nil
	}
	return updates.Emit(ctx, update)
}

func sleepBounded(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func mapTransportError(ctx context.Context, err error, fallback GatewayErrorCode) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return newGatewayError(CodeRemoteTimeout, "remote call deadline exceeded", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return err
	}
	return newGatewayError(fallback, "remote transport failed", err)
}
