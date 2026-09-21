// Package exportpackage builds the self-contained input consumed by the
// standalone AGenUI runtime. It expands source and operator identities only
// from frozen, Host-admitted artifacts; model output is never used as a
// lookup authority.
package exportpackage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/requirements"
	stepartifact "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/artifact"
	bindingcontract "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/binding/contract"
	platformoperator "github.com/AGenUI/agenui-studio/agenui-agent/internal/operator"
	"github.com/AGenUI/agenui-studio/harness/sdk"
)

const Version = "1.0"

// ErrStaleFinal prevents a downloaded or published runtime package from
// silently delivering an older materialization after a newer Design edit.
var ErrStaleFinal = errors.New("package exporter: latest design is not materialized")

type ArtifactStore interface {
	Load(context.Context, harness.Identity, string) (string, error)
	LatestRunID(context.Context, harness.Identity, string) (string, error)
}

type latestStepArtifactStore interface {
	LatestStep(context.Context, harness.Identity, string) (stepartifact.LatestStep, error)
}

type OperatorResolver interface {
	GetOperator(context.Context, uint64) (platformoperator.OperatorDetail, error)
}

type Exporter struct {
	steps   ArtifactStore
	baseURL string
	ops     OperatorResolver
	now     func() time.Time
}

func New(steps ArtifactStore, baseURL string, operators OperatorResolver) (*Exporter, error) {
	if steps == nil {
		return nil, errors.New("package exporter: artifact store is required")
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL != "" {
		parsed, err := url.Parse(baseURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, errors.New("package exporter: data source base URL must be absolute")
		}
	}
	return &Exporter{steps: steps, baseURL: baseURL, ops: operators, now: time.Now}, nil
}

type Package struct {
	Version     string            `json:"version"`
	CardID      string            `json:"cardId"`
	Name        string            `json:"name"`
	Contract    json.RawMessage   `json:"contract,omitempty"`
	Protocol    []json.RawMessage `json:"protocol"`
	DataSources []DataSource      `json:"dataSources"`
	Bindings    []Binding         `json:"bindings"`
	Operators   []Operator        `json:"operators,omitempty"`
	Actions     []Action          `json:"actions,omitempty"`
	Meta        Meta              `json:"meta"`
}

type DataSource struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Endpoint    string            `json:"endpoint"`
	Method      string            `json:"method"`
	Headers     map[string]string `json:"headers,omitempty"`
	Role        string            `json:"role"`
	ItemsPath   string            `json:"itemsPath,omitempty"`
	EntityKey   string            `json:"entityKey,omitempty"`
	Params      []Param           `json:"params,omitempty"`
}

type Param struct {
	Name     string `json:"name"`
	In       string `json:"in"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Template string `json:"template,omitempty"`
	Value    any    `json:"value,omitempty"`
}

type Invocation struct {
	OperatorVersionID uint64         `json:"operatorVersionId"`
	Params            map[string]any `json:"params,omitempty"`
}

type Binding struct {
	SlotID        string       `json:"slotId"`
	RequirementID string       `json:"requirementId"`
	DataSourceID  string       `json:"dataSourceId"`
	FieldPath     string       `json:"fieldPath"`
	Target        string       `json:"target"`
	Scope         string       `json:"scope"`
	MissingPolicy string       `json:"missingPolicy"`
	Transform     []Invocation `json:"transform,omitempty"`
}

type Operator struct {
	OperatorVersionID uint64 `json:"operatorVersionId"`
	Version           int    `json:"version"`
	SourceHash        string `json:"sourceHash,omitempty"`
	Language          string `json:"language"`
	LanguageVersion   string `json:"languageVersion,omitempty"`
	SourceCode        string `json:"sourceCode"`
	Entry             string `json:"entry"`
}

type Action struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload,omitempty"`
}

type Meta struct {
	ContractHash     string    `json:"contractHash,omitempty"`
	DesignHash       string    `json:"designHash,omitempty"`
	RequirementsHash string    `json:"requirementsHash,omitempty"`
	GeneratedAt      time.Time `json:"generatedAt"`
	Generator        string    `json:"generator"`
}

