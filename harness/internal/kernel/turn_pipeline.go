package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
)

// turn_pipeline.go 是入口层扩展的预回合管线（R2a）：IdentityResolver →
// RunInitializer → ContextContributor → InputNormalizer 四个阶段在 OpenTurn
// 之前按 §7.1 固定顺序执行。它是这些阶段的**唯一**执行器实现——HTTP 入口
// （internal/protocol/server）与 SDK 入口（harness.Engine.Start）都调用
// PrepareTurn，从而穿过同一条治理链；harness Facade 不再自带副本。
//
// 执行语义（与原 harness 侧实现逐条对齐）：
//   - 每个条目独立 timeout（entry.Timeout > 0 时）与 panic 恢复；
//   - 四个阶段全部 fail-closed：任一条目失败则整轮拒绝开启；
//   - 条目顺序由 ExtensionCatalog 的 Kind → Order → ID 确定性排序保证；
//   - 类型不匹配的注册（Kind 与实现接口不符）fail-closed 拒绝；
//   - step/trace 名与错误信息使用派生 ID 格式 extension:<kind>:<entryID>。

// ErrTurnPipelineInvalid 标记预回合管线的注册 / 产出无效类错误（类型不匹配、
// panic、空输出等）。入口层应把它映射为"无效请求"语义（SDK 侧为
// harness.ErrInvalidRequest）。扩展实现自身返回的业务错误不会包上本哨兵，
// 而是保留原错误链透传（调用方 errors.Is 仍可命中业务哨兵）。
var ErrTurnPipelineInvalid = errors.New("kernel: turn pipeline invalid")

// SDKContractVersion 是 SDK 公共契约线（与 harness.Version 同值；由
// harness 侧 parity 测试守护一致性）。预回合管线在扩展 Context.SDKVersion
// 中携带它，使 HTTP 与 SDK 两个入口的扩展看到相同环境。
const SDKContractVersion = "harness.sdk.v0"

// CanonicalSchemaVersions 返回本 kernel 遵循的 canonical schema 线列表。
// harness.BuildReport 与预回合管线的扩展 Context 共用本列表（唯一事实源）。
func CanonicalSchemaVersions() []string {
	return []string{
		"harness.agent_event.v1",
		"harness.agent_binding.v1",
		"harness.control_request.v1",
		"harness.artifact_meta.v1",
		"harness.sse.v1",
		"harness.composition.v1",
	}
}

// TurnEnvironment 提供构建扩展 Context 所需的静态环境信息，在 Build 时冻结。
type TurnEnvironment struct {
	Environment    string
	SDKVersion     string
	SchemaVersions []string
}

// TurnIdentity 是预回合管线操作的身份作用域（IdentityResolver 的入参与
// 折叠输出）。
type TurnIdentity struct {
	TenantID     string
	UserID       string
	SessionID    string
	AgentID      string
	AgentVersion string
}

// PrepareTurnRequest 是 PrepareTurn 的入参。Input / ScopedData 使用
// harness/extension 的中立 DTO，避免入口层各自再造一套形状。
type PrepareTurnRequest struct {
	// Identity 是调用方提供的初始身份；resolver 的非空输出字段会覆盖它。
	Identity TurnIdentity
	// Metadata 承载 IdentityResolver 可能参考的调用方信号。
	Metadata map[string]string
	// Input 是本轮用户输入。
	Input extension.NormalizedMessage
	// ScopedData 是调用方预置的 run 级 scoped data 种子（可为 nil）。
	ScopedData map[string]extension.ScopedDataEntry
}

// PrepareTurnResult 是四个阶段执行完毕后的冻结产出。
type PrepareTurnResult struct {
	// Identity 是解析后的最终身份。
	Identity TurnIdentity
	// Input 是（可能被 RunInitializer 追加 ref、被 InputNormalizer 改写的）
	// 最终输入。
	Input extension.NormalizedMessage
	// ScopedData 是 RunInitializer 合并后的 run 级 scoped data。
	ScopedData map[string]extension.ScopedDataEntry
	// Fragments 是 ContextContributor 产出的全部业务 context 片段，调度层
	// 负责把它们透传进 ModelContext 装配。
	Fragments []extension.ContextFragment
	// NormalizerID 是实际执行的 InputNormalizer 条目 ID 链（按执行序逗号连接）；
	// 未注册时为空。
	NormalizerID string
}

