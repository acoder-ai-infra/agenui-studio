package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/AGenUI/agenui-studio/harness/internal/agentbinding"
	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/orchestrator"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
	"github.com/AGenUI/agenui-studio/harness/internal/storagewrite"
)

func runtimeStorageWriter(state agentruntime.RuntimeStateManager, stores storage.Stores, artifacts *artifact.Store) *storagewrite.Executor {
	ports := agentruntime.NewRuntimeStateStorePorts(state)
	ports[storagewrite.StoreRun] = compositionRunPort{runtime: ports[storagewrite.StoreRun], runs: stores.Runs}
	ports[storagewrite.StoreMessage] = runtimeMessagePort{messages: stores.Messages, runs: stores.Runs}
	ports[storagewrite.StoreArtifact] = runtimeArtifactPort{artifacts: artifacts}
	return storagewrite.NewExecutor(ports)
}

// compositionRunPort owns app-level translation for Run payloads emitted by
// Binding and Runtime. Domain packages remain unaware of each other's DTOs.
type compositionRunPort struct {
	runtime storagewrite.StorePort
	runs    storage.RunStore
}

func (p compositionRunPort) Write(ctx context.Context, write storagewrite.Write) (storagewrite.WriteReceipt, error) {
	switch payload := write.Payload.(type) {
	case agentbinding.SuccessPayload:
		if p.runs == nil || payload.Binding.BindingID == "" || payload.Binding.ConfigSnapshotRef == "" {
			return storagewrite.WriteReceipt{}, agentruntime.ErrInvalidStorePayload
		}
		err := p.runs.BindAgentConfig(ctx, write.RunID, payload.Binding.BindingID, payload.Binding.ConfigSnapshotRef)
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, err
	case agentbinding.FailurePayload:
		if p.runs == nil {
			return storagewrite.WriteReceipt{}, agentruntime.ErrInvalidStorePayload
		}
		_, err := p.runs.CompareAndSetStatus(ctx, write.RunID, storage.RunStatusCreated, storage.RunStatusFailed, storage.RunMutation{ErrorCode: string(payload.Code), ErrorMessage: payload.SafeMessage})
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, err
	case orchestrator.PreRuntimeFailure:
		if p.runs == nil {
			return storagewrite.WriteReceipt{}, agentruntime.ErrInvalidStorePayload
		}
		_, err := p.runs.CompareAndSetStatus(ctx, write.RunID, storage.RunStatusCreated, storage.RunStatusFailed, storage.RunMutation{ErrorCode: payload.Error.Code, ErrorMessage: payload.Error.Message})
		return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, err
	default:
		if p.runtime == nil {
			return storagewrite.WriteReceipt{}, agentruntime.ErrInvalidStorePayload
		}
		return p.runtime.Write(ctx, write)
	}
}

type runtimeMessagePort struct {
	messages storage.MessageStore
	runs     storage.RunStore
}

func (p runtimeMessagePort) Write(ctx context.Context, write storagewrite.Write) (storagewrite.WriteReceipt, error) {
	payload, ok := write.Payload.(agentruntime.AssistantMessageWrite)
	if !ok || p.messages == nil || p.runs == nil {
		return storagewrite.WriteReceipt{}, agentruntime.ErrInvalidStorePayload
	}
	run, err := p.runs.Get(ctx, write.RunID)
	if err != nil {
		return storagewrite.WriteReceipt{}, err
	}
	message := &storage.Message{
		ID: payload.MessageID, SessionID: payload.SessionID, TurnID: run.TurnID,
		RunID: payload.RunID, TenantID: write.TenantID, Role: payload.Role,
		Visibility: payload.Visibility, ContentRef: payload.ContentRef,
		ContentPreview: payload.Preview, CreatedAt: payload.CreatedAt,
	}
	if err := p.messages.Append(ctx, message); err != nil {
		existing, getErr := p.messages.Get(ctx, message.ID)
		if getErr != nil || !sameRuntimeMessage(existing, message) {
			return storagewrite.WriteReceipt{}, err
		}
	}
	return storagewrite.WriteReceipt{Store: write.Store, Ref: write.Ref}, nil
}

func sameRuntimeMessage(left, right *storage.Message) bool {
	return left != nil && right != nil &&
		left.ID == right.ID && left.SessionID == right.SessionID && left.TurnID == right.TurnID &&
		left.RunID == right.RunID && left.TenantID == right.TenantID && left.Role == right.Role &&
		left.Visibility == right.Visibility && left.ContentRef == right.ContentRef &&
		left.ContentPreview == right.ContentPreview
}

type runtimeArtifactPort struct {
	artifacts *artifact.Store
}

func (p runtimeArtifactPort) Write(ctx context.Context, write storagewrite.Write) (storagewrite.WriteReceipt, error) {
	payload, ok := write.Payload.(agentruntime.FinalArtifactWrite)
	if !ok || p.artifacts == nil {
		return storagewrite.WriteReceipt{}, agentruntime.ErrInvalidStorePayload
	}
	tc := observability.MustTraceContext(ctx)
	ctx = artifact.ContextWithActor(ctx, artifact.Actor{
		TenantID: write.TenantID, UserID: tc.UserID, SessionID: write.SessionID,
		RunID: write.RunID, AgentID: tc.AgentID, Role: artifact.ActorRuntime,
	})
	meta, err := p.artifacts.Put(ctx, artifact.PutArtifactRequest{
		ArtifactID: payload.ArtifactID, TenantID: write.TenantID, UserID: tc.UserID,
		SessionID: write.SessionID, RunID: write.RunID, OwnerModule: artifact.OwnerModuleRuntime,
		OwnerID: write.RunID, ArtifactType: artifact.ArtifactTypeFinalResult,
		MimeType: payload.ContentType, Name: "final-response.txt", Visibility: artifactVisibility(payload.Visibility),
		Content: strings.NewReader(string(payload.Content)), RetentionPolicy: artifact.RetentionSessionTTL,
		IdempotencyKey: write.IdempotencyKey,
		Metadata:       map[string]string{"content_hash": payload.ContentHash, "size_bytes": fmt.Sprint(payload.SizeBytes)},
	})
	if err != nil {
		return storagewrite.WriteReceipt{}, err
	}
	if meta.ArtifactRef != write.Ref {
		return storagewrite.WriteReceipt{}, fmt.Errorf("artifact ref mismatch: got %s want %s", meta.ArtifactRef, write.Ref)
	}
	return storagewrite.WriteReceipt{Store: write.Store, Ref: meta.ArtifactRef}, nil
}

func artifactVisibility(visibility observability.EventVisibility) artifact.Visibility {
	switch visibility {
	case observability.VisibilityInternal:
		return artifact.VisibilityInternal
	case observability.VisibilityDebug:
		return artifact.VisibilityDebug
	case observability.VisibilityRestricted:
		return artifact.VisibilityRestricted
	default:
		return artifact.VisibilityUserVisible
	}
}
