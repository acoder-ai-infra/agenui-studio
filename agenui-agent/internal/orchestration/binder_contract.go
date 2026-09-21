package orchestration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/contract"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/edit"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/requirements"
	bindingcontract "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/binding/contract"
	"github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/workspace"
)

func buildBinderContractPrompt(
	userIntent string,
	contractJSON string,
	design string,
	requirementsJSON string,
	editContractJSON string,
	apiResult string,
) (string, bindingcontract.Input, error) {
	var revision contract.Revision
	if err := decodeStrictJSON([]byte(contractJSON), &revision); err != nil {
		return "", bindingcontract.Input{}, fmt.Errorf("Binder contract: %w", err)
	}
	requirementSet, err := parseRequirementSet(requirementsJSON)
	if err != nil {
		return "", bindingcontract.Input{}, err
	}
	fieldHints, actionSlots, slotErr := workspace.ArtifactSlotsJSON(design)
	if slotErr != nil || !json.Valid([]byte(fieldHints)) || !json.Valid([]byte(actionSlots)) {
		return "", bindingcontract.Input{}, errors.New("Binder design sidecars are invalid")
	}
	sources, err := binderContractSources(apiResult)
	if err != nil {
		return "", bindingcontract.Input{}, err
	}
	input := bindingcontract.Input{
		SchemaVersion: bindingcontract.InputSchemaV1,
		Contract:      revision,
		Design: bindingcontract.DesignSnapshot{
			Ref:         "design:" + edit.HashText(design),
			ContentHash: edit.HashText(design),
			FieldHints:  json.RawMessage(fieldHints),
			ActionSlots: json.RawMessage(actionSlots),
		},
		Requirements: requirementSet,
		Sources:      sources,
	}
	if strings.TrimSpace(editContractJSON) != "" {
		var authorization edit.Contract
		if err := decodeStrictJSON([]byte(editContractJSON), &authorization); err != nil {
			return "", bindingcontract.Input{}, fmt.Errorf("Binder edit contract: %w", err)
		}
		input.EditContract = &authorization
	}
	_, err = bindingcontract.BuildInput(input)
	if err != nil {
		return "", bindingcontract.Input{}, err
	}
	prompt := strings.TrimSpace(userIntent)
	if prompt != "" {
		prompt += "\n\n"
	}
	prompt += "权威绑定输入由 Host 保存在 Workspace，不在提示词中复制。" +
		"先调用 agenui_workspace.inspect_binding 获取本轮 Requirement、真实数据源与语义槽位；" +
		"由你完成语义选择，再按工具 schema 调用 agenui_workspace.commit_binding。" +
		"不要读取或重写完整 AGenUI DSL，不要猜测工具投影中不存在的路径、来源或算子。"
	return prompt, input, nil
}

func isUsableBindingUpdateEditContract(
	raw string,
	expected edit.Contract,
) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	var authorization edit.Contract
	if decodeStrictJSON([]byte(raw), &authorization) != nil {
		return false
	}
	actual, actualErr := json.Marshal(authorization)
	want, wantErr := json.Marshal(expected)
	if actualErr != nil || wantErr != nil {
		return false
	}
	return bytes.Equal(actual, want)
}

func parseRequirementSet(raw string) (requirements.Set, error) {
	var compiled requirements.Result
	if err := decodeStrictJSON([]byte(raw), &compiled); err == nil &&
		compiled.RequirementSet.SchemaVersion != "" {
		return compiled.RequirementSet, nil
	}
	var direct requirements.Set
	if err := decodeStrictJSON([]byte(raw), &direct); err != nil || direct.SchemaVersion == "" {
		return requirements.Set{}, errors.New("Binder requirements artifact is invalid")
	}
	return direct, nil
}

