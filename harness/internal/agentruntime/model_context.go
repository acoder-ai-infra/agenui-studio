package agentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/mcp"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/skill"
)

const ModelContextPackageSchemaVersion = "harness.model_context.v1"

var (
	ErrModelContextPackageMissing      = errors.New("model context package missing")
	ErrModelContextIntegrityInvalid    = errors.New("model context package integrity invalid")
	ErrProductionContextDependency     = errors.New("production model context dependency missing")
	ErrProductionContextSnapshot       = errors.New("production context snapshot invalid")
	ErrProductionContextUnavailable    = errors.New("production context snapshot unavailable")
	ErrProductionCapabilityUnavailable = errors.New("production capability dependency unavailable")
	ErrProductionRunBinding            = errors.New("production run binding identity invalid")
	ErrProductionToolSnapshot          = errors.New("production tool snapshot invalid")
	ErrProductionMCPSnapshot           = errors.New("production MCP snapshot invalid")
	ErrCapabilityToolSchemaInvalid     = errors.New("capability tool schema invalid or conflicting")
	ErrCurrentInputNotFrozen           = errors.New("current input missing from frozen context snapshot")
	ErrCurrentInputMissing             = errors.New("current input missing from model context package")
	ErrSystemPromptResolverMissing     = errors.New("system prompt resolver missing")
	ErrSystemPromptSnapshotMismatch    = errors.New("system prompt snapshot mismatch")
	ErrSystemPromptBudgetExceeded      = errors.New("system prompt exceeds model context budget")
)

type modelContextPackageKey struct{}

func WithModelContextPackage(ctx context.Context, pkg ModelContextPackage) context.Context {
	return context.WithValue(ctx, modelContextPackageKey{}, pkg)
}

func ModelContextPackageFrom(ctx context.Context) (ModelContextPackage, bool) {
	pkg, ok := ctx.Value(modelContextPackageKey{}).(ModelContextPackage)
	return pkg, ok
}

type RuntimeContextAssembler interface {
	Build(ctx context.Context, req RuntimeContextAssemblyRequest) (ModelContextPackage, error)
}

type ContextSnapshotProvider interface {
	Materialize(ctx context.Context, req RuntimeContextAssemblyRequest) (ContextSnapshot, error)
}

// SystemPromptResolver 按 Registry 已冻结的版本读取受信任 system prompt。
// 它不处理用户输入或会话上下文，也不允许在运行时解析 latest。
type SystemPromptResolver interface {
	ResolveSystemPrompt(ctx context.Context, req SystemPromptResolveRequest) (SystemPromptSnapshot, error)
}

type SystemPromptResolveRequest struct {
	Ref                 string `json:"ref"`
	Version             string `json:"version"`
	ExpectedPromptHash  string `json:"expected_prompt_hash,omitempty"`
	ExpectedSnapshotRef string `json:"expected_snapshot_ref,omitempty"`
	ExpectedContentHash string `json:"expected_content_hash,omitempty"`
}

type SystemPromptSnapshot struct {
	Ref         string   `json:"ref"`
	Version     string   `json:"version"`
	SnapshotRef string   `json:"snapshot_ref"`
	ContentHash string   `json:"content_hash"`
	PromptHash  string   `json:"prompt_hash"`
	Content     string   `json:"-"`
	Variables   []string `json:"variables,omitempty"`
}

type CapabilitySnapshotProvider interface {
	Resolve(ctx context.Context, req RuntimeContextAssemblyRequest) (CapabilitySnapshot, error)
}

type TokenBudgetAllocator interface {
	Allocate(ctx context.Context, req RuntimeContextAssemblyRequest, snapshot ContextSnapshot, capabilities CapabilitySnapshot) (ModelTokenBudget, error)
}

type PackageGuardrail interface {
	Validate(ctx context.Context, pkg ModelContextPackage) error
}

type RuntimeContextAssemblyRequest struct {
	Run          RunRequest
	Handle       AgentHandle
	Capabilities RuntimeCapabilities
	Trace        observability.TraceContext
}

type ContextSnapshot struct {
	Ref          string `json:"ref,omitempty"`
	ContentHash  string `json:"content_hash,omitempty"`
	LastSequence int64  `json:"last_sequence,omitempty"`
	// Messages are transient resolved facts used only during package assembly.
	// The durable contract is Ref + ContentHash, never a copied large prompt.
	Messages    []contextpkg.Message         `json:"-"`
	Fragments   []contextpkg.ContextFragment `json:"-"`
	Attachments []contextpkg.AttachmentFact  `json:"-"`
}

type CapabilitySnapshot struct {
	Tools             []string                     `json:"tools,omitempty"`
	ToolSnapshot      *ToolSchemaSnapshot          `json:"tool_snapshot,omitempty"`
	Skills            []string                     `json:"skills,omitempty"`
	SubAgents         []string                     `json:"sub_agents,omitempty"`
	MCPServers        []string                     `json:"mcp_servers,omitempty"`
	SkillResolutions  []skill.Resolution           `json:"skill_resolutions,omitempty"`
	MCPSnapshots      []mcp.CapabilitySnapshot     `json:"mcp_snapshots,omitempty"`
	HTTPToolSnapshots []HTTPToolSnapshot           `json:"http_tool_snapshots,omitempty"`
	ContextFragments  []contextpkg.ContextFragment `json:"-"`
	ToolDefinitions   []ModelToolDefinition        `json:"tool_definitions,omitempty"`
}

// HTTPToolSnapshot is Runtime's neutral, frozen projection of one tenant-authored
// HTTP tool definition (internal/httptooldef). It carries both the model-facing
// input schema and the execution spec (method/base_url/header_env names — never
// secrets), plus a DefinitionHash used to reject definitions that drifted since
// the snapshot was taken. Mirrors the role of mcp.CapabilitySnapshot for HTTP.
type HTTPToolSnapshot struct {
	Name           string            `json:"name"`
	Description    string            `json:"description,omitempty"`
	InputSchema    json.RawMessage   `json:"input_schema,omitempty"`
	Method         string            `json:"method,omitempty"`
	BaseURL        string            `json:"base_url,omitempty"`
	ResponseMode   string            `json:"response_mode,omitempty"`
	Write          bool              `json:"write,omitempty"`
	HeaderEnv      map[string]string `json:"header_env,omitempty"`
	TimeoutMS      int               `json:"timeout_ms,omitempty"`
	RiskLevel      string            `json:"risk_level,omitempty"`
	DefinitionHash string            `json:"definition_hash,omitempty"`
}