// TurnPipeline 从冻结的 ExtensionCatalog 构建，无内部状态，可并发使用。
type TurnPipeline struct {
	catalog *ExtensionCatalog
	env     TurnEnvironment
}

// NewTurnPipeline 构建预回合管线。catalog 为 nil 时返回一个恒直通的管线。
func NewTurnPipeline(catalog *ExtensionCatalog, env TurnEnvironment) *TurnPipeline {
	return &TurnPipeline{catalog: catalog, env: env}
}

// HasStages 报告管线是否注册了任一预回合阶段；false 时入口层可零成本跳过。
func (p *TurnPipeline) HasStages() bool {
	if p == nil || p.catalog == nil {
		return false
	}
	return len(p.catalog.ByKind(ExtIdentityResolver)) > 0 ||
		len(p.catalog.ByKind(ExtRunInitializer)) > 0 ||
		len(p.catalog.ByKind(ExtContextContributor)) > 0 ||
		len(p.catalog.ByKind(ExtInputNormalizer)) > 0
}

// PrepareTurn 依次执行四个预回合阶段并返回冻结产出。任何阶段失败都
// fail-closed：调用方不得继续 OpenTurn。
func (p *TurnPipeline) PrepareTurn(ctx context.Context, req PrepareTurnRequest) (PrepareTurnResult, error) {
	result := PrepareTurnResult{
		Identity:   req.Identity,
		Input:      req.Input,
		ScopedData: cloneScopedEntries(req.ScopedData),
	}
	if p == nil || p.catalog == nil {
		return result, nil
	}
	if err := p.applyIdentityResolvers(ctx, &result, req.Identity, req.Metadata); err != nil {
		return PrepareTurnResult{}, err
	}
	if err := p.applyRunInitializers(ctx, &result); err != nil {
		return PrepareTurnResult{}, err
	}
	if err := p.applyContextContributors(ctx, &result); err != nil {
		return PrepareTurnResult{}, err
	}
	if err := p.applyInputNormalizer(ctx, &result); err != nil {
		return PrepareTurnResult{}, err
	}
	return result, nil
}

// derivedExtensionID 生成派生 ID：extension:<kind>:<entryID>（命名对齐约定）。
func derivedExtensionID(kind ExtensionKind, entryID string) string {
	return "extension:" + string(kind) + ":" + entryID
}

// extensionContext 构建每次扩展调用共享的只读上下文。
func (p *TurnPipeline) extensionContext(identity TurnIdentity, deadline time.Time) extension.Context {
	return extension.Context{
		InvocationKind: extension.InvocationRoot,
		TenantID:       identity.TenantID,
		UserID:         identity.UserID,
		SessionID:      identity.SessionID,
		AgentID:        identity.AgentID,
		AgentVersion:   identity.AgentVersion,
		Environment:    p.env.Environment,
		SDKVersion:     p.env.SDKVersion,
		SchemaVersions: p.env.SchemaVersions,
		Deadline:       deadline,
		Attempt:        1,
	}
}

// stageContext 为单个条目派生（可选超时的）子 context。
func stageContext(ctx context.Context, entry ExtensionEntry) (context.Context, context.CancelFunc) {
	if entry.Timeout > 0 {
		return context.WithTimeout(ctx, entry.Timeout)
	}
	return ctx, func() {}
}

func deadlineOf(ctx context.Context) time.Time {
	if ctx == nil {
		return time.Time{}
	}
	dl, ok := ctx.Deadline()
	if !ok {
		return time.Time{}
	}
	return dl
}