func binderContractSources(raw string) ([]bindingcontract.SourceSnapshot, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	normalized := normalizedJSONText(raw)
	var envelope struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(normalized), &envelope); err != nil || len(envelope.Results) == 0 {
		// An explicitly empty governed receipt (results:[] total:0) is the
		// honest "knowledge base found no API" outcome: there are simply no
		// authorized sources, and a Bind Result with zero bindings stays
		// valid. Anything else is corrupt and keeps failing closed.
		if binderFrozenSearchIsEmpty(normalized) {
			return nil, nil
		}
		return nil, errors.New("Binder requires a governed API search receipt")
	}
	if len(envelope.Results) > 3 {
		return nil, errors.New("Binder accepts at most three selected source receipts")
	}
	result := make([]bindingcontract.SourceSnapshot, 0, len(envelope.Results))
	seen := make(map[string]struct{}, len(envelope.Results))
	for index, item := range envelope.Results {
		dataSourceID := binderContractString(item["data_source_id"])
		apiVersion := binderContractString(item["api_version"])
		knowledgeRevision := binderContractString(item["knowledge_revision"])
		contentHash := binderContractString(item["content_hash"])
		if dataSourceID == "" || apiVersion == "" || knowledgeRevision == "" ||
			!strings.HasPrefix(contentHash, "sha256:") {
			return nil, fmt.Errorf("Binder source %d lacks immutable receipt", index)
		}
		sourceID := dataSourceID + "@" + apiVersion
		knowledgeID := knowledgeRevision + "@" + contentHash
		key := sourceID + "\x00" + knowledgeID
		if _, duplicate := seen[key]; duplicate {
			return nil, errors.New("Binder source receipt is duplicated")
		}
		seen[key] = struct{}{}
		paths, listPath := binderSourcePaths(item)
		result = append(result, bindingcontract.SourceSnapshot{
			SourceID: sourceID, KnowledgeID: knowledgeID, Primary: index == 0,
			Namespace: fmt.Sprintf("source_%d", index), Paths: paths, ListPath: listPath,
		})
	}
	return result, nil
}

// binderSourcePaths projects only response facts into the frozen Binder input.
// Endpoint paths, descriptions and relevance scores are discovery metadata and
// must never become writable data paths.
func binderSourcePaths(item map[string]any) ([]string, string) {
	set := make(map[string]struct{})
	if fields, ok := item["response_fields"].([]any); ok {
		for _, raw := range fields {
			field, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			path := strings.TrimSpace(binderContractString(field["path"]))
			if path != "" {
				set[path] = struct{}{}
			}
		}
	}
	listPath := ""
	if profile, ok := item["binding_contract"].(map[string]any); ok {
		listPath = strings.TrimSpace(binderContractString(profile["primary_row_list"]))
		if listPath != "" {
			set[listPath] = struct{}{}
		}
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		paths = nil
	}
	return paths, listPath
}

// canonicalizeBinderContractResultSourceIDs deterministically maps a model echo of
// raw search-receipt identifiers (data_source_id / result_id) back to the
// composite source_id / knowledge_id pairs frozen in the input. A binding
// is rewritten only when it resolves to exactly one authorized source: an
// already-exact pair is kept, an unknown or ambiguous echo stays untouched so
// ValidateResult still rejects it fail-closed.
func canonicalizeBinderContractResultSourceIDs(
	sources []bindingcontract.SourceSnapshot,
	result *bindingcontract.Result,
) {
	if result == nil || len(sources) == 0 {
		return
	}
	exact := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		exact[source.SourceID+"\x00"+source.KnowledgeID] = struct{}{}
	}
	for index := range result.Bindings {
		binding := &result.Bindings[index]
		if _, ok := exact[binding.SourceID+"\x00"+binding.KnowledgeID]; ok {
			continue
		}
		matched := -1
		for candidate, source := range sources {
			if !binderContractIdentityEcho(source.SourceID, binding.SourceID) &&
				!binderContractIdentityEcho(source.KnowledgeID, binding.KnowledgeID) {
				continue
			}
			if matched >= 0 {
				matched = -1
				break
			}
			matched = candidate
		}
		if matched < 0 {
			continue
		}
		binding.SourceID = sources[matched].SourceID
		binding.KnowledgeID = sources[matched].KnowledgeID
	}
}

// dedupeBinderContractResultBindings collapses byte-semantic repeats only. Choosing
// the first of two different source paths, operators or legacy targets would
// silently turn an ambiguous model result into a valid binding, so materially
// different repeats stay in the result for ValidateResult to reject.
func dedupeBinderContractResultBindings(result *bindingcontract.Result) {
	if result == nil || len(result.Bindings) < 2 {
		return
	}
	kept := result.Bindings[:0]
	first := make(map[string]bindingcontract.Binding, len(result.Bindings))
	for _, binding := range result.Bindings {
		leader, exists := first[binding.RequirementID]
		if !exists {
			first[binding.RequirementID] = binding
			kept = append(kept, binding)
			continue
		}
		if sameBinderContractBinding(leader, binding) {
			continue
		}
		kept = append(kept, binding)
	}
	result.Bindings = kept
}