// ToolSchemaSnapshot is Runtime's neutral projection of one tenant-scoped,
// immutable Tool Registry snapshot. Runtime does not depend on Tool Gateway's
// domain types, but preserves these facts for integrity checks and replay.
type ToolSchemaSnapshot struct {
	SnapshotID        string   `json:"snapshot_id"`
	ToolRefs          []string `json:"tool_refs"`
	SchemaArtifactRef string   `json:"schema_artifact_ref,omitempty"`
	CapabilityHash    string   `json:"capability_hash"`
	PolicyHash        string   `json:"policy_hash"`
}

type ModelContextPackage struct {
	SchemaVersion      string                         `json:"schema_version"`
	PackageID          string                         `json:"package_id"`
	ContextHash        string                         `json:"context_hash"`
	CreatedAt          time.Time                      `json:"created_at"`
	Run                ModelContextRun                `json:"run"`
	Instructions       ModelContextInstructions       `json:"instructions"`
	Messages           ModelContextMessages           `json:"messages"`
	State              ModelContextState              `json:"state"`
	Capabilities       ModelContextCapabilities       `json:"capabilities"`
	RuntimeConstraints ModelContextRuntimeConstraints `json:"runtime_constraints"`
	Security           ModelContextSecurity           `json:"security"`
	Observability      ModelContextObservability      `json:"observability"`
}

type ModelContextRun struct {
	SessionID          string `json:"session_id"`
	RunID              string `json:"run_id"`
	AgentBindingID     string `json:"agent_binding_id,omitempty"`
	AgentID            string `json:"agent_id"`
	AgentVersion       string `json:"agent_version"`
	ConfigSnapshotRef  string `json:"config_snapshot_ref,omitempty"`
	ConfigHash         string `json:"config_hash,omitempty"`
	ContextSnapshotRef string `json:"context_snapshot_ref,omitempty"`
	Runtime            string `json:"runtime"`
	RuntimeMode        string `json:"runtime_mode,omitempty"`
}

type ModelContextInstructions struct {
	SystemPromptRef   string            `json:"system_prompt_ref,omitempty"`
	DynamicInjections map[string]string `json:"dynamic_injections,omitempty"`
}

type ModelContextMessages struct {
	ConversationWindow []ModelContextMessage     `json:"conversation_window,omitempty"`
	TokenCount         int                       `json:"token_count"`
	TrimmedCount       int                       `json:"trimmed_count,omitempty"`
	CacheBreak         int                       `json:"cache_break,omitempty"`
	CompactionRecords  []ContextCompactionRecord `json:"compaction_records,omitempty"`
}

type ModelContextMessage struct {
	ID                 string                 `json:"id,omitempty"`
	Sequence           int64                  `json:"sequence,omitempty"`
	Role               string                 `json:"role"`
	Content            string                 `json:"content"`
	ContentParts       []ModelContentPart     `json:"content_parts,omitempty"`
	Name               string                 `json:"name,omitempty"`
	ToolCalls          []contextpkg.ToolCall  `json:"tool_calls,omitempty"`
	ToolResult         *contextpkg.ToolResult `json:"tool_result,omitempty"`
	ToolCallID         string                 `json:"tool_call_id,omitempty"`
	ToolName           string                 `json:"tool_name,omitempty"`
	ReasoningContent   string                 `json:"reasoning_content,omitempty"`
	ReasoningSignature string                 `json:"reasoning_signature,omitempty"`
}

type ModelContextState struct {
	ScopedDataRef string `json:"scoped_data_ref,omitempty"`
}

type ModelContextCapabilities struct {
	Tools             []string                 `json:"tools,omitempty"`
	ToolSnapshot      *ToolSchemaSnapshot      `json:"tool_snapshot,omitempty"`
	ToolDefinitions   []ModelToolDefinition    `json:"tool_definitions,omitempty"`
	Skills            []string                 `json:"skills,omitempty"`
	SubAgents         []string                 `json:"sub_agents,omitempty"`
	MCPServers        []string                 `json:"mcp_servers,omitempty"`
	SkillSnapshots    []skill.Snapshot         `json:"skill_snapshots,omitempty"`
	MCPSnapshots      []mcp.CapabilitySnapshot `json:"mcp_snapshots,omitempty"`
	HTTPToolSnapshots []HTTPToolSnapshot       `json:"http_tool_snapshots,omitempty"`
	EstimatedTokens   int                      `json:"estimated_tokens,omitempty"`
}

type ModelContextRuntimeConstraints struct {
	ModelOptions      ModelCallOptions        `json:"model_options,omitempty"`
	TokenBudget       ModelTokenBudget        `json:"token_budget"`
	RuntimeCapability RuntimeCapabilities     `json:"runtime_capability"`
	CompactionPolicy  ContextCompactionPolicy `json:"compaction_policy"`
}

type ModelTokenBudget struct {
	// MaxInputTokens is the usable input budget after output reservation,
	// provider overhead and safety margin have already been deducted.
	MaxInputTokens int `json:"max_input_tokens"`
	// ReservedOutputTokens is capacity reserved for generation, not part of the
	// input budget above.
	ReservedOutputTokens int `json:"reserved_output_tokens"`
}

type ModelContextSecurity struct {
	TenantID string `json:"tenant_id,omitempty"`
	UserID   string `json:"user_id,omitempty"`
}

type ModelContextObservability struct {
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id,omitempty"`
}

type DefaultRuntimeContextAssembler struct {
	Builder               contextpkg.Builder
	ConversationCompactor *contextpkg.RollingConversationCompactor
	SystemPrompts         SystemPromptResolver
	// AllowLegacySystemPrompt 只允许本地 demo 显式开启；线上必须使用冻结 Prompt snapshot。
	AllowLegacySystemPrompt bool
	ContextSnapshots        ContextSnapshotProvider
	CapabilitySnapshots     CapabilitySnapshotProvider
	TokenBudgets            TokenBudgetAllocator
	Guardrail               PackageGuardrail
	IDs                     observability.IDGenerator
	Clock                   func() time.Time
	TokenBudget             contextpkg.ContextBudget
	RequireGovernedInputs   bool
}