func (e *Exporter) Export(ctx context.Context, tenantID, userID, sessionID string) ([]byte, error) {
	identity := harness.Identity{TenantID: tenantID, UserID: userID, SessionID: sessionID}
	runID, err := e.steps.LatestRunID(ctx, identity, stepartifact.StepFinal)
	if err != nil {
		return nil, fmt.Errorf("package exporter: final artifact Run: %w", err)
	}
	if err := latestFinalIsCurrent(ctx, e.steps, identity); err != nil {
		return nil, err
	}
	identity.RunID = runID
	load := func(step string) (string, error) { return e.steps.Load(ctx, identity, step) }
	finalRaw, err := load(stepartifact.StepFinal)
	if err != nil {
		return nil, fmt.Errorf("package exporter: final protocol: %w", err)
	}
	var protocol []json.RawMessage
	if json.Unmarshal([]byte(finalRaw), &protocol) != nil || len(protocol) == 0 {
		return nil, errors.New("package exporter: final protocol is invalid")
	}
	contractRaw, err := load(stepartifact.StepContract)
	if err != nil || !json.Valid([]byte(contractRaw)) {
		return nil, errors.New("package exporter: content contract is unavailable")
	}
	requirementsRaw, err := load(stepartifact.StepRequirements)
	if err != nil {
		return nil, fmt.Errorf("package exporter: requirements: %w", err)
	}
	requirementSet, err := decodeRequirements(requirementsRaw)
	if err != nil {
		return nil, err
	}
	bindingRaw, err := load(stepartifact.StepBinding)
	if err != nil {
		return nil, fmt.Errorf("package exporter: binding: %w", err)
	}
	result, err := decodeBindingResult(bindingRaw)
	if err != nil {
		return nil, err
	}
	searchRaw, err := load(stepartifact.StepSearch)
	if err != nil {
		return nil, fmt.Errorf("package exporter: source receipt: %w", err)
	}
	sources, sourceIndex, err := e.expandSources(searchRaw, result)
	if err != nil {
		return nil, err
	}
	bindings, actions, operatorIDs, err := expandBindings(result, requirementSet, sourceIndex)
	if err != nil {
		return nil, err
	}
	operators, err := e.expandOperators(ctx, operatorIDs)
	if err != nil {
		return nil, err
	}
	designRaw, err := load(stepartifact.StepDesign)
	if err != nil || !json.Valid([]byte(designRaw)) {
		return nil, errors.New("package exporter: design is unavailable")
	}
	name := runID
	var contractEnvelope struct {
		Goal string `json:"goal"`
	}
	if json.Unmarshal([]byte(contractRaw), &contractEnvelope) == nil && strings.TrimSpace(contractEnvelope.Goal) != "" {
		name = strings.TrimSpace(contractEnvelope.Goal)
	}
	pkg := Package{
		Version: Version, CardID: runID, Name: name,
		Contract: json.RawMessage(contractRaw), Protocol: protocol,
		DataSources: sources, Bindings: bindings, Operators: operators, Actions: actions,
		Meta: Meta{ContractHash: contentHash(contractRaw), DesignHash: contentHash(designRaw),
			RequirementsHash: contentHash(requirementsRaw), GeneratedAt: e.now().UTC(), Generator: "agenui-studio"},
	}
	return json.MarshalIndent(pkg, "", "  ")
}

// latestFinalIsCurrent is intentionally stricter than preview selection. A
// preview may render a newer Design draft, while an executable package must
// never claim to contain that design until the Design, Binding and Final are
// materialized together.
func latestFinalIsCurrent(ctx context.Context, store ArtifactStore, identity harness.Identity) error {
	timed, ok := store.(latestStepArtifactStore)
	if !ok {
		return nil
	}
	final, err := timed.LatestStep(ctx, identity, stepartifact.StepFinal)
	if err != nil {
		return fmt.Errorf("package exporter: final artifact Run: %w", err)
	}
	design, err := timed.LatestStep(ctx, identity, stepartifact.StepDesign)
	if err != nil {
		return nil
	}
	if design.CreatedAt.After(final.CreatedAt) {
		return ErrStaleFinal
	}
	return nil
}