// applyIdentityResolvers 按目录顺序执行全部 IdentityResolver 并把非空输出字段
// 折叠进身份（信任链：后注册的可以覆盖先注册的）。失败 fail-closed。
func (p *TurnPipeline) applyIdentityResolvers(ctx context.Context, result *PrepareTurnResult, base TurnIdentity, metadata map[string]string) error {
	entries := p.catalog.ByKind(ExtIdentityResolver)
	for _, entry := range entries {
		resolver, ok := entry.Implementation.(extension.IdentityResolver)
		if !ok {
			return fmt.Errorf("%w: extension %s does not implement extension.IdentityResolver", ErrTurnPipelineInvalid, entry.ID)
		}
		reqCtx, cancel := stageContext(ctx, entry)
		resolved, err := invokeIdentityResolver(reqCtx, entry, resolver, extension.IdentityRequest{
			Ctx: p.extensionContext(result.Identity, deadlineOf(reqCtx)),
			// BusinessXxx 恒为调用方原始身份（不随折叠推进），与原 SDK 行为一致。
			BusinessTenantID:  base.TenantID,
			BusinessUserID:    base.UserID,
			BusinessSessionID: base.SessionID,
			Metadata:          metadata,
		})
		cancel()
		if err != nil {
			return err
		}
		if resolved.TenantID != "" {
			result.Identity.TenantID = resolved.TenantID
		}
		if resolved.UserID != "" {
			result.Identity.UserID = resolved.UserID
		}
		if resolved.SessionID != "" {
			result.Identity.SessionID = resolved.SessionID
		}
	}
	return nil
}

func invokeIdentityResolver(ctx context.Context, entry ExtensionEntry, resolver extension.IdentityResolver, req extension.IdentityRequest) (resolved extension.ResolvedIdentity, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %s panicked: %v", ErrTurnPipelineInvalid, derivedExtensionID(ExtIdentityResolver, entry.ID), r)
		}
	}()
	return resolver.Resolve(ctx, req)
}

// applyRunInitializers 按目录顺序执行全部 RunInitializer，把 ScopedData 合并进
// 结果、把追加的 ArtifactRefs 转为输入 parts。失败 fail-closed。
func (p *TurnPipeline) applyRunInitializers(ctx context.Context, result *PrepareTurnResult) error {
	entries := p.catalog.ByKind(ExtRunInitializer)
	if len(entries) == 0 {
		return nil
	}
	if result.ScopedData == nil {
		result.ScopedData = make(map[string]extension.ScopedDataEntry)
	}
	inputRefs := collectNormalizedArtifactRefs(result.Input)
	inputPreview := normalizedMessagePreview(result.Input)
	for _, entry := range entries {
		initializer, ok := entry.Implementation.(extension.RunInitializer)
		if !ok {
			return fmt.Errorf("%w: extension %s does not implement extension.RunInitializer", ErrTurnPipelineInvalid, entry.ID)
		}
		reqCtx, cancel := stageContext(ctx, entry)
		output, err := invokeRunInitializer(reqCtx, entry, initializer, extension.RunInitRequest{
			Ctx:               p.extensionContext(result.Identity, deadlineOf(reqCtx)),
			InputPreview:      inputPreview,
			InputArtifactRefs: inputRefs,
			SeedScopedData:    cloneScopedEntries(result.ScopedData),
		})
		cancel()
		if err != nil {
			return err
		}
		for key, value := range output.ScopedData {
			if key == "" {
				return fmt.Errorf("%w: %s produced empty ScopedData key", ErrTurnPipelineInvalid, derivedExtensionID(ExtRunInitializer, entry.ID))
			}
			result.ScopedData[key] = value
		}
		for _, ref := range output.ArtifactRefs {
			result.Input.Parts = append(result.Input.Parts, extension.NormalizedPart{
				Kind: "artifact_ref",
				MIME: ref.MIME,
				Hash: ref.Hash,
				Ref:  ref,
			})
		}
	}
	return nil
}

func invokeRunInitializer(ctx context.Context, entry ExtensionEntry, init extension.RunInitializer, req extension.RunInitRequest) (output extension.RunInitOutput, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %s panicked: %v", ErrTurnPipelineInvalid, derivedExtensionID(ExtRunInitializer, entry.ID), r)
		}
	}()
	return init.Initialize(ctx, req)
}