func NewDefaultRuntimeContextAssembler(builder contextpkg.Builder) *DefaultRuntimeContextAssembler {
	return &DefaultRuntimeContextAssembler{
		Builder:               builder,
		ConversationCompactor: contextpkg.NewRollingConversationCompactor(nil),
		IDs:                   observability.NewULIDGenerator(""),
		Clock:                 time.Now,
		TokenBudget: contextpkg.ContextBudget{
			Limit:          16000,
			ResponseBuffer: 2048,
		},
	}
}

// NewProductionRuntimeContextAssembler creates the fail-closed production
// composition. The default constructor remains a development/test convenience.
func NewProductionRuntimeContextAssembler(
	builder contextpkg.Builder,
	snapshots ContextSnapshotProvider,
	capabilities CapabilitySnapshotProvider,
	budgets TokenBudgetAllocator,
	guardrail PackageGuardrail,
) (*DefaultRuntimeContextAssembler, error) {
	if snapshots == nil || capabilities == nil || budgets == nil || guardrail == nil {
		return nil, ErrProductionContextDependency
	}
	assembler := NewDefaultRuntimeContextAssembler(builder)
	assembler.ContextSnapshots = snapshots
	assembler.CapabilitySnapshots = capabilities
	assembler.TokenBudgets = budgets
	assembler.Guardrail = guardrail
	assembler.RequireGovernedInputs = true
	return assembler, nil
}

func (a *DefaultRuntimeContextAssembler) ValidateProduction() error {
	if a == nil || !a.RequireGovernedInputs || a.SystemPrompts == nil || a.AllowLegacySystemPrompt ||
		a.ContextSnapshots == nil || a.CapabilitySnapshots == nil || a.TokenBudgets == nil || a.Guardrail == nil {
		return ErrProductionContextDependency
	}
	return nil
}

