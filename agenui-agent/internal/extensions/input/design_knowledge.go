package input

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge"
	dkRuntime "github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge/runtime"
	agenuiextensions "github.com/AGenUI/agenui-studio/agenui-agent/internal/extensions"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

const (
	DesignKnowledgeTransformerID = "agenui.model_input.design_knowledge.v1"
)

type DesignKnowledgeTransformer struct {
	provider dkRuntime.RepositoryProvider
}

func NewDesignKnowledgeTransformer(
	repository *designknowledge.Repository,
) (*DesignKnowledgeTransformer, error) {
	if repository == nil {
		return nil, errors.New("design knowledge transformer: repository is required")
	}
	return NewDesignKnowledgeTransformerWithProvider(
		dkRuntime.MustStaticProvider(repository),
	)
}

func NewDesignKnowledgeTransformerWithProvider(
	provider dkRuntime.RepositoryProvider,
) (*DesignKnowledgeTransformer, error) {
	if provider == nil || provider.Current() == nil {
		return nil, errors.New("design knowledge transformer: provider is required")
	}
	return &DesignKnowledgeTransformer{provider: provider}, nil
}

func (*DesignKnowledgeTransformer) ID() string { return DesignKnowledgeTransformerID }

func (t *DesignKnowledgeTransformer) BeforeModel(
	ctx context.Context,
	req extension.BeforeModelRequest,
) (extension.BeforeModelResult, error) {
	if !agenuiextensions.IsStyleGenerationAgent(req.Ctx.AgentID) {
		return beforeModelResult(req, req.Messages), nil
	}
	// Tool results are durable conversation facts. Once one design-knowledge
	// read succeeded, do not inject the selection catalog again and do not keep
	// offering the same read tool. Repeated catalog injection otherwise makes a
	// tool-using model select the same layout every round until max_turns.
	if hasToolResult(req.Messages, "agenui_read_design_knowledge") {
		result := beforeModelResult(req, req.Messages)
		result.Tools = withoutTool(result.Tools, "agenui_read_design_knowledge")
		return result, nil
	}
	repository, info := dkRuntime.CurrentSnapshot(t.provider)
	if repository == nil {
		log.Printf(
			"designknowledge.generation.catalog.skip agent=%s run=%s session=%s round=%d reason=repository_unavailable source=%s version=%s artifact=%s",
			req.Ctx.AgentID,
			req.Ctx.RunID,
			req.Ctx.SessionID,
			req.Round,
			info.Source,
			info.Version,
			info.ArtifactPath,
		)
		return beforeModelResult(req, req.Messages), nil
	}
	messages := cloneBeforeModelMessages(req.Messages)
	target := lastUserMessageIndex(messages)
	if target < 0 {
		return beforeModelResult(req, messages), nil
	}
	catalog := repository.Catalog(
		designknowledge.KindLayout,
	)
	if len(catalog) == 0 {
		log.Printf(
			"designknowledge.generation.catalog.skip agent=%s run=%s session=%s round=%d reason=empty_catalog source=%s version=%s revision_id=%s revision_hash=%s artifact=%s",
			req.Ctx.AgentID,
			req.Ctx.RunID,
			req.Ctx.SessionID,
			req.Round,
			info.Source,
			info.Version,
			repository.RevisionID(),
			repository.RevisionHash(),
			info.ArtifactPath,
		)
		return beforeModelResult(req, messages), nil
	}
	layoutIDs := catalogIDs(catalog)
	log.Printf(
		"designknowledge.generation.catalog agent=%s run=%s session=%s round=%d source=%s version=%s revision_id=%s revision_hash=%s artifact=%s layout_count=%d layout_ids=%s",
		req.Ctx.AgentID,
		req.Ctx.RunID,
		req.Ctx.SessionID,
		req.Round,
		info.Source,
		info.Version,
		repository.RevisionID(),
		repository.RevisionHash(),
		info.ArtifactPath,
		len(layoutIDs),
		strings.Join(layoutIDs, ","),
	)
	block := renderKnowledgeCatalog(
		repository.RevisionID(),
		repository.RevisionHash(),
		catalog,
	)
	if len(messages[target].Parts) == 0 {
		if strings.Contains(messages[target].Content, block) {
			return beforeModelResult(req, messages), nil
		}
		messages[target].Content = strings.TrimSpace(messages[target].Content) + "\n\n" + block
	} else {
		for _, part := range messages[target].Parts {
			if part.Type == "text" &&
				strings.Contains(part.Text, block) {
				return beforeModelResult(req, messages), nil
			}
		}
		messages[target].Parts = append(messages[target].Parts, extension.BeforeModelPart{
			Type: "text",
			Text: block,
		})
	}
	return beforeModelResult(req, messages), nil
}

func hasToolResult(messages []extension.BeforeModelMessage, toolName string) bool {
	for _, message := range messages {
		if message.Role == "tool" && message.ToolName == toolName &&
			(strings.TrimSpace(message.Content) != "" || len(message.Parts) > 0) {
			return true
		}
	}
	return false
}

func withoutTool(tools []extension.ToolDefinition, name string) []extension.ToolDefinition {
	result := make([]extension.ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		if tool.Name != name {
			result = append(result, tool)
		}
	}
	return result
}

func catalogIDs(catalog []designknowledge.Metadata) []string {
	var layoutIDs []string
	for _, metadata := range catalog {
		if metadata.Kind == designknowledge.KindLayout {
			layoutIDs = append(layoutIDs, metadata.ID)
		}
	}
	return layoutIDs
}

func renderKnowledgeCatalog(
	revisionID string,
	revisionHash string,
	catalog []designknowledge.Metadata,
) string {
	var builder strings.Builder
	fmt.Fprintf(
		&builder,
		"<design-knowledge-catalog revision_id=%q revision_hash=%q>\n",
		revisionID,
		revisionHash,
	)
	builder.WriteString(
		"选择且仅选择一个匹配的 layout_id，并调用 agenui_read_design_knowledge。" +
			"layout_id 必须逐字复制；工具会返回该布局依赖的元素定义和规则。" +
			"只有读取成功并取得 receipt 后才能生成；不得凭目录摘要臆造规则。\n",
	)
	for _, metadata := range catalog {
		fmt.Fprintf(
			&builder,
			"- [%s] %s: %s；适用=%s；不适用=%s\n",
			metadata.Kind,
			metadata.ID,
			metadata.Summary,
			strings.Join(metadata.AppliesTo, "、"),
			strings.Join(metadata.NotFor, "、"),
		)
	}
	builder.WriteString("</design-knowledge-catalog>")
	return builder.String()
}

func lastUserMessageIndex(messages []extension.BeforeModelMessage) int {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == "user" {
			return index
		}
	}
	return -1
}
