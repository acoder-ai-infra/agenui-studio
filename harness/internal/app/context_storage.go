package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

var errContextLedgerReadOnly = errors.New("app context ledger adapter is read-only; messages must use the storage write path")

// maxDerefContentBytes caps how much externalized message content is pulled into
// memory to inline into the model context for a single message.
const maxDerefContentBytes = 1 << 20 // 1 MiB

// storageMessageLedger projects the canonical Message Store into Context's
// immutable snapshot contract. Message writes remain owned by storage/runtime.
// artifacts dereferences externalized (large) message content: a message stores
// only a preview inline and the full body behind ContentRef, so the model must
// see the dereferenced body, not the truncated preview.
type storageMessageLedger struct {
	messages         storage.MessageStore
	artifacts        *artifact.Store
	currentRunID     string
	currentTurnID    string
	currentMessageID string
}

func (l storageMessageLedger) Append(context.Context, string, contextpkg.Message) (contextpkg.Message, error) {
	return contextpkg.Message{}, errContextLedgerReadOnly
}

func (l storageMessageLedger) List(ctx context.Context, sessionID string, afterSequence int64, limit int) ([]contextpkg.Message, error) {
	messages, err := l.messages.ListRecent(ctx, sessionID, limit+int(afterSequence))
	if err != nil {
		return nil, err
	}
	if afterSequence >= int64(len(messages)) {
		return nil, nil
	}
	return l.toContextMessages(ctx, messages[afterSequence:])
}

func (l storageMessageLedger) ListRecent(ctx context.Context, sessionID string, limit int) ([]contextpkg.Message, error) {
	messages, err := l.messages.ListRecent(ctx, sessionID, limit)
	if err != nil {
		return nil, err
	}
	return l.toContextMessages(ctx, messages)
}

func (l storageMessageLedger) GetByIDs(ctx context.Context, sessionID string, ids []string) ([]contextpkg.Message, error) {
	messages, err := l.messages.GetByIDs(ctx, sessionID, ids)
	if err != nil {
		return nil, err
	}
	return l.toContextMessages(ctx, messages)
}

func (l storageMessageLedger) toContextMessages(ctx context.Context, messages []*storage.Message) ([]contextpkg.Message, error) {
	out := make([]contextpkg.Message, 0, len(messages))
	for index, message := range messages {
		if message == nil {
			continue
		}
		content, err := l.resolveContent(ctx, message)
		if err != nil {
			return nil, err
		}
		// 多 Part envelope 正文解码为结构化 Parts + 压扁预览；纯文本消息
		// 原样透传。
		parts, flattened, decoded := contextpkg.DecodePartsEnvelope(content)
		if decoded {
			content = flattened
		}
		messageID := message.ID
		if l.currentMessageID != "" && message.RunID == l.currentRunID && message.TurnID == l.currentTurnID {
			messageID = l.currentMessageID
		}
		out = append(out, contextpkg.Message{
			ID: messageID, SessionID: message.SessionID, Sequence: int64(index + 1),
			IdempotencyKey: messageID, Role: contextpkg.RoleType(message.Role),
			Content: content, Parts: parts, Timestamp: message.CreatedAt,
		})
	}
	return out, nil
}

// resolveContent returns the full message body the model must see: when the
// message externalized its body to ContentRef (large content), dereference the
// artifact; otherwise use the inline body. This model-context adapter fails
// closed on referenced-content errors: substituting a preview would make one
// immutable snapshot resolve to different facts before and after recovery.
func (l storageMessageLedger) resolveContent(ctx context.Context, message *storage.Message) (string, error) {
	if message == nil {
		return "", errors.New("context message is nil")
	}
	if message.ContentRef == "" {
		return message.ContentPreview, nil
	}
	// message:// is the canonical locator for an inline Message Store body. In
	// P0 the complete small body is persisted in ContentPreview; only
	// artifact:// refs require Object Store dereferencing.
	if strings.HasPrefix(message.ContentRef, "message://") {
		return message.ContentPreview, nil
	}
	if !strings.HasPrefix(message.ContentRef, "artifact://") {
		return "", fmt.Errorf("resolve message %s content_ref: unsupported ref scheme", message.ID)
	}
	if l.artifacts == nil {
		return "", fmt.Errorf("resolve message %s content_ref: artifact store unavailable", message.ID)
	}
	tc := observability.MustTraceContext(ctx)
	if tc.TenantID == "" || tc.UserID == "" || tc.SessionID == "" || tc.RunID == "" || tc.SessionID != message.SessionID {
		return "", fmt.Errorf("resolve message %s content_ref: incomplete or mismatched context identity", message.ID)
	}
	// Context Engine may read an earlier Run's message body only within the same
	// tenant, user and Session. The Artifact Store still denies ordinary Runtime
	// actors any cross-Run access.
	actorCtx := artifact.ContextWithActor(ctx, artifact.Actor{
		Role: artifact.ActorContextEngine, TenantID: tc.TenantID, UserID: tc.UserID,
		SessionID: tc.SessionID, RunID: tc.RunID,
	})
	obj, err := l.artifacts.Get(actorCtx, message.ContentRef, artifact.GetOptions{Purpose: artifact.PurposeModelContext})
	if err != nil {
		return "", fmt.Errorf("resolve message %s content_ref: %w", message.ID, err)
	}
	defer obj.Content.Close()
	// Bound the read: never pull an unbounded externalized body fully into memory
	// for a single message. Read one byte past the cap to detect overflow; an
	// oversized body fails closed rather than inlining a truncated blob.
	body, err := io.ReadAll(io.LimitReader(obj.Content, maxDerefContentBytes+1))
	if err != nil {
		return "", fmt.Errorf("read message %s content_ref: %w", message.ID, err)
	}
	if len(body) == 0 || len(body) > maxDerefContentBytes || !utf8.Valid(body) {
		return "", fmt.Errorf("message %s content_ref is empty, oversized, or not valid UTF-8", message.ID)
	}
	return string(body), nil
}

type contextSnapshotBuilder struct{ manager contextpkg.SnapshotManager }

func (b contextSnapshotBuilder) Build(ctx context.Context, req storage.SnapshotBuildRequest) (string, error) {
	trace := observability.MustTraceContext(ctx)
	trace.TenantID = req.TenantID
	trace.SessionID = req.SessionID
	trace.RunID = req.RunID
	trace.AgentID = req.AgentID
	ctx = observability.WithTraceContext(ctx, trace)
	manager := b.manager
	if ledger, ok := manager.Ledger.(storageMessageLedger); ok {
		ledger.currentRunID = req.RunID
		ledger.currentTurnID = req.TurnID
		ledger.currentMessageID = req.MessageID
		manager.Ledger = ledger
	}
	snapshot, err := manager.MaterializeWithAttachments(ctx, req.SessionID, req.RunID, contextAttachmentsFromStorage(req.Attachments))
	if err != nil {
		return "", err
	}
	return snapshot.ID, nil
}

func contextAttachmentsFromStorage(input []storage.ContextAttachmentSnapshot) []contextpkg.AttachmentFact {
	if len(input) == 0 {
		return nil
	}
	out := make([]contextpkg.AttachmentFact, 0, len(input))
	for _, item := range input {
		out = append(out, contextpkg.AttachmentFact{
			Name:        item.Name,
			MimeType:    item.MimeType,
			SizeBytes:   item.SizeBytes,
			ArtifactRef: item.ArtifactRef,
			Type:        item.Type,
		})
	}
	return out
}