func (a *DefaultRuntimeContextAssembler) Build(ctx context.Context, req RuntimeContextAssemblyRequest) (ModelContextPackage, error) {
	if err := ctx.Err(); err != nil {
		return ModelContextPackage{}, err
	}
	if a.RequireGovernedInputs && (a.SystemPrompts == nil || a.AllowLegacySystemPrompt ||
		a.ContextSnapshots == nil || a.CapabilitySnapshots == nil || a.TokenBudgets == nil || a.Guardrail == nil) {
		return ModelContextPackage{}, ErrProductionContextDependency
	}
	if a.RequireGovernedInputs && (req.Run.AgentBindingID == "" || req.Run.ConfigSnapshotRef == "" || req.Run.ConfigHash == "") {
		return ModelContextPackage{}, ErrProductionRunBinding
	}
	resolvedPrompt, err := a.resolveSystemPrompt(ctx, req.Run.Definition)
	if err != nil {
		return ModelContextPackage{}, err
	}
	builder := a.Builder
	if builder == nil {
		builder = defaultContextBuilder(req.Run.Definition)
	}
	ids := a.IDs
	if ids == nil {
		ids = observability.NewULIDGenerator("")
	}
	clock := a.Clock
	if clock == nil {
		clock = time.Now
	}
	budget := a.TokenBudget
	if budget.Limit <= 0 {
		budget.Limit = 16000
	}
	if budget.ResponseBuffer <= 0 {
		budget.ResponseBuffer = 2048
	}

	snapshot := ContextSnapshot{Ref: req.Run.ContextSnapshotRef}
	if a.ContextSnapshots != nil {
		var err error
		snapshot, err = a.ContextSnapshots.Materialize(ctx, req)
		if err != nil {
			return ModelContextPackage{}, err
		}
	}
	if a.RequireGovernedInputs && (snapshot.Ref == "" || snapshot.ContentHash == "") {
		return ModelContextPackage{}, ErrProductionContextSnapshot
	}
	capabilities := CapabilitySnapshot{
		Tools:      append([]string(nil), req.Run.Definition.ToolRefs...),
		Skills:     capabilityValues(req.Run.Definition.Metadata, "skill_refs"),
		SubAgents:  append([]string(nil), req.Run.Definition.SubAgentRefs...),
		MCPServers: capabilityValues(req.Run.Definition.Metadata, "mcp_servers"),
	}
	if a.CapabilitySnapshots != nil {
		var err error
		capabilities, err = a.CapabilitySnapshots.Resolve(ctx, req)
		if err != nil {
			return ModelContextPackage{}, err
		}
	}
	// Platform child Runs are execution leaves. Even a stale or mistakenly
	// authored child snapshot must not expose task/sub-agent delegation to the
	// model. Agent Gateway retains a second depth check as defense in depth.
	if req.Run.ParentRunID != "" {
		capabilities.SubAgents = nil
	}
	if a.RequireGovernedInputs {
		if err := validateGovernedMCPSnapshots(capabilities); err != nil {
			return ModelContextPackage{}, err
		}
	}
	if len(capabilities.Tools) > 0 {
		if capabilities.ToolSnapshot != nil {
			if err := validateGovernedToolDefinitions(*capabilities.ToolSnapshot, capabilities.Tools, capabilities.ToolDefinitions); err != nil {
				return ModelContextPackage{}, err
			}
		} else if a.RequireGovernedInputs {
			return ModelContextPackage{}, ErrProductionToolSnapshot
		}
	} else if a.RequireGovernedInputs && (capabilities.ToolSnapshot != nil || len(capabilities.ToolDefinitions) > 0) {
		return ModelContextPackage{}, ErrProductionToolSnapshot
	}
	definitions, err := compileCapabilityToolDefinitions(capabilities)
	if err != nil {
		return ModelContextPackage{}, err
	}
	resolvedRuntime := req.Run.Definition.Runtime
	if req.Handle.Runtime != "" && req.Handle.Runtime != RuntimeTypeAuto {
		resolvedRuntime.Type = req.Handle.Runtime
		resolvedRuntime.Preferred = ""
		resolvedRuntime.Candidates = nil
	}
	for _, definition := range definitions {
		if IsRuntimeReservedToolName(resolvedRuntime, definition.Name) {
			return ModelContextPackage{}, fmt.Errorf("%w: tool name %q is reserved by runtime mode %q", ErrCapabilityToolSchemaInvalid, definition.Name, req.Run.Definition.Runtime.Mode)
		}
	}
	capabilities.ToolDefinitions = definitions
	modelBudget := ModelTokenBudget{
		MaxInputTokens:       budget.Limit,
		ReservedOutputTokens: budget.ResponseBuffer,
	}
	if a.TokenBudgets != nil {
		var err error
		modelBudget, err = a.TokenBudgets.Allocate(ctx, req, snapshot, capabilities)
		if err != nil {
			return ModelContextPackage{}, err
		}
	}
	if modelBudget.MaxInputTokens <= 0 {
		modelBudget.MaxInputTokens = budget.Limit
	}
	if modelBudget.ReservedOutputTokens <= 0 {
		modelBudget.ReservedOutputTokens = budget.ResponseBuffer
	}
	compactionPolicy, err := NormalizeContextCompactionPolicy(req.Run.Definition.ContextCompaction)
	if err != nil {
		return ModelContextPackage{}, err
	}
	budget.Limit = modelBudget.MaxInputTokens
	budget.ResponseBuffer = modelBudget.ReservedOutputTokens
	capabilityTokens := estimateCapabilityTokens(capabilities)
	budget.Limit -= capabilityTokens
	if budget.Limit <= 0 {
		return ModelContextPackage{}, fmt.Errorf("%w: capabilities use %d tokens, input limit is %d", contextpkg.ErrBudgetExceeded, capabilityTokens, modelBudget.MaxInputTokens)
	}

	buildBudget := budget
	promptTokens := contextpkg.EstimateCounter{}.Count(resolvedPrompt.Content)
	if promptTokens >= buildBudget.Limit {
		return ModelContextPackage{}, fmt.Errorf("%w: prompt=%d limit=%d", ErrSystemPromptBudgetExceeded, promptTokens, buildBudget.Limit)
	}
	buildBudget.Limit -= promptTokens

	// context.history=none 时跳过会话历史装配：本轮模型输入只含当前 Run
	// 材料，历史由宿主经 before_model_hook 自供（ADR-019）。事实落账不受
	// 影响（Session/Message/Event 已在存储层写入）。
	dropHistory := req.Run.Definition.ContextHistory == "none"
	contextMessages, err := mergeContextMessages(snapshot, req.Run.SessionID, req.Run.Input, a.RequireGovernedInputs, dropHistory)
	if err != nil {
		return ModelContextPackage{}, err
	}
	currentInputFacts := contextMessages
	allFragments := append(append([]contextpkg.ContextFragment(nil), snapshot.Fragments...), attachmentContextFragments(snapshot.Attachments)...)
	allFragments = append(allFragments, capabilities.ContextFragments...)
	// ExternalFragments 来自入口层 ContextContributor 扩展（经 dispatcher 透传），
	// 与其他 fragment 一起参与后续压缩与预算治理。
	allFragments = append(allFragments, req.Run.ExternalFragments...)
	contextMessages, assemblyCompaction, err := a.compactInitialConversation(ctx, contextMessages, allFragments, buildBudget.Limit, compactionPolicy)
	if err != nil {
		return ModelContextPackage{}, err
	}
	buildResult, err := builder.Build(ctx, contextpkg.BuildRequest{
		SessionID:   req.Run.SessionID,
		Generation:  1,
		Messages:    contextMessages,
		Fragments:   allFragments,
		TokenBudget: buildBudget,
	})
	if err != nil {
		return ModelContextPackage{}, err
	}
	if record, ok := builderCompactionRecord(buildResult, compactionPolicy); ok {
		assemblyCompaction = append(assemblyCompaction, record)
	}
	if err := validateCurrentInputsPreserved(currentInputFacts, buildResult); err != nil {
		return ModelContextPackage{}, err
	}
	if err := ctx.Err(); err != nil {
		return ModelContextPackage{}, err
	}
	prependSystemPrompt(buildResult, resolvedPrompt.Content, promptTokens)
	if buildResult.TokenCount+capabilityTokens > modelBudget.MaxInputTokens {
		return ModelContextPackage{}, fmt.Errorf("%w: context=%d capabilities=%d limit=%d", contextpkg.ErrBudgetExceeded, buildResult.TokenCount, capabilityTokens, modelBudget.MaxInputTokens)
	}
	systemPromptRef := req.Run.Definition.PromptRef
	if resolvedPrompt.SnapshotRef != "" {
		systemPromptRef = resolvedPrompt.SnapshotRef
	}

	pkg := ModelContextPackage{
		SchemaVersion: ModelContextPackageSchemaVersion,
		PackageID:     ids.NewRequestID(),
		CreatedAt:     clock(),
		Run: ModelContextRun{
			SessionID:          req.Run.SessionID,
			RunID:              req.Run.RunID,
			AgentBindingID:     req.Run.AgentBindingID,
			AgentID:            req.Run.Definition.AgentID,
			AgentVersion:       req.Run.Definition.Version,
			ConfigSnapshotRef:  req.Run.ConfigSnapshotRef,
			ConfigHash:         req.Run.ConfigHash,
			ContextSnapshotRef: snapshot.Ref,
			Runtime:            string(resolvedRuntime.Type),
			RuntimeMode:        string(req.Run.Definition.Runtime.Mode),
		},
		Instructions: ModelContextInstructions{
			SystemPromptRef: systemPromptRef,
			DynamicInjections: map[string]string{
				"current_time": clock().Format(time.RFC3339Nano),
			},
		},
		Messages: ModelContextMessages{
			ConversationWindow: toModelMessages(buildResult.Messages),
			TokenCount:         buildResult.TokenCount,
			TrimmedCount:       len(buildResult.Trimmed) + compactedMessageCount(assemblyCompaction),
			CacheBreak:         buildResult.CacheBreak,
			CompactionRecords:  assemblyCompaction,
		},
		State: ModelContextState{
			ScopedDataRef: req.Run.Metadata["scoped_data_ref"],
		},
		Capabilities: ModelContextCapabilities{
			Tools:             append([]string(nil), capabilities.Tools...),
			ToolSnapshot:      cloneToolSchemaSnapshot(capabilities.ToolSnapshot),
			Skills:            append([]string(nil), capabilities.Skills...),
			SubAgents:         append([]string(nil), capabilities.SubAgents...),
			MCPServers:        append([]string(nil), capabilities.MCPServers...),
			SkillSnapshots:    resolvedSkillSnapshots(capabilities.SkillResolutions),
			MCPSnapshots:      append([]mcp.CapabilitySnapshot(nil), capabilities.MCPSnapshots...),
			HTTPToolSnapshots: append([]HTTPToolSnapshot(nil), capabilities.HTTPToolSnapshots...),
			ToolDefinitions:   cloneModelToolDefinitions(capabilities.ToolDefinitions),
			EstimatedTokens:   capabilityTokens,
		},
		RuntimeConstraints: ModelContextRuntimeConstraints{
			ModelOptions:      cloneModelCallOptions(req.Run.Definition.ModelOptions),
			RuntimeCapability: req.Capabilities,
			TokenBudget:       modelBudget,
			CompactionPolicy:  compactionPolicy,
		},
		Security: ModelContextSecurity{
			TenantID: req.Run.TenantID,
			UserID:   req.Run.UserID,
		},
		Observability: ModelContextObservability{
			TraceID: req.Trace.TraceID,
			SpanID:  req.Trace.SpanID,
		},
	}
	pkg.ContextHash = modelContextHash(pkg)
	if a.Guardrail != nil {
		if err := a.Guardrail.Validate(ctx, pkg); err != nil {
			return ModelContextPackage{}, err
		}
	}
	return pkg, nil
}

