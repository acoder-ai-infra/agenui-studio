package harness

import (
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// pipeline_context_apply.go 只保留 Message 与 extension.NormalizedMessage 之间
// 的纯转换助手。ContextContributor / InputNormalizer 的执行机制已下沉到
// kernel.TurnPipeline（R2a）：fragment 不再以 metadata digest 占位，而是经
// OpenTurn → dispatcher 透传进 ModelContext 装配。

// messageToExtension 把 SDK Message 投影为 extension DTO。
func messageToExtension(msg Message) extension.NormalizedMessage {
	parts := make([]extension.NormalizedPart, 0, len(msg.Parts))
	for _, p := range msg.Parts {
		parts = append(parts, extension.NormalizedPart{
			Kind:       string(p.Kind),
			Order:      p.Order,
			MIME:       p.MIME,
			Filename:   p.Filename,
			Hash:       p.Hash,
			Visibility: string(p.Visibility),
			Text:       p.Text,
			JSON:       p.JSON,
			Ref: extension.ArtifactRef{
				ID:        p.Ref.ID,
				TenantID:  p.Ref.TenantID,
				SessionID: p.Ref.SessionID,
				RunID:     p.Ref.RunID,
				MIME:      p.Ref.MIME,
				Filename:  p.Ref.Filename,
				Hash:      p.Ref.Hash,
				Size:      p.Ref.Size,
			},
			Inline: p.Inline,
		})
	}
	return extension.NormalizedMessage{
		Role:       string(msg.Role),
		Parts:      parts,
		Visibility: string(msg.Visibility),
		Metadata:   msg.Metadata,
	}
}

// extensionToMessage 把 extension DTO 折回 SDK Message；空 Role 默认 user。
func extensionToMessage(msg extension.NormalizedMessage) Message {
	parts := make([]MessagePart, 0, len(msg.Parts))
	for _, p := range msg.Parts {
		parts = append(parts, MessagePart{
			Kind:       PartKind(p.Kind),
			Order:      p.Order,
			MIME:       p.MIME,
			Filename:   p.Filename,
			Hash:       p.Hash,
			Visibility: Visibility(p.Visibility),
			Text:       p.Text,
			JSON:       p.JSON,
			Ref: ArtifactRef{
				ID:        p.Ref.ID,
				TenantID:  p.Ref.TenantID,
				SessionID: p.Ref.SessionID,
				RunID:     p.Ref.RunID,
				MIME:      p.Ref.MIME,
				Filename:  p.Ref.Filename,
				Hash:      p.Ref.Hash,
				Size:      p.Ref.Size,
			},
			Inline: p.Inline,
		})
	}
	role := Role(msg.Role)
	if role == "" {
		role = RoleUser
	}
	return Message{
		Role:       role,
		Parts:      parts,
		Visibility: Visibility(msg.Visibility),
		Metadata:   msg.Metadata,
	}
}
