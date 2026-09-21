package context

import (
	"encoding/json"
	"strings"
	"time"
)

// RoleType identifies the role of a message participant.
type RoleType string

const (
	RoleSystem    RoleType = "system"
	RoleUser      RoleType = "user"
	RoleAssistant RoleType = "assistant"
	RoleTool      RoleType = "tool"
)

// Message is a framework-agnostic chat message.
// It does not depend on any external LLM framework types.
type Message struct {
	ID             string `json:"id"`
	SessionID      string `json:"session_id,omitempty"`
	Sequence       int64  `json:"sequence,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// CurrentInput 是单次上下文构建的瞬态标记，不属于 Ledger/Snapshot 持久化事实。
	CurrentInput bool     `json:"-"`
	Role         RoleType `json:"role"`
	Content      string   `json:"content"`
	// Parts 是多 Part 输入的结构化事实（文本/JSON/artifact 引用）。
	// 纯文本消息为空，序列化形状与历史数据一致（omitempty）；非空时
	// Content 保存压扁预览，Parts 才是模型可见输入的权威形状。
	Parts      []ContentPart  `json:"parts,omitempty"`
	ToolCalls  []ToolCall     `json:"tool_calls,omitempty"`
	ToolResult *ToolResult    `json:"tool_result,omitempty"`
	Timestamp  time.Time      `json:"timestamp"`
	Extra      map[string]any `json:"extra,omitempty"`
}

// ContentPart 是 framework-agnostic 的单个输入 Part。大二进制一律以
// ArtifactRef 引用，Ledger 不存内联字节。
type ContentPart struct {
	// Kind 取 text | json | image_ref | file_ref | artifact_ref | inline_binary。
	Kind        string          `json:"kind"`
	MIME        string          `json:"mime,omitempty"`
	Filename    string          `json:"filename,omitempty"`
	Hash        string          `json:"hash,omitempty"`
	Text        string          `json:"text,omitempty"`
	JSON        json.RawMessage `json:"json,omitempty"`
	ArtifactRef string          `json:"artifact_ref,omitempty"`
	// Inline 在 Kind == inline_binary 时承载受上限约束的小二进制 payload
	//（如视频帧，ADR-013）。JSON 序列化为 base64，随 parts envelope 持久化。
	Inline []byte `json:"inline,omitempty"`
}

// ToolCall and ToolResult preserve the model protocol relationship through
// snapshotting, trimming, replay, and runtime materialization.
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type ToolResult struct {
	CallID  string `json:"call_id"`
	Name    string `json:"name,omitempty"`
	Content string `json:"content,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

// AttachmentFact is immutable structured context for a user-supplied artifact.
// The bytes remain in Artifact Store; the snapshot freezes only addressing and
// metadata so tools can resolve the exact object without prompt parsing.
type AttachmentFact struct {
	Name        string `json:"name"`
	MimeType    string `json:"mime_type"`
	SizeBytes   int64  `json:"size_bytes"`
	ArtifactRef string `json:"artifact_ref,omitempty"`
	Type        string `json:"type,omitempty"`
}

// MessagePartsFromAttachments projects immutable snapshot attachment facts into
// the canonical multimodal shape consumed by model runtimes. The text is
// repeated as the first part because providers use either Message.Content or
// Message.Parts; once an image/file part exists, Parts is authoritative.
//
// This is a mechanical projection only: it does not inspect filenames or infer
// business meaning from an attachment.
func MessagePartsFromAttachments(text string, attachments []AttachmentFact) []ContentPart {
	if len(attachments) == 0 {
		return nil
	}
	parts := make([]ContentPart, 0, len(attachments)+1)
	if text = strings.TrimSpace(text); text != "" {
		parts = append(parts, ContentPart{Kind: "text", Text: text})
	}
	for _, attachment := range attachments {
		if strings.TrimSpace(attachment.ArtifactRef) == "" {
			continue
		}
		kind := "file_ref"
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(attachment.MimeType)), "image/") || strings.EqualFold(strings.TrimSpace(attachment.Type), "image") {
			kind = "image_ref"
		}
		parts = append(parts, ContentPart{
			Kind: kind, MIME: attachment.MimeType, Filename: attachment.Name,
			ArtifactRef: attachment.ArtifactRef,
		})
	}
	if len(parts) == 1 && parts[0].Kind == "text" {
		return nil
	}
	return parts
}

// ContextSlot classifies the type of a context fragment.
type ContextSlot string

const (
	SlotSystemPrompt  ContextSlot = "system_prompt"
	SlotAgentIdentity ContextSlot = "agent_identity"
	SlotPolicies      ContextSlot = "policies"
	SlotUserProfile   ContextSlot = "user_profile"
	SlotMemory        ContextSlot = "memory"
	SlotRAG           ContextSlot = "rag"
	SlotSummary       ContextSlot = "summary"
	SlotWorkspace     ContextSlot = "workspace"
	SlotConversation  ContextSlot = "conversation"
	SlotToolResult    ContextSlot = "tool_result"
	SlotRuntime       ContextSlot = "runtime"
)

// Stability indicates how frequently a fragment changes.
// Used for cache partitioning and sort ordering.
type Stability string

const (
	StabilityStable    Stability = "stable"    // almost never changes (system prompt)
	StabilitySemi      Stability = "semi"      // changes occasionally within a session
	StabilityDynamic   Stability = "dynamic"   // changes every turn
	StabilityEphemeral Stability = "ephemeral" // can be dropped at any time
)

// stabilityRank defines sort ordering: lower value = earlier in prompt.
var stabilityRank = map[Stability]int{
	StabilityStable:    0,
	StabilitySemi:      1,
	StabilityDynamic:   2,
	StabilityEphemeral: 3,
}

// ContextFragment is the standard carrier of context data.
// Sources produce fragments; Builder consumes them.
type ContextFragment struct {
	Slot       ContextSlot `json:"slot"`
	Stability  Stability   `json:"stability"`
	Priority   int         `json:"priority"`
	Pinned     bool        `json:"pinned"`
	TokenCost  int         `json:"token_cost"`
	Generation int         `json:"generation"`
	Source     string      `json:"source"`
	Role       RoleType    `json:"role"`
	Content    string      `json:"content"`
	Messages   []*Message  `json:"-"`
	Ordinal    int64       `json:"ordinal,omitempty"`
	PairingIDs []string    `json:"pairing_ids,omitempty"`
}

// ContextBudget describes the token budget for a build operation.
type ContextBudget struct {
	Limit          int `json:"limit"`
	ResponseBuffer int `json:"response_buffer"`
}

// TrimRecord logs a single trim operation.
type TrimRecord struct {
	Fragment ContextFragment `json:"fragment"`
	Reason   string          `json:"reason"`
}

// PathOwnership classifies who can write to a state path.
type PathOwnership string

const (
	PathRuleOwned  PathOwnership = "rule_owned"
	PathAgentOwned PathOwnership = "agent_owned"
	PathShared     PathOwnership = "shared"
)

// PathSpec describes a state path's governance rules.
type PathSpec struct {
	Pattern   string        `json:"pattern"`
	Ownership PathOwnership `json:"ownership"`
}

// Writer identifies who wrote a state mutation.
type Writer struct {
	Role string `json:"role"`
	Name string `json:"name"`
}

// WriteRequest is a state write operation.
type WriteRequest struct {
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
	Value     any    `json:"value"`
	Writer    Writer `json:"writer"`
}