func compactedMessageCount(records []ContextCompactionRecord) int {
	total := 0
	for _, record := range records {
		total += record.RemovedMessages
	}
	return total
}

func attachmentContextFragments(attachments []contextpkg.AttachmentFact) []contextpkg.ContextFragment {
	if len(attachments) == 0 {
		return nil
	}
	data, _ := json.Marshal(struct {
		SchemaVersion string                      `json:"schema_version"`
		Attachments   []contextpkg.AttachmentFact `json:"attachments"`
		ReadPolicy    string                      `json:"read_policy"`
	}{
		SchemaVersion: "harness.context_attachments.v1",
		Attachments:   attachments,
		ReadPolicy:    "Attachment facts are frozen in this snapshot. Compatible image/file artifacts are also projected as multimodal parts on the current user message; artifact_ref remains the exact lookup address for tools.",
	})
	content := string(data)
	return []contextpkg.ContextFragment{{
		Slot:      contextpkg.SlotWorkspace,
		Stability: contextpkg.StabilityDynamic,
		Priority:  95,
		Pinned:    true,
		TokenCost: contextpkg.EstimateCounter{}.Count(content),
		Source:    "attachments",
		Role:      contextpkg.RoleSystem,
		Content:   content,
	}}
}

func builderCompactionRecord(result *contextpkg.BuildResult, policy ContextCompactionPolicy) (ContextCompactionRecord, bool) {
	if result == nil || len(result.Trimmed) == 0 {
		return ContextCompactionRecord{}, false
	}
	before := result.TokenCount
	strategies := make([]string, 0, len(result.Trimmed))
	seen := make(map[string]struct{})
	for _, trim := range result.Trimmed {
		before += trim.Fragment.TokenCost
		if _, ok := seen[trim.Reason]; !ok {
			seen[trim.Reason] = struct{}{}
			strategies = append(strategies, "context.builder."+trim.Reason)
		}
	}
	return newContextCompactionRecord(ContextCompactionRecord{
		Phase: ContextCompactionPhaseAssembly, PolicyHash: policy.PolicyHash,
		BeforeTokens: before, AfterTokens: result.TokenCount,
		RemovedItems: len(result.Trimmed), AppliedStrategies: strategies,
	}), true
}

func (a *DefaultRuntimeContextAssembler) compactInitialConversation(
	ctx context.Context,
	messages []*contextpkg.Message,
	fragments []contextpkg.ContextFragment,
	limit int,
	policy ContextCompactionPolicy,
) ([]*contextpkg.Message, []ContextCompactionRecord, error) {
	if policy.SemanticSummary == SemanticSummaryDisabled || limit <= 0 || len(messages) == 0 {
		return messages, nil, nil
	}
	counter := contextpkg.EstimateCounter{}
	messageTokens := 0
	values := make([]contextpkg.Message, 0, len(messages))
	for _, message := range messages {
		if message == nil {
			continue
		}
		messageTokens += counter.CountMessage(message)
		values = append(values, *message)
	}
	fragmentTokens := 0
	for _, fragment := range fragments {
		fragmentTokens += fragment.TokenCost
	}
	if float64(messageTokens+fragmentTokens) < float64(limit)*policy.CompactTriggerRatio {
		return messages, nil, nil
	}
	compactor := a.ConversationCompactor
	if compactor == nil {
		compactor = contextpkg.NewRollingConversationCompactor(nil)
	}
	target := int(float64(limit)*policy.TargetRatio) - fragmentTokens
	if target <= 0 {
		if policy.SemanticSummary == SemanticSummaryRequired {
			return nil, nil, ErrContextSummaryRequired
		}
		return messages, nil, nil
	}
	result, err := compactor.Compact(ctx, contextpkg.ConversationCompactionRequest{
		Messages: values, TargetTokens: target,
		PreserveRecentTurns: policy.PreserveRecentTurns,
		MaxSummaryTokens:    policy.MaxSummaryTokens,
	})
	if err != nil {
		return nil, nil, err
	}
	for _, index := range result.RemovedIndices {
		if index < 0 || index >= len(values) {
			return nil, nil, fmt.Errorf("%w: compactor returned invalid message index=%d", ErrCurrentInputMissing, index)
		}
		// CurrentInput 是本轮请求事实，任何 compactor 都只能压缩历史，不能删除或摘要化本轮输入。
		if values[index].CurrentInput {
			return nil, nil, fmt.Errorf("%w: compactor removed message_id=%s", ErrCurrentInputMissing, values[index].ID)
		}
	}
	if len(result.RemovedIndices) == 0 {
		if policy.SemanticSummary == SemanticSummaryRequired && result.BeforeTokens > target {
			return nil, nil, ErrContextSummaryRequired
		}
		return messages, nil, nil
	}
	if policy.SemanticSummary == SemanticSummaryRequired && result.Summary == nil {
		return nil, nil, ErrContextSummaryRequired
	}
	removed := make(map[int]struct{}, len(result.RemovedIndices))
	insertAt := result.RemovedIndices[0]
	for _, index := range result.RemovedIndices {
		removed[index] = struct{}{}
	}
	compacted := make([]*contextpkg.Message, 0, len(values)-len(removed)+1)
	inserted := false
	for index := range values {
		if !inserted && index >= insertAt && result.Summary != nil {
			summary := *result.Summary
			summary.SessionID = values[index].SessionID
			summary.Extra = map[string]any{"summary_ref": result.SummaryRef, "source": "context_compaction"}
			compacted = append(compacted, &summary)
			inserted = true
		}
		if _, drop := removed[index]; !drop {
			message := values[index]
			compacted = append(compacted, &message)
		}
	}
	summaryRefs := []string(nil)
	if result.SummaryRef != "" {
		summaryRefs = []string{result.SummaryRef}
	}
	record := newContextCompactionRecord(ContextCompactionRecord{
		Phase: ContextCompactionPhaseAssembly, PolicyHash: policy.PolicyHash,
		BeforeTokens: result.BeforeTokens, AfterTokens: result.AfterTokens,
		RemovedItems:      len(result.RemovedIndices),
		RemovedMessages:   len(result.RemovedIndices),
		AppliedStrategies: []string{"context.rolling_summary.v1"}, SummaryRefs: summaryRefs,
	})
	return compacted, []ContextCompactionRecord{record}, nil
}

