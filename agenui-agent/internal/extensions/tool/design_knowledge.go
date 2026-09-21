package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge"
	dkRuntime "github.com/AGenUI/agenui-studio/agenui-agent/internal/designknowledge/runtime"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

const (
	DesignKnowledgeProviderID = "agenui.tools.design_knowledge.v1"
	ReadDesignKnowledgeName   = "agenui_read_design_knowledge"
)

type DesignKnowledgeProvider struct {
	reader *ReadDesignKnowledge
}

func NewDesignKnowledgeProvider(
	repository *designknowledge.Repository,
	authority *designknowledge.ReceiptAuthority,
	maxDocuments int,
	byteBudget int,
) (*DesignKnowledgeProvider, error) {
	if repository == nil {
		return nil, errors.New("design knowledge tool: repository is required")
	}
	return NewDesignKnowledgeProviderWithProvider(
		dkRuntime.MustStaticProvider(repository),
		authority,
		maxDocuments,
		byteBudget,
	)
}

func NewDesignKnowledgeProviderWithProvider(
	provider dkRuntime.RepositoryProvider,
	authority *designknowledge.ReceiptAuthority,
	maxDocuments int,
	byteBudget int,
) (*DesignKnowledgeProvider, error) {
	if provider == nil || provider.Current() == nil {
		return nil, errors.New("design knowledge tool: provider is required")
	}
	if maxDocuments < 0 || byteBudget < 0 {
		return nil, errors.New("design knowledge tool: limits must not be negative")
	}
	return &DesignKnowledgeProvider{reader: &ReadDesignKnowledge{
		provider:     provider,
		authority:    authority,
		maxDocuments: maxDocuments,
		byteBudget:   byteBudget,
	}}, nil
}

func (*DesignKnowledgeProvider) ID() string { return DesignKnowledgeProviderID }

func (p *DesignKnowledgeProvider) FunctionTools() []extension.FunctionTool {
	return []extension.FunctionTool{p.reader}
}

type ReadDesignKnowledge struct {
	provider     dkRuntime.RepositoryProvider
	authority    *designknowledge.ReceiptAuthority
	maxDocuments int
	byteBudget   int
}

func (*ReadDesignKnowledge) Name() string { return ReadDesignKnowledgeName }