func sameBinderContractBinding(left, right bindingcontract.Binding) bool {
	return left.RequirementID == right.RequirementID &&
		sameStringMultiset(left.TargetSlotIDs, right.TargetSlotIDs) &&
		left.SourceID == right.SourceID &&
		left.KnowledgeID == right.KnowledgeID &&
		left.FieldPath == right.FieldPath &&
		left.ActionPath == right.ActionPath &&
		reflect.DeepEqual(left.Transforms, right.Transforms) &&
		left.RefKey == right.RefKey &&
		left.ComponentID == right.ComponentID
}

// sameStringMultiset compares target_slot_ids using their wire-contract
// semantics: order is irrelevant, while cardinality (including duplicates)
// remains significant. The duplicate count matters because normalization must
// not hide a malformed repeated slot from the later strict validator.
func sameStringMultiset(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, value := range a {
		counts[value]++
	}
	for _, value := range b {
		remaining := counts[value]
		if remaining == 0 {
			return false
		}
		if remaining == 1 {
			delete(counts, value)
		} else {
			counts[value] = remaining - 1
		}
	}
	return len(counts) == 0
}

// binderContractIdentityEcho reports whether a model-authored identifier
// unmistakably refers to the authorized identity: the exact value, its
// revision-less prefix, a non-trivial embedded fragment such as the bare API
// path inside a host-observed entity ID
// (api:host-observed:/ws/tools/x@rev vs "/ws/tools/x"), or a shared content
// digest such as "api-bc2ec2cc94ee2bbd" vs "snapshot-bc2ec2cc94ee2bbd@…".
func binderContractIdentityEcho(authorized string, echo string) bool {
	echo = strings.TrimSpace(echo)
	if echo == "" || authorized == "" {
		return false
	}
	if authorized == echo || strings.HasPrefix(authorized, echo+"@") {
		return true
	}
	if len(echo) >= 8 && strings.Contains(authorized, echo) {
		return true
	}
	digest := longestHexFragment(echo)
	return len(digest) >= 12 && strings.Contains(authorized, digest)
}

func longestHexFragment(value string) string {
	best, start := "", -1
	for index := 0; index <= len(value); index++ {
		isHex := index < len(value) &&
			((value[index] >= '0' && value[index] <= '9') ||
				(value[index] >= 'a' && value[index] <= 'f'))
		if isHex {
			if start < 0 {
				start = index
			}
			continue
		}
		if start >= 0 && index-start > len(best) {
			best = value[start:index]
		}
		start = -1
	}
	return best
}

// binderContractResultDiag summarizes one validated Bind Result as grep-stable
// key=value tokens for the [agenui-binding-diag] line: per-status binding count
// and a deterministic issue-code histogram, so an evaluation harness can
// attribute unbound requirements without parsing free-form messages.
func binderContractResultDiag(result bindingcontract.Result) string {
	bound := 0
	operatorBound := 0
	for _, binding := range result.Bindings {
		bound++
		if len(binding.Transforms) > 0 {
			operatorBound++
		}
	}
	codes := map[string]int{}
	for _, issue := range result.Issues {
		codes[strings.ToUpper(strings.TrimSpace(issue.Code))]++
	}
	names := make([]string, 0, len(codes))
	for name := range codes {
		names = append(names, name)
	}
	sort.Strings(names)
	histogram := make([]string, 0, len(names))
	for _, name := range names {
		histogram = append(histogram, fmt.Sprintf("%s:%d", name, codes[name]))
	}
	if len(histogram) == 0 {
		histogram = append(histogram, "-")
	}
	return fmt.Sprintf(
		"status=%s bindings=%d operator_bindings=%d issues=%d issue_codes=%s",
		result.Status, bound, operatorBound, len(result.Issues),
		strings.Join(histogram, ","),
	)
}

func parseBinderContractResult(raw string) (bindingcontract.Result, string, error) {
	submission, err := bindingcontract.ParseSubmission(raw)
	if err != nil {
		return bindingcontract.Result{}, "", err
	}
	plan, err := bindingcontract.EncodeExecutablePlan(submission.Plan.FieldMappings, submission.Plan.ActionMappings)
	if err != nil {
		return bindingcontract.Result{}, "", err
	}
	return submission.Result, plan, nil
}

func decodeStrictJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}