// applyContextContributors 按目录顺序执行全部 ContextContributor，拼接 fragment
// 列表并把数量 + 摘要写入输入 metadata（审计可见）。失败 fail-closed。
func (p *TurnPipeline) applyContextContributors(ctx context.Context, result *PrepareTurnResult) error {
	entries := p.catalog.ByKind(ExtContextContributor)
	if len(entries) == 0 {
		return nil
	}
	scoped := cloneScopedEntries(result.ScopedData)
	for _, entry := range entries {
		contributor, ok := entry.Implementation.(extension.ContextContributor)
		if !ok {
			return fmt.Errorf("%w: extension %s does not implement extension.ContextContributor", ErrTurnPipelineInvalid, entry.ID)
		}
		reqCtx, cancel := stageContext(ctx, entry)
		fragments, err := invokeContextContributor(reqCtx, entry, contributor, extension.ContribRequest{
			Ctx:        p.extensionContext(result.Identity, deadlineOf(reqCtx)),
			ScopedData: scoped,
		})
		cancel()
		if err != nil {
			return err
		}
		result.Fragments = append(result.Fragments, fragments...)
	}
	if len(result.Fragments) == 0 {
		return nil
	}
	ensureInputMetadata(&result.Input)
	result.Input.Metadata["harness.context_contributions"] = fmt.Sprintf("%d", len(result.Fragments))
	digest, _ := json.Marshal(summarizeTurnFragments(result.Fragments))
	result.Input.Metadata["harness.context_fragment_digest"] = string(digest)
	return nil
}

func invokeContextContributor(ctx context.Context, entry ExtensionEntry, contributor extension.ContextContributor, req extension.ContribRequest) (fragments []extension.ContextFragment, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %s panicked: %v", ErrTurnPipelineInvalid, derivedExtensionID(ExtContextContributor, entry.ID), r)
		}
	}()
	return contributor.Contribute(ctx, req)
}

// applyInputNormalizer 按注册序链式执行全部 InputNormalizer 并用最终输出替换
// 输入：前一个的 Message 作为下一个的 RawInput，SourceRefs 累积去重。任一环节
// 失败或产出空 parts 均 fail-closed。Engine 级语义：不与 agent_id 绑定。
func (p *TurnPipeline) applyInputNormalizer(ctx context.Context, result *PrepareTurnResult) error {
	entries := p.catalog.ByKind(ExtInputNormalizer)
	if len(entries) == 0 {
		return nil
	}
	var (
		appliedIDs []string
		sourceRefs []extension.SourceRef
		seenRefs   = make(map[string]struct{})
	)
	for _, entry := range entries {
		normalizer, ok := entry.Implementation.(extension.InputNormalizer)
		if !ok {
			return fmt.Errorf("%w: extension %s does not implement extension.InputNormalizer", ErrTurnPipelineInvalid, entry.ID)
		}
		reqCtx, cancel := stageContext(ctx, entry)
		out, err := invokeInputNormalizer(reqCtx, entry, normalizer, extension.NormalizeRequest{
			Ctx:                  p.extensionContext(result.Identity, deadlineOf(reqCtx)),
			RawInput:             result.Input,
			ScopedData:           cloneScopedEntries(result.ScopedData),
			ArtifactRefs:         collectNormalizedArtifactRefs(result.Input),
			ContextContributions: result.Fragments,
		})
		cancel()
		if err != nil {
			return err
		}
		if len(out.Message.Parts) == 0 {
			return fmt.Errorf("%w: InputNormalizer %s returned empty message parts", ErrTurnPipelineInvalid, entry.ID)
		}
		result.Input = out.Message
		appliedIDs = append(appliedIDs, entry.ID)
		for _, ref := range out.SourceRefs {
			key := ref.Kind + "\x00" + ref.Key + "\x00" + ref.Hash
			if _, ok := seenRefs[key]; ok {
				continue
			}
			seenRefs[key] = struct{}{}
			sourceRefs = append(sourceRefs, ref)
		}
	}
	result.NormalizerID = strings.Join(appliedIDs, ",")
	// 记录归一化元数据与累积来源引用，供审计与下游确定性校验。
	ensureInputMetadata(&result.Input)
	result.Input.Metadata["harness.input_normalizer"] = result.NormalizerID
	if len(sourceRefs) > 0 {
		digest, _ := json.Marshal(sourceRefs)
		result.Input.Metadata["harness.input_source_refs"] = string(digest)
	}
	return nil
}

func invokeInputNormalizer(ctx context.Context, entry ExtensionEntry, normalizer extension.InputNormalizer, req extension.NormalizeRequest) (out extension.NormalizedInput, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %s panicked: %v", ErrTurnPipelineInvalid, derivedExtensionID(ExtInputNormalizer, entry.ID), r)
		}
	}()
	return normalizer.Normalize(ctx, req)
}