func estimateCapabilityTokens(capabilities CapabilitySnapshot) int {
	counter := contextpkg.EstimateCounter{}
	total := 0
	defined := make(map[string]struct{}, len(capabilities.ToolDefinitions))
	for _, tool := range capabilities.ToolDefinitions {
		defined[tool.Name] = struct{}{}
		total += counter.Count(tool.Name) + counter.Count(tool.Description) + counter.Count(string(tool.Schema)) + 16
	}
	// Until Tool Registry supplies exact frozen schemas, reserve a conservative
	// minimum for every unresolved registry ref instead of treating it as free.
	for _, ref := range uniqueStrings(capabilities.Tools) {
		name := strings.SplitN(ref, "@", 2)[0]
		if _, ok := defined[name]; !ok {
			total += 256
		}
	}
	for _, resolution := range capabilities.SkillResolutions {
		total += 16
		for _, dependency := range resolution.Dependencies {
			total += 8 + counter.Count(dependency.Snapshot.SkillID)
		}
	}
	total += len(uniqueStrings(capabilities.SubAgents)) * 64
	return total
}

func compileCapabilityToolDefinitions(capabilities CapabilitySnapshot) ([]ModelToolDefinition, error) {
	definitions := cloneModelToolDefinitions(capabilities.ToolDefinitions)
	for _, snapshot := range capabilities.MCPSnapshots {
		for _, tool := range snapshot.Tools {
			definitions = append(definitions, ModelToolDefinition{
				Name: tool.Name, Description: tool.Description, Schema: append(json.RawMessage(nil), tool.InputSchema...),
			})
		}
	}
	for _, tool := range capabilities.HTTPToolSnapshots {
		definitions = append(definitions, ModelToolDefinition{
			Name: tool.Name, Description: tool.Description, Schema: append(json.RawMessage(nil), tool.InputSchema...),
		})
	}
	seen := make(map[string]struct{}, len(definitions))
	for _, definition := range definitions {
		if definition.Name == "" || len(definition.Schema) == 0 || !json.Valid(definition.Schema) {
			return nil, fmt.Errorf("%w: tool=%q", ErrCapabilityToolSchemaInvalid, definition.Name)
		}
		if _, exists := seen[definition.Name]; exists {
			return nil, fmt.Errorf("%w: duplicate tool name=%q", ErrCapabilityToolSchemaInvalid, definition.Name)
		}
		seen[definition.Name] = struct{}{}
	}
	return definitions, nil
}

func resolvedSkillSnapshots(resolutions []skill.Resolution) []skill.Snapshot {
	var snapshots []skill.Snapshot
	for _, resolution := range resolutions {
		snapshots = append(snapshots, resolution.Root)
		for _, dependency := range resolution.Dependencies {
			snapshots = append(snapshots, dependency.Snapshot)
		}
	}
	return snapshots
}

func defaultContextBuilder(def AgentDefinition) contextpkg.Builder {
	registry := contextpkg.NewSourceRegistry(
		// Agent 身份可以来自冻结 Definition；system policy 必须由受治理的
		// Prompt/Capability snapshot 注入，不能信任调用方可写的 Metadata。
		contextpkg.NewSystemPromptSource("", def.AgentID, ""),
		contextpkg.NewConversationSource(0),
	)
	return contextpkg.NewDefaultBuilder(registry, contextpkg.EstimateCounter{}, contextpkg.BuilderConfig{
		MinPreserve: contextpkg.MinPreserveConfig{StablePrefix: true, CurrentInput: true, RecentTurns: 1},
	})
}