type sourceReceipt struct {
	Path         string         `json:"path"`
	Method       string         `json:"method"`
	Description  string         `json:"description"`
	DataSourceID string         `json:"data_source_id"`
	APIVersion   string         `json:"api_version"`
	Entity       map[string]any `json:"entity"`
	Profile      map[string]any `json:"binding_contract"`
}

func (e *Exporter) expandSources(raw string, result bindingcontract.Result) ([]DataSource, map[string]string, error) {
	var envelope struct {
		Results []sourceReceipt `json:"results"`
	}
	if json.Unmarshal([]byte(raw), &envelope) != nil || len(envelope.Results) == 0 {
		return nil, nil, errors.New("package exporter: source receipt has no expanded results")
	}
	used := make(map[string]struct{})
	for _, binding := range result.Bindings {
		used[binding.SourceID] = struct{}{}
	}
	var sources []DataSource
	index := make(map[string]string)
	for _, receipt := range envelope.Results {
		authorityID := receipt.DataSourceID + "@" + receipt.APIVersion
		if _, ok := used[authorityID]; !ok {
			continue
		}
		endpoint := strings.TrimSpace(receipt.Path)
		parsed, _ := url.Parse(endpoint)
		if parsed == nil || parsed.Scheme == "" || parsed.Host == "" {
			if e.baseURL == "" || !strings.HasPrefix(endpoint, "/") {
				return nil, nil, fmt.Errorf("package exporter: source %q has relative endpoint and no export base URL", authorityID)
			}
			endpoint = e.baseURL + endpoint
		}
		id := fmt.Sprintf("ds-%d", len(sources)+1)
		role := "supplement"
		if len(sources) == 0 {
			role = "primary"
		}
		headers, params, profileErr := decodeExecutionProfile(receipt.Profile)
		if profileErr != nil {
			return nil, nil, fmt.Errorf("package exporter: source %q execution profile: %w", authorityID, profileErr)
		}
		sources = append(sources, DataSource{ID: id, Name: receipt.DataSourceID,
			Description: receipt.Description, Endpoint: endpoint, Method: strings.ToUpper(receipt.Method),
			Headers: headers, Params: params, Role: role, ItemsPath: stringFact(receipt.Profile, "primary_row_list"), EntityKey: stringFact(receipt.Entity, "entity_key")})
		index[authorityID] = id
	}
	if len(sources) == 0 {
		return nil, nil, errors.New("package exporter: bound sources are absent from frozen receipt")
	}
	return sources, index, nil
}

func expandBindings(result bindingcontract.Result, set requirements.Set, sources map[string]string) ([]Binding, []Action, []uint64, error) {
	missing := make(map[string]string)
	for _, requirement := range set.Data {
		missing[requirement.RequirementID] = runtimeMissingPolicy(requirement.WhenMissing)
	}
	var bindings []Binding
	var actions []Action
	opSet := make(map[uint64]struct{})
	for _, item := range result.Bindings {
		ds := sources[item.SourceID]
		if ds == "" {
			return nil, nil, nil, fmt.Errorf("package exporter: binding %s references unexpanded source", item.RequirementID)
		}
		if item.ActionPath != "" {
			actions = append(actions, Action{ID: item.RequirementID, Name: item.RequirementID,
				Type: item.ActionSourceType, Payload: map[string]any{"dataSourceId": ds, "path": item.ActionPath, "componentId": item.ComponentID}})
			continue
		}
		for _, slot := range item.TargetSlotIDs {
			binding := Binding{SlotID: slot, RequirementID: item.RequirementID, DataSourceID: ds,
				FieldPath: item.FieldPath, Target: runtimeTarget(item.RefKey), Scope: runtimeScope(item.RefKey),
				MissingPolicy: missing[item.RequirementID]}
			if binding.MissingPolicy == "" {
				binding.MissingPolicy = "hide"
			}
			for _, transform := range item.Transforms {
				if transform.OperatorVersionID == 0 {
					return nil, nil, nil, fmt.Errorf("package exporter: binding %s has invalid operator version", item.RequirementID)
				}
				binding.Transform = append(binding.Transform, Invocation{OperatorVersionID: transform.OperatorVersionID, Params: transform.Params})
				opSet[transform.OperatorVersionID] = struct{}{}
			}
			bindings = append(bindings, binding)
		}
	}
	operatorIDs := make([]uint64, 0, len(opSet))
	for id := range opSet {
		operatorIDs = append(operatorIDs, id)
	}
	sort.Slice(operatorIDs, func(i, j int) bool { return operatorIDs[i] < operatorIDs[j] })
	return bindings, actions, operatorIDs, nil
}

