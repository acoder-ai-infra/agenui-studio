package agentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
)

const (
	ContextCompactionPolicySchemaVersion = "harness.context_compaction_policy.v1"
	PreserveManifestSchemaVersion        = "harness.preserve_manifest.v1"
	ContextCompactionRecordSchemaVersion = "harness.context_compaction_record.v1"
)

var (
	ErrContextCompactionPolicyInvalid  = errors.New("context compaction policy invalid")
	ErrContextCompactionPolicyDrift    = errors.New("context compaction policy differs from frozen package")
	ErrRuntimePreModelCompactorMissing = errors.New("runtime pre-model compactor missing")
	ErrPreserveManifestInvalid         = errors.New("preserve manifest invalid")
	ErrContextSummaryRequired          = errors.New("context summary required but could not be produced")
)

func validateContextCompactionPolicyBinding(definition AgentDefinition, pkg ModelContextPackage) error {
	definitionPolicy, err := NormalizeContextCompactionPolicy(definition.ContextCompaction)
	if err != nil {
		return err
	}
	packagePolicy, err := NormalizeContextCompactionPolicy(pkg.RuntimeConstraints.CompactionPolicy)
	if err != nil {
		return err
	}
	if definitionPolicy.PolicyHash != packagePolicy.PolicyHash {
		return fmt.Errorf("%w: definition=%s package=%s", ErrContextCompactionPolicyDrift, definitionPolicy.PolicyHash, packagePolicy.PolicyHash)
	}
	return nil
}

type SemanticSummaryMode string

type ContextCompactionPhase string

const (
	SemanticSummaryAuto     SemanticSummaryMode = "auto"
	SemanticSummaryRequired SemanticSummaryMode = "required"
	SemanticSummaryDisabled SemanticSummaryMode = "disabled"

	ContextCompactionPhaseAssembly ContextCompactionPhase = "runtime_assembly"
	ContextCompactionPhasePreModel ContextCompactionPhase = "pre_model"
	ContextCompactionPhaseFinal    ContextCompactionPhase = "final_governor"
)

// ContextCompactionRecord is bounded, inline provenance for one compaction
// decision. RecordRef identifies this immutable value; it is not an external
// Artifact Store reference. A persistence adapter may materialize the same
// record later without changing the runtime contract.
type ContextCompactionRecord struct {
	SchemaVersion     string                 `json:"schema_version"`
	RecordRef         string                 `json:"record_ref"`
	Phase             ContextCompactionPhase `json:"phase"`
	PolicyHash        string                 `json:"policy_hash"`
	BeforeTokens      int                    `json:"before_tokens"`
	AfterTokens       int                    `json:"after_tokens"`
	RemovedItems      int                    `json:"removed_items"`
	RemovedMessages   int                    `json:"removed_messages"`
	AppliedStrategies []string               `json:"applied_strategies,omitempty"`
	SummaryRefs       []string               `json:"summary_refs,omitempty"`
}

func newContextCompactionRecord(record ContextCompactionRecord) ContextCompactionRecord {
	record.SchemaVersion = ContextCompactionRecordSchemaVersion
	record.RecordRef = ""
	record.RecordRef = "context-trim-inline://" + stableJSONHash(record)
	return record
}

// ContextCompactionPolicy is the runtime-neutral, immutable policy consumed by
// every adapter. Runtime-specific resolvers compile it; they never choose it.
type ContextCompactionPolicy struct {
	SchemaVersion       string              `json:"schema_version" yaml:"schema_version"`
	PolicyID            string              `json:"policy_id" yaml:"policy_id"`
	Version             string              `json:"version" yaml:"version"`
	PolicyHash          string              `json:"policy_hash" yaml:"policy_hash"`
	SemanticSummary     SemanticSummaryMode `json:"semantic_summary" yaml:"semantic_summary"`
	SoftTriggerRatio    float64             `json:"soft_trigger_ratio" yaml:"soft_trigger_ratio"`
	CompactTriggerRatio float64             `json:"compact_trigger_ratio" yaml:"compact_trigger_ratio"`
	TargetRatio         float64             `json:"target_ratio" yaml:"target_ratio"`
	PreserveRecentTurns int                 `json:"preserve_recent_turns" yaml:"preserve_recent_turns"`
	MaxSummaryTokens    int                 `json:"max_summary_tokens" yaml:"max_summary_tokens"`
}