func (a *DefaultRuntimeContextAssembler) resolveSystemPrompt(ctx context.Context, def AgentDefinition) (SystemPromptSnapshot, error) {
	version := def.Metadata["prompt_version"]
	promptHash := def.Metadata["prompt_hash"]
	snapshotRef := def.Metadata["prompt_snapshot_ref"]
	contentHash := def.Metadata["prompt_content_hash"]
	if a.RequireGovernedInputs && (def.PromptRef == "" || version == "" || promptHash == "" || snapshotRef == "" || contentHash == "") {
		return SystemPromptSnapshot{}, ErrSystemPromptSnapshotMismatch
	}
	if promptHash == "" && snapshotRef == "" && contentHash == "" {
		legacyContent := def.Metadata["system_prompt"]
		if def.PromptRef == "" && legacyContent == "" {
			return SystemPromptSnapshot{}, nil
		}
		if !a.AllowLegacySystemPrompt {
			return SystemPromptSnapshot{}, ErrSystemPromptResolverMissing
		}
		return SystemPromptSnapshot{Ref: def.PromptRef, Version: version, Content: legacyContent}, nil
	}
	if def.PromptRef == "" || version == "" || promptHash == "" || snapshotRef == "" || contentHash == "" {
		return SystemPromptSnapshot{}, ErrSystemPromptSnapshotMismatch
	}
	if a.SystemPrompts == nil {
		return SystemPromptSnapshot{}, ErrSystemPromptResolverMissing
	}
	snapshot, err := a.SystemPrompts.ResolveSystemPrompt(ctx, SystemPromptResolveRequest{
		Ref: def.PromptRef, Version: version,
		ExpectedPromptHash: promptHash, ExpectedSnapshotRef: snapshotRef, ExpectedContentHash: contentHash,
	})
	if err != nil {
		return SystemPromptSnapshot{}, err
	}
	if snapshot.Ref != def.PromptRef || snapshot.Version != version ||
		snapshot.PromptHash != promptHash || snapshot.SnapshotRef != snapshotRef ||
		snapshot.ContentHash != contentHash || snapshot.Content == "" || snapshot.ContentHash != systemPromptContentHash(snapshot.Content) {
		return SystemPromptSnapshot{}, ErrSystemPromptSnapshotMismatch
	}
	return snapshot, nil
}

func systemPromptContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func prependSystemPrompt(result *contextpkg.BuildResult, content string, tokenCount int) {
	if result == nil || content == "" {
		return
	}
	message := &contextpkg.Message{ID: "system_prompt", Role: contextpkg.RoleSystem, Content: content}
	result.Messages = append([]*contextpkg.Message{message}, result.Messages...)
	result.Fragments = append([]contextpkg.ContextFragment{{
		Slot: contextpkg.SlotSystemPrompt, Stability: contextpkg.StabilityStable,
		Priority: 100, Pinned: true, TokenCost: tokenCount, Generation: 1,
		Source: string(contextpkg.SourceSystemPrompt), Role: contextpkg.RoleSystem, Content: content,
	}}, result.Fragments...)
	result.TokenCount += tokenCount
	result.CacheBreak++
}

// mergeContextMessages 按消息身份把 Run.Input 标记为本轮输入；正文相同不代表同一条消息。
// 生产链路只接受已落 Ledger 且进入冻结 snapshot 的消息，本地 demo 才允许临时追加。
func mergeContextMessages(snapshot ContextSnapshot, sessionID string, input []Message, requireFrozen bool, dropHistory bool) ([]*contextpkg.Message, error) {
	var history []*contextpkg.Message
	if !dropHistory {
		history = snapshotMessages(snapshot)
	}
	byID := make(map[string]*contextpkg.Message, len(history)+len(input))
	usedIDs := make(map[string]struct{}, len(history)+len(input))
	for _, message := range history {
		if message == nil || message.ID == "" {
			continue
		}
		if _, exists := byID[message.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate snapshot message_id=%s", contextpkg.ErrMessageConflict, message.ID)
		}
		byID[message.ID] = message
		usedIDs[message.ID] = struct{}{}
	}
	if len(input) == 0 {
		return history, nil
	}
	lastSequence := snapshot.LastSequence
	if int64(len(history)) > lastSequence {
		lastSequence = int64(len(history))
	}
	for _, message := range history {
		if message != nil && message.Sequence > lastSequence {
			lastSequence = message.Sequence
		}
	}
	for i, message := range input {
		if message.Role != string(contextpkg.RoleUser) {
			return nil, ErrRunInputRoleInvalid
		}
		if message.ID == "" {
			if requireFrozen {
				return nil, fmt.Errorf("%w: current input[%d]", contextpkg.ErrMessageIDMissing, i)
			}
			message.ID = nextInputMessageID(i+1, usedIDs)
		}
		if existing, exists := byID[message.ID]; exists {
			if !sameInputFact(existing, message) {
				return nil, fmt.Errorf("%w: message_id=%s", contextpkg.ErrMessageConflict, message.ID)
			}
			existing.CurrentInput = true
			continue
		}
		if requireFrozen {
			return nil, fmt.Errorf("%w: message_id=%s", ErrCurrentInputNotFrozen, message.ID)
		}
		usedIDs[message.ID] = struct{}{}
		lastSequence++
		current := &contextpkg.Message{
			ID:             message.ID,
			SessionID:      sessionID,
			Sequence:       lastSequence,
			IdempotencyKey: message.IdempotencyKey,
			CurrentInput:   true,
			Role:           contextpkg.RoleUser,
			Content:        message.Content,
			Parts:          append([]contextpkg.ContentPart(nil), message.Parts...),
			Timestamp:      time.Now(),
		}
		history = append(history, current)
		byID[current.ID] = current
	}
	return history, nil
}

func sameInputFact(snapshot *contextpkg.Message, input Message) bool {
	return snapshot != nil && snapshot.ID == input.ID && string(snapshot.Role) == input.Role &&
		snapshot.Content == input.Content && snapshot.IdempotencyKey == input.IdempotencyKey &&
		samePartsFact(snapshot.Parts, input.Parts)
}

// samePartsFact 比较多 Part 事实是否一致（序列化指纹对比）；两侧均空
// 视为一致。
func samePartsFact(left, right []contextpkg.ContentPart) bool {
	if len(left) == 0 && len(right) == 0 {
		return true
	}
	leftData, _ := json.Marshal(left)
	rightData, _ := json.Marshal(right)
	return string(leftData) == string(rightData)
}

func nextInputMessageID(inputIndex int, used map[string]struct{}) string {
	base := fmt.Sprintf("input_%d", inputIndex)
	candidate := base
	for suffix := 2; ; suffix++ {
		if _, exists := used[candidate]; !exists {
			used[candidate] = struct{}{}
			return candidate
		}
		candidate = fmt.Sprintf("%s_%d", base, suffix)
	}
}