func binderContractString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func buildBindingUpdateEditContract(
	runID string,
	baseGenerationID string,
	baseCardRevision int64,
	userIntent string,
	contractJSON string,
	design string,
	requirementsJSON string,
	baselineBinding string,
	targetRequirementIDs []string,
	tenantID string,
	userID string,
	sessionID string,
) (edit.Contract, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(baseGenerationID) == "" ||
		baseCardRevision < 1 || strings.TrimSpace(userIntent) == "" ||
		strings.TrimSpace(baselineBinding) == "" ||
		strings.TrimSpace(tenantID) == "" || strings.TrimSpace(userID) == "" ||
		strings.TrimSpace(sessionID) == "" {
		return edit.Contract{}, errors.New("binding edit contract: invocation identity and query are required")
	}
	var revision contract.Revision
	if err := decodeStrictJSON([]byte(contractJSON), &revision); err != nil {
		return edit.Contract{}, fmt.Errorf("binding edit contract: content contract: %w", err)
	}
	requirementSet, err := parseRequirementSet(requirementsJSON)
	if err != nil {
		return edit.Contract{}, fmt.Errorf("binding edit contract: requirements: %w", err)
	}
	targets, err := resolveBindingEditTargets(targetRequirementIDs, requirementSet)
	if err != nil {
		return edit.Contract{}, err
	}
	slotHash, err := edit.SlotSignatureHash(design)
	if err != nil {
		return edit.Contract{}, err
	}
	seed := strings.Join([]string{
		tenantID, userID, sessionID, runID, baseGenerationID,
		strconv.FormatInt(baseCardRevision, 10), revision.ContractID,
		revision.ContentHash, edit.HashText(design), strings.Join(targetRequirementIDs, ","),
		strings.TrimSpace(userIntent),
	}, "\x00")
	digest := strings.TrimPrefix(edit.HashText(seed), "sha256:")
	if len(digest) > 16 {
		digest = digest[:16]
	}
	return edit.Contract{
		EditID:           "edit_" + digest,
		SchemaVersion:    edit.SchemaVersion,
		BaseGenerationID: baseGenerationID,
		BaseCardRevision: baseCardRevision,
		ChangeScope:      "binding_update",
		Operation:        "update",
		Intent:           strings.TrimSpace(userIntent),
		TargetSet:        targets,
		ProtectedSet: []edit.Protection{
			{Kind: "content_contract", ID: revision.ContractID},
			{Kind: "design", ID: edit.HashText(design)},
			{Kind: "all_other_requirements", ID: "*"},
		},
		ImpactSet: []edit.Impact{
			{Kind: "binding", Action: "regenerate_partial"},
			{Kind: "runtime_preview", Action: "regenerate"},
		},
		Preconditions: edit.Preconditions{
			ContentContractHash: revision.ContentHash,
			DesignHash:          edit.HashText(design),
			SlotSignatureHash:   slotHash,
			BindingPlanHash:     edit.HashText(baselineBinding),
		},
		Acceptance: edit.Acceptance{
			TargetChanged: true, ProtectedObjectsUnchanged: true,
			RuntimePreviewRequired: true,
		},
		IdempotencyKey: sessionID + ":" + runID + ":" + "edit_" + digest,
	}, nil
}

func resolveBindingEditTargets(
	selected []string,
	requirementSet requirements.Set,
) ([]edit.Target, error) {
	available := make(map[string]edit.Target, len(requirementSet.Data)+len(requirementSet.Actions))
	for _, requirement := range requirementSet.Data {
		available[requirement.RequirementID] = edit.Target{
			Kind:           "data_requirement",
			ID:             requirement.RequirementID,
			ComponentID:    firstString(requirement.TargetSlotIDs...),
			ContractItemID: requirement.ContractItemID,
		}
	}
	for _, requirement := range requirementSet.Actions {
		available[requirement.RequirementID] = edit.Target{
			Kind:             "action_requirement",
			ID:               requirement.RequirementID,
			ComponentID:      firstString(requirement.TargetSlotIDs...),
			ContractActionID: requirement.ContractActionID,
		}
	}
	seen := make(map[string]struct{}, len(selected))
	targets := make([]edit.Target, 0, len(selected))
	for _, id := range selected {
		id = strings.TrimSpace(id)
		target, ok := available[id]
		if !ok {
			return nil, fmt.Errorf("binding edit contract: unknown requirement %q", id)
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return nil, errors.New("binding edit contract: selected requirements are required")
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	return targets, nil
}

func firstString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