func (e *Exporter) expandOperators(ctx context.Context, ids []uint64) ([]Operator, error) {
	if len(ids) > 0 && e.ops == nil {
		return nil, errors.New("package exporter: operator detail resolver is unavailable")
	}
	result := make([]Operator, 0, len(ids))
	for _, id := range ids {
		detail, err := e.ops.GetOperator(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("package exporter: expand operator %d: %w", id, err)
		}
		version, err := strconv.Atoi(detail.Version)
		if err != nil || version < 1 || detail.Code == "" || detail.Entry == "" {
			return nil, fmt.Errorf("package exporter: operator %d detail is incomplete", id)
		}
		result = append(result, Operator{OperatorVersionID: id, Version: version,
			SourceHash: contentHash(detail.Code), Language: detail.Language, LanguageVersion: detail.LanguageVersion,
			SourceCode: detail.Code, Entry: detail.Entry})
	}
	return result, nil
}

func decodeBindingResult(raw string) (bindingcontract.Result, error) {
	submission, err := bindingcontract.ParseSubmission(raw)
	if err != nil {
		return bindingcontract.Result{}, errors.New("package exporter: binding result is invalid")
	}
	if !bindingcontract.IsExecutableStatus(submission.Result.Status) {
		return bindingcontract.Result{}, fmt.Errorf("package exporter: binding status %s is not executable", submission.Result.Status)
	}
	return submission.Result, nil
}

func decodeRequirements(raw string) (requirements.Set, error) {
	var compiled requirements.Result
	if json.Unmarshal([]byte(raw), &compiled) == nil && compiled.RequirementSet.SchemaVersion != "" {
		return compiled.RequirementSet, nil
	}
	var direct requirements.Set
	if json.Unmarshal([]byte(raw), &direct) != nil || direct.SchemaVersion == "" {
		return direct, errors.New("package exporter: requirements are invalid")
	}
	return direct, nil
}

func runtimeTarget(ref string) string {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "$")
	ref = strings.TrimPrefix(ref, "/")
	// A list-item binding is assigned to each list entity by Runtime.  Its
	// target must therefore be relative to that entity: /items[*]/title is
	// "title", not "itemstitle".  Card-scoped references keep their complete
	// object path below.
	if marker := strings.LastIndex(ref, "[*]/"); marker >= 0 {
		ref = ref[marker+len("[*]/"):]
	}
	ref = strings.ReplaceAll(ref, "/", ".")
	return ref
}

func runtimeScope(ref string) string {
	if strings.Contains(ref, "[*]") {
		return "list_item"
	}
	return "card"
}

func runtimeMissingPolicy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "block", "hide", "fallback":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "hide"
	}
}

func stringFact(value map[string]any, key string) string {
	text, _ := value[key].(string)
	return strings.TrimSpace(text)
}

func contentHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("sha256:%x", sum[:])
}

func decodeExecutionProfile(profile map[string]any) (map[string]string, []Param, error) {
	var headers map[string]string
	var params []Param
	if value, ok := profile["headers"]; ok {
		raw, _ := json.Marshal(value)
		if json.Unmarshal(raw, &headers) != nil {
			return nil, nil, errors.New("headers must be a string map")
		}
	}
	if value, ok := profile["params"]; ok {
		raw, _ := json.Marshal(value)
		if json.Unmarshal(raw, &params) != nil {
			return nil, nil, errors.New("params must be an array")
		}
	}
	return headers, params, nil
}