func DefaultContextCompactionPolicy() ContextCompactionPolicy {
	policy := ContextCompactionPolicy{
		SchemaVersion:       ContextCompactionPolicySchemaVersion,
		PolicyID:            "platform-default",
		Version:             "v1",
		SemanticSummary:     SemanticSummaryAuto,
		SoftTriggerRatio:    0.75,
		CompactTriggerRatio: 0.88,
		TargetRatio:         0.70,
		PreserveRecentTurns: 2,
		MaxSummaryTokens:    1024,
	}
	policy.PolicyHash = computeContextCompactionPolicyHash(policy)
	return policy
}

func NormalizeContextCompactionPolicy(policy ContextCompactionPolicy) (ContextCompactionPolicy, error) {
	if policy == (ContextCompactionPolicy{}) {
		return DefaultContextCompactionPolicy(), nil
	}
	if policy.SchemaVersion == "" {
		policy.SchemaVersion = ContextCompactionPolicySchemaVersion
	}
	if policy.PolicyID == "" {
		policy.PolicyID = "platform-default"
	}
	if policy.Version == "" {
		policy.Version = "v1"
	}
	if policy.SemanticSummary == "" {
		policy.SemanticSummary = SemanticSummaryAuto
	}
	if policy.SoftTriggerRatio == 0 {
		policy.SoftTriggerRatio = 0.75
	}
	if policy.CompactTriggerRatio == 0 {
		policy.CompactTriggerRatio = 0.88
	}
	if policy.TargetRatio == 0 {
		policy.TargetRatio = 0.70
	}
	if policy.PreserveRecentTurns == 0 {
		policy.PreserveRecentTurns = 2
	}
	if policy.MaxSummaryTokens == 0 {
		policy.MaxSummaryTokens = 1024
	}
	if policy.SchemaVersion != ContextCompactionPolicySchemaVersion {
		return ContextCompactionPolicy{}, fmt.Errorf("%w: schema_version=%s", ErrContextCompactionPolicyInvalid, policy.SchemaVersion)
	}
	switch policy.SemanticSummary {
	case SemanticSummaryAuto, SemanticSummaryRequired, SemanticSummaryDisabled:
	default:
		return ContextCompactionPolicy{}, fmt.Errorf("%w: semantic_summary=%s", ErrContextCompactionPolicyInvalid, policy.SemanticSummary)
	}
	if policy.TargetRatio <= 0 || policy.SoftTriggerRatio <= 0 || policy.CompactTriggerRatio <= 0 ||
		policy.TargetRatio >= policy.SoftTriggerRatio || policy.SoftTriggerRatio >= policy.CompactTriggerRatio || policy.CompactTriggerRatio >= 1 {
		return ContextCompactionPolicy{}, fmt.Errorf("%w: require target < soft < compact < 1", ErrContextCompactionPolicyInvalid)
	}
	if policy.PreserveRecentTurns < 1 || policy.MaxSummaryTokens < 32 {
		return ContextCompactionPolicy{}, fmt.Errorf("%w: preserve_recent_turns must be positive and max_summary_tokens >= 32", ErrContextCompactionPolicyInvalid)
	}
	expected := computeContextCompactionPolicyHash(policy)
	if policy.PolicyHash != "" && policy.PolicyHash != expected {
		return ContextCompactionPolicy{}, fmt.Errorf("%w: policy hash mismatch", ErrContextCompactionPolicyInvalid)
	}
	policy.PolicyHash = expected
	return policy, nil
}