func validateCurrentInputsPreserved(input []*contextpkg.Message, result *contextpkg.BuildResult) error {
	expected := make(map[string]*contextpkg.Message)
	for _, message := range input {
		if message != nil && message.CurrentInput {
			expected[message.ID] = message
		}
	}
	if len(expected) == 0 {
		return nil
	}
	if result == nil {
		return ErrCurrentInputMissing
	}
	for _, message := range result.Messages {
		if message == nil || !message.CurrentInput {
			continue
		}
		original, ok := expected[message.ID]
		if !ok {
			continue
		}
		if message.Role != original.Role || message.Content != original.Content || message.IdempotencyKey != original.IdempotencyKey {
			return fmt.Errorf("%w: message_id=%s content changed", ErrCurrentInputMissing, message.ID)
		}
		delete(expected, message.ID)
	}
	for id := range expected {
		return fmt.Errorf("%w: message_id=%s", ErrCurrentInputMissing, id)
	}
	return nil
}

func toModelMessages(messages []*contextpkg.Message) []ModelContextMessage {
	out := make([]ModelContextMessage, 0, len(messages))
	for _, msg := range messages {
		out = append(out, ModelContextMessage{
			ID:           msg.ID,
			Sequence:     msg.Sequence,
			Role:         string(msg.Role),
			Content:      msg.Content,
			ContentParts: modelPartsFromContext(msg.Parts),
			ToolCalls:    append([]contextpkg.ToolCall(nil), msg.ToolCalls...),
			ToolResult:   cloneToolResult(msg.ToolResult),
		})
	}
	return out
}

// modelPartsFromContext 把 Ledger 的结构化 Parts 投影为模型可见的
// ModelContentPart：text/json 按文本透传；image_ref 投影为 image_url
// （artifact:// URL 由 ModelInvoker 在送 provider 前解析为 data URI）；
// file_ref 投影为 file part（同样由 ModelInvoker 解析为内联字节，G-C）；
// 通用 artifact_ref 语义不明，保持占位文本（附件事实另经 attachments
// fragment 告知模型）。
func modelPartsFromContext(parts []contextpkg.ContentPart) []ModelContentPart {
	if len(parts) == 0 {
		return nil
	}
	out := make([]ModelContentPart, 0, len(parts))
	for _, part := range parts {
		switch part.Kind {
		case "text":
			out = append(out, ModelContentPart{Type: "text", Text: part.Text})
		case "json":
			out = append(out, ModelContentPart{Type: "text", Text: string(part.JSON)})
		case "image_ref":
			out = append(out, ModelContentPart{
				Type: "image_url", URL: artifactRefURL(part.ArtifactRef),
				MIMEType: part.MIME, Name: part.Filename,
			})
		case "file_ref":
			// 非图片文件引用直达模型请求（G-C）：artifact:// URL 由
			// ModelInvoker 在送 provider 前读取为内联 base64。
			out = append(out, ModelContentPart{
				Type: "file", URL: artifactRefURL(part.ArtifactRef),
				MIMEType: part.MIME, Name: part.Filename,
			})
		case "artifact_ref":
			out = append(out, ModelContentPart{Type: "text", Text: "[artifact:" + part.ArtifactRef + "]"})
		case "inline_binary":
			// 内联小二进制直达模型请求（ADR-013）：图片 MIME 编码为
			// image_url data URI；非图片 MIME 投影为 file part（G-C，供
			// 支持文件理解的模型原生读取）。Base64Data 由 provider
			// adapter 组装为最终 data URI。
			if strings.HasPrefix(part.MIME, "image/") && len(part.Inline) > 0 {
				out = append(out, ModelContentPart{
					Type: "image_url", MIMEType: part.MIME, Name: part.Filename,
					Base64Data: base64.StdEncoding.EncodeToString(part.Inline),
				})
			} else if len(part.Inline) > 0 {
				out = append(out, ModelContentPart{
					Type: "file", MIMEType: part.MIME, Name: part.Filename,
					Base64Data: base64.StdEncoding.EncodeToString(part.Inline),
				})
			} else {
				out = append(out, ModelContentPart{Type: "text", Text: fmt.Sprintf("[inline:%d bytes]", len(part.Inline))})
			}
		}
	}
	return out
}

// artifactRefURL 把裸 artifact id 归一为 artifact:// 定位符；已带 scheme
// （artifact:// / http(s):// / data:）的引用原样透传。
func artifactRefURL(ref string) string {
	if ref == "" || strings.Contains(ref, "://") || strings.HasPrefix(ref, "data:") {
		return ref
	}
	return "artifact://" + ref
}

func snapshotMessages(snapshot ContextSnapshot) []*contextpkg.Message {
	out := make([]*contextpkg.Message, 0, len(snapshot.Messages))
	for i := range snapshot.Messages {
		message := snapshot.Messages[i]
		message.CurrentInput = false
		out = append(out, &message)
	}
	return out
}

func cloneToolResult(result *contextpkg.ToolResult) *contextpkg.ToolResult {
	if result == nil {
		return nil
	}
	cloned := *result
	return &cloned
}

func capabilityValues(metadata map[string]string, key string) []string {
	if metadata == nil || metadata[key] == "" {
		return nil
	}
	var values []string
	if err := json.Unmarshal([]byte(metadata[key]), &values); err == nil {
		return values
	}
	return []string{metadata[key]}
}

func modelContextHash(pkg ModelContextPackage) string {
	copyPkg := pkg
	copyPkg.PackageID = ""
	copyPkg.ContextHash = ""
	copyPkg.CreatedAt = time.Time{}
	data, _ := json.Marshal(copyPkg)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ComputeModelContextHash returns the canonical integrity hash of a single
// model-call package. Package identity and creation time are intentionally not
// part of the semantic hash.
func ComputeModelContextHash(pkg ModelContextPackage) string {
	return modelContextHash(pkg)
}

func cloneToolSchemaSnapshot(input *ToolSchemaSnapshot) *ToolSchemaSnapshot {
	if input == nil {
		return nil
	}
	output := *input
	output.ToolRefs = append([]string(nil), input.ToolRefs...)
	return &output
}

func validateModelContextIntegrity(pkg ModelContextPackage) error {
	if pkg.ContextHash == "" || pkg.ContextHash != modelContextHash(pkg) {
		return fmt.Errorf("%w: context hash mismatch", ErrModelContextIntegrityInvalid)
	}
	return nil
}