func (t *ReadDesignKnowledge) Invoke(
	ctx context.Context,
	call extension.FunctionCall,
) (*extension.FunctionResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	repository, info := dkRuntime.CurrentSnapshot(t.provider)
	if repository == nil {
		return nil, errors.New("read design knowledge: repository is unavailable")
	}
	var input struct {
		LayoutIDs []string `json:"layout_ids"`
	}
	if err := json.Unmarshal(call.Arguments, &input); err != nil {
		return nil, fmt.Errorf("read design knowledge: decode input: %w", err)
	}
	log.Printf(
		"designknowledge.tool.request agent=%s run=%s session=%s source=%s version=%s revision_id=%s revision_hash=%s artifact=%s layout_ids=%s",
		call.Ctx.AgentID,
		call.Ctx.RunID,
		call.Ctx.SessionID,
		info.Source,
		info.Version,
		repository.RevisionID(),
		repository.RevisionHash(),
		info.ArtifactPath,
		strings.Join(input.LayoutIDs, ","),
	)
	if len(input.LayoutIDs) != 1 {
		logDesignKnowledgeToolReject(
			call,
			info,
			repository,
			"invalid_layout_count",
			input.LayoutIDs,
			nil,
		)
		return nil, errors.New("read design knowledge: layout_ids must contain exactly 1 id")
	}
	for _, id := range input.LayoutIDs {
		metadata, exists := repository.Metadata(id)
		if !exists || metadata.Status != "enabled" ||
			metadata.Kind != designknowledge.KindLayout {
			logDesignKnowledgeToolReject(
				call,
				info,
				repository,
				"layout_not_enabled",
				input.LayoutIDs,
				nil,
			)
			return nil, fmt.Errorf(
				"read design knowledge: %q is not an enabled layout",
				id,
			)
		}
	}
	layoutSummaries, layoutHashes := metadataLogValues(repository, input.LayoutIDs)
	log.Printf(
		"designknowledge.tool.read agent=%s run=%s session=%s source=%s version=%s revision_id=%s revision_hash=%s artifact=%s layout_ids=%s layout_summaries=%q layout_hashes=%s",
		call.Ctx.AgentID,
		call.Ctx.RunID,
		call.Ctx.SessionID,
		info.Source,
		info.Version,
		repository.RevisionID(),
		repository.RevisionHash(),
		info.ArtifactPath,
		strings.Join(input.LayoutIDs, ","),
		strings.Join(layoutSummaries, " | "),
		strings.Join(layoutHashes, ","),
	)
	documents, err := repository.Resolve(input.LayoutIDs, designknowledge.ResolveOptions{
		MaxDocuments: t.maxDocuments,
		ByteBudget:   t.byteBudget,
	})
	if err != nil {
		logDesignKnowledgeToolReject(
			call,
			info,
			repository,
			"resolve_failed",
			input.LayoutIDs,
			nil,
		)
		return nil, fmt.Errorf("read design knowledge: %w", err)
	}
	type documentPayload struct {
		ID      string               `json:"id"`
		Kind    designknowledge.Kind `json:"kind"`
		Version string               `json:"version"`
		Content string               `json:"content"`
	}
	output := struct {
		RevisionID   string                  `json:"revision_id"`
		RevisionHash string                  `json:"revision_hash"`
		Receipt      designknowledge.Receipt `json:"receipt"`
		Documents    []documentPayload       `json:"documents"`
	}{
		RevisionID:   repository.RevisionID(),
		RevisionHash: repository.RevisionHash(),
		Documents:    make([]documentPayload, 0, len(documents)),
	}
	if t.authority == nil {
		return nil, errors.New("read design knowledge: receipt authority is required")
	}
	output.Receipt, err = t.authority.Issue(
		designknowledge.ReceiptScope{
			TenantID: call.Ctx.TenantID, UserID: call.Ctx.UserID,
			SessionID: call.Ctx.SessionID, RunID: call.Ctx.RunID,
			AgentID: call.Ctx.AgentID,
		},
		repository.RevisionID(),
		repository.RevisionHash(),
		input.LayoutIDs,
		documents,
	)
	if err != nil {
		return nil, fmt.Errorf("read design knowledge: issue receipt: %w", err)
	}
	for _, document := range documents {
		output.Documents = append(output.Documents, documentPayload{
			ID:      document.Metadata.ID,
			Kind:    document.Metadata.Kind,
			Version: document.Metadata.Version,
			Content: document.Content,
		})
	}
	raw, err := json.Marshal(output)
	if err != nil {
		return nil, fmt.Errorf("read design knowledge: encode output: %w", err)
	}
	return &extension.FunctionResult{
		Data:     raw,
		MimeType: "application/json",
	}, nil
}

func metadataLogValues(
	repository *designknowledge.Repository,
	ids []string,
) ([]string, []string) {
	summaries := make([]string, 0, len(ids))
	hashes := make([]string, 0, len(ids))
	for _, id := range ids {
		metadata, exists := repository.Metadata(id)
		if !exists {
			continue
		}
		summaries = append(summaries, metadata.Summary)
		hashes = append(hashes, metadata.ContentHash)
	}
	return summaries, hashes
}

func logDesignKnowledgeToolReject(
	call extension.FunctionCall,
	info dkRuntime.SnapshotInfo,
	repository *designknowledge.Repository,
	reason string,
	layoutIDs []string,
	_ []string,
) {
	availableLayouts := catalogIDsForKind(repository, designknowledge.KindLayout)
	log.Printf(
		"designknowledge.tool.reject agent=%s run=%s session=%s reason=%s source=%s version=%s revision_id=%s revision_hash=%s artifact=%s layout_ids=%s available_layout_ids=%s",
		call.Ctx.AgentID,
		call.Ctx.RunID,
		call.Ctx.SessionID,
		reason,
		info.Source,
		info.Version,
		repository.RevisionID(),
		repository.RevisionHash(),
		info.ArtifactPath,
		strings.Join(layoutIDs, ","),
		strings.Join(availableLayouts, ","),
	)
}

func catalogIDsForKind(
	repository *designknowledge.Repository,
	kind designknowledge.Kind,
) []string {
	if repository == nil {
		return nil
	}
	metadata := repository.Catalog(kind)
	result := make([]string, 0, len(metadata))
	for _, item := range metadata {
		result = append(result, item.ID)
	}
	return result
}