func computeContextCompactionPolicyHash(policy ContextCompactionPolicy) string {
	policy.PolicyHash = ""
	data, _ := json.Marshal(policy)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type PreservedMessage struct {
	Role        string `json:"role"`
	Fingerprint string `json:"fingerprint"`
	Count       int    `json:"count"`
}

// PreserveManifest freezes the semantics that no runtime-native middleware or
// generic compactor may silently remove from one actual model request.
type PreserveManifest struct {
	SchemaVersion       string             `json:"schema_version"`
	ManifestHash        string             `json:"manifest_hash"`
	RequiredMessages    []PreservedMessage `json:"required_messages,omitempty"`
	CurrentUserHash     string             `json:"current_user_hash,omitempty"`
	ToolPairIDs         []string           `json:"tool_pair_ids,omitempty"`
	RunContextHash      string             `json:"run_context_hash"`
	RuntimePolicyHash   string             `json:"runtime_policy_hash"`
	InstructionsHash    string             `json:"instructions_hash"`
	SecurityContextHash string             `json:"security_context_hash"`
}

func BuildPreserveManifest(req ModelInvokeRequest) (PreserveManifest, error) {
	req = materializeModelInvokeRequest(req)
	if err := validateModelToolPairing(req.Messages); err != nil {
		return PreserveManifest{}, fmt.Errorf("%w: %v", ErrPreserveManifestInvalid, err)
	}
	counts := make(map[string]PreservedMessage)
	currentUser := ""
	for _, message := range req.Messages {
		fingerprint := modelCallMessageFingerprint(message)
		if message.Role == "system" {
			key := message.Role + ":" + fingerprint
			entry := counts[key]
			entry.Role = message.Role
			entry.Fingerprint = fingerprint
			entry.Count++
			counts[key] = entry
		}
		if message.Role == "user" {
			currentUser = fingerprint
		}
	}
	required := make([]PreservedMessage, 0, len(counts))
	for _, entry := range counts {
		required = append(required, entry)
	}
	sort.Slice(required, func(i, j int) bool {
		if required[i].Role != required[j].Role {
			return required[i].Role < required[j].Role
		}
		return required[i].Fingerprint < required[j].Fingerprint
	})
	toolPairs := modelToolPairIDs(req.Messages)
	manifest := PreserveManifest{
		SchemaVersion:       PreserveManifestSchemaVersion,
		RequiredMessages:    required,
		CurrentUserHash:     currentUser,
		ToolPairIDs:         toolPairs,
		RunContextHash:      stableJSONHash(req.Package.Run),
		RuntimePolicyHash:   runtimeConstraintsHash(req.Package.RuntimeConstraints),
		InstructionsHash:    stableJSONHash(req.Package.Instructions),
		SecurityContextHash: stableJSONHash(req.Package.Security),
	}
	manifest.ManifestHash = computePreserveManifestHash(manifest)
	return manifest, nil
}

func ValidatePreserveManifest(manifest PreserveManifest, req ModelInvokeRequest) error {
	if manifest.SchemaVersion != PreserveManifestSchemaVersion || manifest.ManifestHash == "" || manifest.ManifestHash != computePreserveManifestHash(manifest) {
		return fmt.Errorf("%w: manifest integrity mismatch", ErrPreserveManifestInvalid)
	}
	req = materializeModelInvokeRequest(req)
	if stableJSONHash(req.Package.Instructions) != manifest.InstructionsHash {
		return fmt.Errorf("%w: instructions changed", ErrPreserveManifestInvalid)
	}
	if stableJSONHash(req.Package.Run) != manifest.RunContextHash {
		return fmt.Errorf("%w: run context changed", ErrPreserveManifestInvalid)
	}
	if runtimeConstraintsHash(req.Package.RuntimeConstraints) != manifest.RuntimePolicyHash {
		return fmt.Errorf("%w: runtime policy changed", ErrPreserveManifestInvalid)
	}
	if stableJSONHash(req.Package.Security) != manifest.SecurityContextHash {
		return fmt.Errorf("%w: security context changed", ErrPreserveManifestInvalid)
	}
	counts := make(map[string]int)
	latestUser := ""
	for _, message := range req.Messages {
		fingerprint := modelCallMessageFingerprint(message)
		counts[message.Role+":"+fingerprint]++
		if message.Role == "user" {
			latestUser = fingerprint
		}
	}
	for _, required := range manifest.RequiredMessages {
		if counts[required.Role+":"+required.Fingerprint] < required.Count {
			return fmt.Errorf("%w: required %s message changed or removed", ErrPreserveManifestInvalid, required.Role)
		}
	}
	if manifest.CurrentUserHash != "" && latestUser != manifest.CurrentUserHash {
		return fmt.Errorf("%w: current user input changed or removed", ErrPreserveManifestInvalid)
	}
	if err := validateModelToolPairing(req.Messages); err != nil {
		return fmt.Errorf("%w: %v", ErrPreserveManifestInvalid, err)
	}
	return nil
}

func runtimeConstraintsHash(constraints ModelContextRuntimeConstraints) string {
	if policy, err := NormalizeContextCompactionPolicy(constraints.CompactionPolicy); err == nil {
		constraints.CompactionPolicy = policy
	}
	return stableJSONHash(constraints)
}

func computePreserveManifestHash(manifest PreserveManifest) string {
	manifest.ManifestHash = ""
	return stableJSONHash(manifest)
}

func stableJSONHash(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func modelCallMessageFingerprint(message ModelCallMessage) string {
	return stableJSONHash(message)
}

func modelToolPairIDs(messages []ModelCallMessage) []string {
	set := make(map[string]struct{})
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			set[call.ToolCallID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

type RuntimePreModelCompactor interface {
	CompactBeforeModel(context.Context, RuntimePreModelCompactionRequest) (RuntimePreModelCompactionResult, error)
}

// ContextCompactorProvider is registered once at the process composition root.
// It selects an implementation for a frozen policy and runtime descriptor;
// individual agents never wire framework handlers themselves.
type ContextCompactorProvider interface {
	Resolve(context.Context, ContextCompactionPolicy, RuntimeDescriptor) (RuntimePreModelCompactor, error)
}

type ContextCompactorProviderFunc func(context.Context, ContextCompactionPolicy, RuntimeDescriptor) (RuntimePreModelCompactor, error)

func (f ContextCompactorProviderFunc) Resolve(ctx context.Context, policy ContextCompactionPolicy, runtime RuntimeDescriptor) (RuntimePreModelCompactor, error) {
	return f(ctx, policy, runtime)
}

type RuntimePreModelCompactorReadiness interface {
	ValidatePolicy(ContextCompactionPolicy) error
}

type RuntimePreModelCompactorFunc func(context.Context, RuntimePreModelCompactionRequest) (RuntimePreModelCompactionResult, error)

func (f RuntimePreModelCompactorFunc) CompactBeforeModel(ctx context.Context, req RuntimePreModelCompactionRequest) (RuntimePreModelCompactionResult, error) {
	return f(ctx, req)
}

type RuntimePreModelCompactionRequest struct {
	Request          ModelInvokeRequest      `json:"request"`
	Policy           ContextCompactionPolicy `json:"policy"`
	PreserveManifest PreserveManifest        `json:"preserve_manifest"`
}

type RuntimePreModelCompactionResult struct {
	Request           ModelInvokeRequest        `json:"request"`
	ObservedTokens    int                       `json:"observed_tokens,omitempty"`
	SoftTriggered     bool                      `json:"soft_triggered,omitempty"`
	AppliedStrategies []string                  `json:"applied_strategies,omitempty"`
	SummaryRefs       []string                  `json:"summary_refs,omitempty"`
	TrimRecordRef     string                    `json:"trim_record_ref,omitempty"`
	Records           []ContextCompactionRecord `json:"records,omitempty"`
}

type RuntimePreModelCompactionStrategy interface {
	Name() string
	CompactBeforeModel(context.Context, RuntimePreModelCompactionRequest) (RuntimePreModelCompactionResult, error)
}

// DefaultRuntimePreModelCompactor is the portable Stage-B pipeline used by
// Direct and runtimes without a native middleware system. Context owns the
// injected strategies; Harness owns policy, invariants and ordering.
type DefaultRuntimePreModelCompactor struct {
	Counter    ModelInputTokenCounter
	Strategies []RuntimePreModelCompactionStrategy
}

// DefaultContextCompactorProvider installs the Context-owned rolling
// conversation compactor for every managed Runtime. A custom summarizer may be
// supplied without changing Runtime adapters or Agent definitions.
type DefaultContextCompactorProvider struct {
	Counter   ModelInputTokenCounter
	Compactor *contextpkg.RollingConversationCompactor
}

// ContextCompactionServices is the process-level composition object for Stage
// A and Stage B. Sharing this object prevents the assembler and runtime
// adapters from silently using different summarizers or token counters.
type ContextCompactionServices struct {
	Conversation *contextpkg.RollingConversationCompactor
	Provider     *DefaultContextCompactorProvider
}

func NewContextCompactionServices(summarizer contextpkg.ConversationSummarizer) *ContextCompactionServices {
	conversation := contextpkg.NewRollingConversationCompactor(summarizer)
	return &ContextCompactionServices{
		Conversation: conversation,
		Provider:     &DefaultContextCompactorProvider{Compactor: conversation},
	}
}

func (s *ContextCompactionServices) ConfigureAssembler(assembler *DefaultRuntimeContextAssembler) {
	if s == nil || assembler == nil {
		return
	}
	assembler.ConversationCompactor = s.Conversation
}

func (s *ContextCompactionServices) ConfigureEnvironment(environment RuntimeEnvironment) RuntimeEnvironment {
	if s != nil {
		environment.ContextCompactors = s.Provider
	}
	return environment
}

func NewDefaultContextCompactorProvider(summarizer contextpkg.ConversationSummarizer) *DefaultContextCompactorProvider {
	return NewContextCompactionServices(summarizer).Provider
}

func (p *DefaultContextCompactorProvider) Resolve(_ context.Context, policy ContextCompactionPolicy, _ RuntimeDescriptor) (RuntimePreModelCompactor, error) {
	if _, err := NormalizeContextCompactionPolicy(policy); err != nil {
		return nil, err
	}
	compactor := p.Compactor
	if compactor == nil {
		compactor = contextpkg.NewRollingConversationCompactor(nil)
	}
	return &DefaultRuntimePreModelCompactor{
		Counter: p.Counter,
		Strategies: []RuntimePreModelCompactionStrategy{
			&ContextConversationCompactionStrategy{Compactor: compactor, Counter: p.Counter},
		},
	}, nil
}

// ContextConversationCompactionStrategy adapts the Context Engine's neutral
// compactor to the actual model request while retaining all non-message
// capability snapshots and provider-neutral message fields.
type ContextConversationCompactionStrategy struct {
	Compactor *contextpkg.RollingConversationCompactor
	Counter   ModelInputTokenCounter
}

func (*ContextConversationCompactionStrategy) Name() string { return "context.rolling_summary.v1" }

func (s *ContextConversationCompactionStrategy) CompactBeforeModel(ctx context.Context, req RuntimePreModelCompactionRequest) (RuntimePreModelCompactionResult, error) {
	request := materializeModelInvokeRequest(req.Request)
	counter := s.Counter
	if counter == nil {
		counter = EstimateModelInputTokenCounter{}
	}
	toolTokens, err := counter.Count(ctx, nil, request.Tools)
	if err != nil {
		return RuntimePreModelCompactionResult{}, fmt.Errorf("count tool capability input: %w", err)
	}
	target := int(float64(request.Package.RuntimeConstraints.TokenBudget.MaxInputTokens)*req.Policy.TargetRatio) - toolTokens
	if target <= 0 {
		if req.Policy.SemanticSummary == SemanticSummaryRequired {
			return RuntimePreModelCompactionResult{}, ErrContextSummaryRequired
		}
		return RuntimePreModelCompactionResult{Request: request}, nil
	}
	compactor := s.Compactor
	if compactor == nil {
		compactor = contextpkg.NewRollingConversationCompactor(nil)
	}
	neutral := make([]contextpkg.Message, 0, len(request.Messages))
	for _, message := range request.Messages {
		neutral = append(neutral, contextMessageFromModelCall(message))
	}
	compacted, err := compactor.Compact(ctx, contextpkg.ConversationCompactionRequest{
		Messages: neutral, TargetTokens: target,
		PreserveRecentTurns: req.Policy.PreserveRecentTurns,
		MaxSummaryTokens:    req.Policy.MaxSummaryTokens,
	})
	if err != nil {
		return RuntimePreModelCompactionResult{}, err
	}
	if len(compacted.RemovedIndices) == 0 {
		if req.Policy.SemanticSummary == SemanticSummaryRequired && compacted.BeforeTokens > target {
			return RuntimePreModelCompactionResult{}, ErrContextSummaryRequired
		}
		return RuntimePreModelCompactionResult{Request: request}, nil
	}
	if req.Policy.SemanticSummary == SemanticSummaryRequired && compacted.Summary == nil {
		return RuntimePreModelCompactionResult{}, ErrContextSummaryRequired
	}
	removed := make(map[int]struct{}, len(compacted.RemovedIndices))
	insertAt := compacted.RemovedIndices[0]
	for _, index := range compacted.RemovedIndices {
		removed[index] = struct{}{}
	}
	messages := make([]ModelCallMessage, 0, len(request.Messages)-len(removed)+1)
	inserted := false
	for index, message := range request.Messages {
		if !inserted && index >= insertAt && compacted.Summary != nil {
			messages = append(messages, ModelCallMessage{Role: string(compacted.Summary.Role), Content: compacted.Summary.Content})
			inserted = true
		}
		if _, drop := removed[index]; !drop {
			messages = append(messages, message)
		}
	}
	request.Messages = messages
	result := RuntimePreModelCompactionResult{Request: request}
	if compacted.SummaryRef != "" {
		result.SummaryRefs = []string{compacted.SummaryRef}
	}
	record := newContextCompactionRecord(ContextCompactionRecord{
		Phase: ContextCompactionPhasePreModel, PolicyHash: req.Policy.PolicyHash,
		BeforeTokens: compacted.BeforeTokens, AfterTokens: compacted.AfterTokens,
		RemovedItems:      len(compacted.RemovedIndices),
		RemovedMessages:   len(compacted.RemovedIndices),
		AppliedStrategies: []string{s.Name()}, SummaryRefs: append([]string(nil), result.SummaryRefs...),
	})
	result.TrimRecordRef = record.RecordRef
	result.Records = []ContextCompactionRecord{record}
	return result, nil
}

func contextMessageFromModelCall(message ModelCallMessage) contextpkg.Message {
	converted := contextpkg.Message{Role: contextpkg.RoleType(message.Role), Content: message.Content}
	for _, call := range message.ToolCalls {
		arguments := make(map[string]any)
		if len(call.Arguments) > 0 {
			if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
				arguments = map[string]any{"_raw": string(call.Arguments)}
			}
		}
		converted.ToolCalls = append(converted.ToolCalls, contextpkg.ToolCall{ID: call.ToolCallID, Name: call.Name, Arguments: arguments})
	}
	if message.Role == string(contextpkg.RoleTool) {
		converted.ToolResult = &contextpkg.ToolResult{CallID: message.ToolCallID, Name: message.ToolName, Content: message.Content}
	}
	return converted
}

func (c *DefaultRuntimePreModelCompactor) ValidatePolicy(policy ContextCompactionPolicy) error {
	normalized, err := NormalizeContextCompactionPolicy(policy)
	if err != nil {
		return err
	}
	if normalized.SemanticSummary == SemanticSummaryRequired && (c == nil || len(c.Strategies) == 0) {
		return ErrRuntimePreModelCompactorMissing
	}
	return nil
}

func (c *DefaultRuntimePreModelCompactor) CompactBeforeModel(ctx context.Context, req RuntimePreModelCompactionRequest) (RuntimePreModelCompactionResult, error) {
	policy, err := NormalizeContextCompactionPolicy(req.Policy)
	if err != nil {
		return RuntimePreModelCompactionResult{}, err
	}
	req.Policy = policy
	req.Request = materializeModelInvokeRequest(req.Request)
	if err := ValidatePreserveManifest(req.PreserveManifest, req.Request); err != nil {
		return RuntimePreModelCompactionResult{}, err
	}
	counter := c.Counter
	if counter == nil {
		counter = EstimateModelInputTokenCounter{}
	}
	tokens, err := counter.Count(ctx, req.Request.Messages, req.Request.Tools)
	if err != nil {
		return RuntimePreModelCompactionResult{}, fmt.Errorf("count pre-model input: %w", err)
	}
	limit := req.Request.Package.RuntimeConstraints.TokenBudget.MaxInputTokens
	base := RuntimePreModelCompactionResult{
		Request: req.Request, ObservedTokens: tokens,
		SoftTriggered: limit > 0 && float64(tokens) >= float64(limit)*policy.SoftTriggerRatio,
	}
	if limit <= 0 || float64(tokens) < float64(limit)*policy.CompactTriggerRatio || policy.SemanticSummary == SemanticSummaryDisabled {
		return base, nil
	}
	if len(c.Strategies) == 0 {
		if policy.SemanticSummary == SemanticSummaryRequired {
			return RuntimePreModelCompactionResult{}, ErrRuntimePreModelCompactorMissing
		}
		return base, nil
	}
	result := base
	for _, strategy := range c.Strategies {
		if strategy == nil {
			continue
		}
		step, compactErr := strategy.CompactBeforeModel(ctx, RuntimePreModelCompactionRequest{
			Request: result.Request, Policy: policy, PreserveManifest: req.PreserveManifest,
		})
		if compactErr != nil {
			return RuntimePreModelCompactionResult{}, fmt.Errorf("pre-model compact with %s: %w", strategy.Name(), compactErr)
		}
		if err := ValidatePreserveManifest(req.PreserveManifest, step.Request); err != nil {
			return RuntimePreModelCompactionResult{}, fmt.Errorf("%w: strategy=%s: %v", ErrPreserveManifestInvalid, strategy.Name(), err)
		}
		result.Request = step.Request
		result.AppliedStrategies = append(result.AppliedStrategies, strategy.Name())
		result.AppliedStrategies = append(result.AppliedStrategies, step.AppliedStrategies...)
		result.SummaryRefs = append(result.SummaryRefs, step.SummaryRefs...)
		result.Records = append(result.Records, step.Records...)
		if step.TrimRecordRef != "" {
			result.TrimRecordRef = step.TrimRecordRef
		}
	}
	return result, nil
}

func prepareRuntimePreModelRequest(ctx context.Context, req ModelInvokeRequest, compactor RuntimePreModelCompactor) (ModelInvokeRequest, error) {
	req = materializeModelInvokeRequest(req)
	policy, err := NormalizeContextCompactionPolicy(req.Package.RuntimeConstraints.CompactionPolicy)
	if err != nil {
		return ModelInvokeRequest{}, err
	}
	req.Package.RuntimeConstraints.CompactionPolicy = policy
	manifest := req.PreserveManifest
	if manifest.ManifestHash == "" {
		manifest, err = BuildPreserveManifest(req)
		if err != nil {
			return ModelInvokeRequest{}, err
		}
	}
	if err := ValidatePreserveManifest(manifest, req); err != nil {
		return ModelInvokeRequest{}, err
	}
	if compactor != nil {
		result, compactErr := compactor.CompactBeforeModel(ctx, RuntimePreModelCompactionRequest{
			Request: req, Policy: policy, PreserveManifest: manifest,
		})
		if compactErr != nil {
			return ModelInvokeRequest{}, compactErr
		}
		req = result.Request
		req.PreModelCompaction = ModelPreModelCompaction{
			Completed:  true,
			PolicyHash: policy.PolicyHash, AppliedStrategies: append([]string(nil), result.AppliedStrategies...),
			SummaryRefs: append([]string(nil), result.SummaryRefs...), TrimRecordRef: result.TrimRecordRef,
			Records:        append([]ContextCompactionRecord(nil), result.Records...),
			ObservedTokens: result.ObservedTokens, SoftTriggered: result.SoftTriggered,
		}
	} else {
		if !req.PreModelCompaction.Completed && (policy.SemanticSummary == SemanticSummaryRequired || req.Package.Run.RuntimeMode != "") {
			return ModelInvokeRequest{}, ErrRuntimePreModelCompactorMissing
		}
		if req.PreModelCompaction.Completed && req.PreModelCompaction.PolicyHash != policy.PolicyHash {
			return ModelInvokeRequest{}, fmt.Errorf("%w: pre_model=%s package=%s", ErrContextCompactionPolicyDrift, req.PreModelCompaction.PolicyHash, policy.PolicyHash)
		}
	}
	if req.PreModelCompaction.PolicyHash == "" && req.Package.Run.RuntimeMode == "" {
		req.PreModelCompaction.PolicyHash = policy.PolicyHash
	}
	req.PreserveManifest = manifest
	if err := ValidatePreserveManifest(manifest, req); err != nil {
		return ModelInvokeRequest{}, err
	}
	return req, nil
}

func materializeModelInvokeRequest(req ModelInvokeRequest) ModelInvokeRequest {
	if len(req.Messages) == 0 {
		req.Messages = modelCallMessagesFromContext(req.Package.Messages.ConversationWindow)
	} else {
		req.Messages = cloneModelCallMessages(req.Messages)
	}
	if len(req.Tools) == 0 {
		req.Tools = cloneModelToolDefinitions(req.Package.Capabilities.ToolDefinitions)
	} else {
		req.Tools = cloneModelToolDefinitions(req.Tools)
	}
	return req
}
