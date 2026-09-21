package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	stepartifact "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/artifact"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/integration/knowrag"
	"github.com/AGenUI/agenui-studio/harness/sdk"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

const (
	BinderAgentID               = "agenui_binder"
	BinderAgentVersion          = "1.0.0"
	BinderPrimarySearchToolName = "search_developer_apis"
)

func IsBinderPrimarySearchCall(call extension.ToolCallInfo, agentID string, agentVersions ...string) bool {
	if call.Ctx.AgentID != agentID || call.Name != BinderPrimarySearchToolName {
		return false
	}
	if strings.TrimSpace(call.Ctx.AgentVersion) == "" || len(agentVersions) == 0 {
		return true
	}
	for _, version := range agentVersions {
		if call.Ctx.AgentVersion == version {
			return true
		}
	}
	return false
}

// interceptBinderSearch preserves the model's source-selection query, admits
// the returned immutable source receipts, and refreshes the Binding Workspace.
// It does not choose a source, synthesize a follow-up query, or infer joins.
func (i *TaskInterceptor) interceptBinderSearch(
	ctx context.Context,
	call extension.ToolCallInfo,
	next extension.ToolCallNext,
) (extension.ToolCallOutcome, error) {
	if i == nil || i.store == nil {
		return extension.ToolCallOutcome{}, errors.New("orchestration: Binder search storage is unavailable")
	}
	resolver, ok := i.bindingOperatorScope.(BindingParentScopeResolver)
	if !ok || resolver == nil {
		return extension.ToolCallOutcome{}, errors.New("orchestration: Binder parent scope resolver is unavailable")
	}
	parent, err := resolver.ResolveBindingParentScope(ctx, call.Ctx)
	if err != nil {
		return extension.ToolCallOutcome{}, err
	}
	identity := harness.Identity{
		TenantID: parent.TenantID, UserID: parent.UserID,
		SessionID: parent.SessionID, RunID: parent.RunID,
	}
	query, topK := searchRequest(call.Arguments)
	if strings.TrimSpace(query) == "" {
		return extension.ToolCallOutcome{}, errors.New("Binder search query is required")
	}
	if topK == 0 {
		topK = 3
	}
	// Source facts are immutable for one Harness Run. A bounded Binder repair
	// may call search again; replay the already frozen artifact instead of
	// issuing another query and attempting to overwrite Run state.
	if frozen, loadErr := i.store.Load(ctx, identity, stepartifact.StepSearch); loadErr == nil {
		if err := i.refreshWorkspaceBindingSources(ctx, identity, frozen); err != nil {
			return extension.ToolCallOutcome{}, err
		}
		return extension.ToolCallOutcome{Result: frozen}, nil
	} else if !errors.Is(loadErr, stepartifact.ErrNotFound) {
		return extension.ToolCallOutcome{}, fmt.Errorf("load frozen Binder source receipts: %w", loadErr)
	}
	outcome, err := next(ctx, call.Arguments)
	if err != nil {
		return extension.ToolCallOutcome{}, err
	}
	full, projection, err := knowrag.ProcessToolResultForBinder(
		outcome.Result, query, identity.TenantID, identity.UserID, topK,
	)
	if err != nil {
		return extension.ToolCallOutcome{}, err
	}
	if _, err = i.store.Save(ctx, identity, stepartifact.StepSearch, full); err != nil {
		return extension.ToolCallOutcome{}, fmt.Errorf("persist Binder source receipts: %w", err)
	}
	if err := i.refreshWorkspaceBindingSources(ctx, identity, full); err != nil {
		return extension.ToolCallOutcome{}, err
	}
	return extension.ToolCallOutcome{Result: projection}, nil
}

func (i *TaskInterceptor) refreshWorkspaceBindingSources(
	ctx context.Context, identity harness.Identity, raw string,
) error {
	if i == nil || i.workspace == nil {
		return nil
	}
	sources, err := binderContractSources(raw)
	if err != nil {
		return fmt.Errorf("project Binder sources into Workspace: %w", err)
	}
	if err := i.workspace.RefreshBindingSources(ctx, identity, sources); err != nil {
		return fmt.Errorf("refresh Workspace binding sources: %w", err)
	}
	return nil
}

func binderFrozenSearchIsEmpty(raw string) bool {
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(normalizedJSONText(raw)), &envelope) != nil {
		return false
	}
	var results []json.RawMessage
	var total int
	return json.Unmarshal(envelope["results"], &results) == nil &&
		json.Unmarshal(envelope["total"], &total) == nil &&
		len(results) == 0 && total == 0
}