// ---- 辅助函数 --------------------------------------------------------------

func cloneScopedEntries(in map[string]extension.ScopedDataEntry) map[string]extension.ScopedDataEntry {
	if in == nil {
		return nil
	}
	out := make(map[string]extension.ScopedDataEntry, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func ensureInputMetadata(msg *extension.NormalizedMessage) {
	if msg.Metadata == nil {
		msg.Metadata = make(map[string]string)
	}
}

// collectNormalizedArtifactRefs 枚举输入中每一个带 Ref.ID 的 part。
func collectNormalizedArtifactRefs(msg extension.NormalizedMessage) []extension.ArtifactRef {
	if len(msg.Parts) == 0 {
		return nil
	}
	out := make([]extension.ArtifactRef, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		if part.Ref.ID == "" {
			continue
		}
		out = append(out, part.Ref)
	}
	return out
}

// normalizedMessagePreview 把有序 parts 摊平为一段 UTF-8 预览（与 SDK 侧
// renderPreview 语义一致），供 RunInitializer 的 InputPreview 与存储层的
// UserContentPreview 使用。
func normalizedMessagePreview(msg extension.NormalizedMessage) string {
	var b strings.Builder
	for _, part := range msg.Parts {
		switch part.Kind {
		case "text":
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(part.Text)
		case "json":
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.Write(part.JSON)
		case "image_ref", "file_ref", "artifact_ref":
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString("[artifact:")
			b.WriteString(part.Ref.ID)
			b.WriteByte(']')
		case "inline_binary":
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(fmt.Sprintf("[inline:%d bytes]", len(part.Inline)))
		}
	}
	return b.String()
}

// NormalizedMessagePreview 是 normalizedMessagePreview 的导出形式，供入口层
// 在 PrepareTurn 之后为 OpenTurn 生成 UserContentPreview。
func NormalizedMessagePreview(msg extension.NormalizedMessage) string {
	return normalizedMessagePreview(msg)
}

// summarizeTurnFragments 生成 fragment 摘要（kind/source/hash），用于跨 Run
// 一致性比对而不暴露正文。
func summarizeTurnFragments(fragments []extension.ContextFragment) []map[string]string {
	out := make([]map[string]string, 0, len(fragments))
	for _, f := range fragments {
		out = append(out, map[string]string{
			"kind":   f.Kind,
			"source": string(f.Source),
			"hash":   f.Hash,
		})
	}
	return out
}

// ContextFragmentsForTurn 把扩展贡献的 fragment 投影为 contextpkg 形状，供
// 入口层填入 storage.OpenTurnRequest.ContextFragments 后经 dispatcher 透传进
// ModelContext 装配（取代旧的 metadata digest 占位）。投影约定：
//   - Slot 固定为 workspace（业务上下文），Stability 为 dynamic；
//   - Source 采用派生 ID 前缀 extension:context_contributor 加 fragment Kind，
//     便于从 snapshot 反查贡献来源；
//   - 正文优先取 Text；仅有 Ref 时降级为 artifact 引用标记，完整内容由
//     下游按 Ref 解引用；
//   - TokenCost 优先用调用方提示的 TokenBudget，否则按内容估算。
func ContextFragmentsForTurn(fragments []extension.ContextFragment) []contextpkg.ContextFragment {
	if len(fragments) == 0 {
		return nil
	}
	out := make([]contextpkg.ContextFragment, 0, len(fragments))
	for _, f := range fragments {
		content := f.Text
		if content == "" && f.Ref.ID != "" {
			content = "[artifact:" + f.Ref.ID + "]"
		}
		if content == "" {
			continue
		}
		tokenCost := f.TokenBudget
		if tokenCost <= 0 {
			tokenCost = contextpkg.EstimateCounter{}.Count(content)
		}
		out = append(out, contextpkg.ContextFragment{
			Slot:      contextpkg.SlotWorkspace,
			Stability: contextpkg.StabilityDynamic,
			Priority:  f.Priority,
			TokenCost: tokenCost,
			Source:    "extension:" + string(ExtContextContributor) + ":" + f.Kind,
			Role:      contextpkg.RoleSystem,
			Content:   content,
		})
	}
	return out
}
