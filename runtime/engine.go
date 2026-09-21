package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type Diagnostic struct {
	Level   string `json:"level"`
	Binding string `json:"binding,omitempty"`
	Source  string `json:"source,omitempty"`
	Message string `json:"message"`
}

type ResolvedAction struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Type        string `json:"type"`
	ComponentID string `json:"componentId"`
	Value       any    `json:"value"`
}

type Result struct {
	CardID      string           `json:"cardId"`
	DataModel   map[string]any   `json:"dataModel"`
	Entities    []map[string]any `json:"entities,omitempty"`
	Actions     []ResolvedAction `json:"resolvedActions,omitempty"`
	Diagnostics []Diagnostic     `json:"diagnostics,omitempty"`
}

type Engine struct {
	Fetcher   Fetcher
	Operators *JSExecutor
}

func NewEngine(fetcher Fetcher, operators *JSExecutor) *Engine {
	if fetcher == nil {
		fetcher = NewHTTPFetcher(0)
	}
	if operators == nil {
		operators = NewJSExecutor(ExecutorConfig{})
	}
	return &Engine{Fetcher: fetcher, Operators: operators}
}

func (e *Engine) Execute(ctx context.Context, pkg *Package, params map[string]any) (*Result, error) {
	if e == nil || e.Fetcher == nil || e.Operators == nil {
		return nil, fmt.Errorf("runtime: engine capabilities are unavailable")
	}
	if pkg == nil {
		return nil, fmt.Errorf("runtime: package is required")
	}
	if err := pkg.Validate(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if params == nil {
		params = map[string]any{}
	}
	result := &Result{CardID: pkg.CardID, DataModel: templateDataModel(pkg)}
	responses, fetchErrs := fetchAll(ctx, e.Fetcher, pkg, params)
	primary, _ := pkg.PrimaryDataSource()
	if err, failed := fetchErrs[primary.ID]; failed {
		return nil, fmt.Errorf("runtime: primary data source %s failed: %w", primary.ID, err)
	}
	entities := extractEntities(responses[primary.ID], primary.ItemsPath)
	result.Diagnostics = append(result.Diagnostics, mergeSupplements(pkg, entities, responses, fetchErrs)...)
	for id, err := range fetchErrs {
		if id != primary.ID {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{
				Level: "warn", Source: id, Message: fmt.Sprintf("supplement source failed: %v", err),
			})
		}
	}
	for _, binding := range pkg.Bindings {
		if err := e.applyBinding(ctx, pkg, binding, responses, fetchErrs, entities, result); err != nil {
			return nil, err
		}
	}
	if err := applyActions(pkg, responses, result.DataModel, result); err != nil {
		return nil, err
	}
	result.Entities = copyEntities(entities)
	return result, nil
}

func (e *Engine) applyBinding(
	ctx context.Context,
	pkg *Package,
	binding Binding,
	responses map[string]any,
	fetchErrs map[string]error,
	entities []map[string]any,
	result *Result,
) error {
	fieldTokens, _ := parseFieldPath(binding.FieldPath) // Package.Validate has already parsed both paths.
	refTokens, _ := parseRefKey(binding.RefKey)
	var err error
	if fetchErr := fetchErrs[binding.DataSourceID]; fetchErr != nil {
		if wildcardCount(refTokens) > 0 {
			if wildcardCount(refTokens) != 1 {
				return executionError(CodeSourceJoinNotProven, "cannot reconstruct nested supplement coordinates after source failure", binding.SlotID, 0, false, fetchErr)
			}
			for index := range entities {
				value, write, missingErr := resolveMissing(binding, CodeBindingRequiredValueMissing, fetchErr, result)
				if missingErr != nil {
					return missingErr
				}
				if !write {
					continue
				}
				value, transformErr := e.applyTransforms(ctx, pkg, binding, value)
				if transformErr != nil {
					return transformErr
				}
				if assignErr := assignBindingValue(result.DataModel, refTokens, []int{index}, value, binding.SlotID); assignErr != nil {
					return assignErr
				}
			}
			return nil
		}
		value, write, missingErr := resolveMissing(binding, CodeBindingRequiredValueMissing, fetchErr, result)
		if missingErr != nil || !write {
			return missingErr
		}
		transformed, transformErr := e.applyTransforms(ctx, pkg, binding, value)
		if transformErr != nil {
			return transformErr
		}
		return assignBindingValue(result.DataModel, refTokens, nil, transformed, binding.SlotID)
	}
	if wildcardCount(refTokens) == 0 {
		value, found, missingCode := extractProjected(responses[binding.DataSourceID], fieldTokens)
		if !found {
			var write bool
			value, write, err = resolveMissing(binding, missingCode, nil, result)
			if err != nil || !write {
				return err
			}
		}
		value, err = e.applyTransforms(ctx, pkg, binding, value)
		if err != nil {
			return err
		}
		return assignBindingValue(result.DataModel, refTokens, nil, value, binding.SlotID)
	}
	matches, matchErr := bindingMatches(pkg, binding, responses[binding.DataSourceID], fieldTokens, entities)
	if matchErr != nil {
		return matchErr
	}
	for _, match := range matches {
		if match.empty {
			if err := assignEmptyRef(result.DataModel, refTokens, match.coordinates); err != nil {
				return executionError(CodeBindingTargetAssignFailed, "cannot assign empty wildcard result", binding.SlotID, 0, false, err)
			}
			continue
		}
		value := match.value
		if !match.found {
			var write bool
			value, write, err = resolveMissing(binding, match.code, nil, result)
			if err != nil {
				return err
			}
			if !write {
				continue
			}
		}
		value, err = e.applyTransforms(ctx, pkg, binding, value)
		if err != nil {
			return err
		}
		if err := assignBindingValue(result.DataModel, refTokens, match.coordinates, value, binding.SlotID); err != nil {
			return err
		}
	}
	return nil
}

func bindingMatches(pkg *Package, binding Binding, response any, fieldTokens []pathToken, entities []map[string]any) ([]pathMatch, error) {
	dataSource, _ := pkg.FindDataSource(binding.DataSourceID)
	primary, _ := pkg.PrimaryDataSource()
	if dataSource.Role != RoleSupplement {
		return selectPath(response, fieldTokens), nil
	}
	if dataSource.EntityKey == "" || primary.EntityKey == "" || dataSource.EntityKey != primary.EntityKey {
		return nil, executionError(CodeSourceJoinNotProven, "supplement entity join is not proven", binding.SlotID, 0, false, nil)
	}
	relative, ok := relativeItemTokens(fieldTokens, dataSource.ItemsPath)
	if !ok {
		return nil, executionError(CodeBindingWildcardMismatch, "supplement wildcard path is outside itemsPath", binding.SlotID, 0, false, nil)
	}
	matches := make([]pathMatch, 0, len(entities))
	for index, entity := range entities {
		supplement, found := entity["@supplement:"+dataSource.ID].(map[string]any)
		if !found {
			matches = append(matches, pathMatch{coordinates: []int{index}, code: CodeMapKeyNotFound})
			continue
		}
		selected := selectPath(supplement, relative)
		for _, match := range selected {
			match.coordinates = append([]int{index}, match.coordinates...)
			matches = append(matches, match)
		}
	}
	return matches, nil
}

func relativeItemTokens(tokens []pathToken, itemsPath string) ([]pathToken, bool) {
	itemsTokens, err := parseFieldPath(itemsPath)
	if err != nil || len(tokens) <= len(itemsTokens) {
		return nil, false
	}
	for index, token := range itemsTokens {
		if !samePathToken(token, tokens[index]) {
			return nil, false
		}
	}
	if tokens[len(itemsTokens)].kind != pathTokenWildcard {
		return nil, false
	}
	return tokens[len(itemsTokens)+1:], true
}

func samePathToken(left, right pathToken) bool {
	return left.kind == right.kind && left.field == right.field && left.index == right.index
}

func (e *Engine) applyTransforms(ctx context.Context, pkg *Package, binding Binding, value any) (any, error) {
	current := value
	for _, invocation := range binding.Transforms {
		op, _ := pkg.FindOperator(invocation.OperatorVersionID)
		params := normalizedParams(invocation.Params)
		if err := validateSchemaValue(op.InputSchema, current); err != nil {
			return nil, executionError(CodeOperatorInputValidationFailed, "operator input does not match inputSchema", binding.SlotID, op.OperatorVersionID, false, err)
		}
		if err := validateSchemaValue(op.ParamsSchema, params); err != nil {
			return nil, executionError(CodeOperatorParamsValidationFailed, "operator params do not match paramsSchema", binding.SlotID, op.OperatorVersionID, false, err)
		}
		next, err := e.Operators.Execute(ctx, op, current, params)
		if err != nil {
			code := CodeOperatorExecutionFailed
			var failure *operatorFailure
			if errors.As(err, &failure) {
				code = failure.code
			}
			return nil, executionError(code, "operator execution failed", binding.SlotID, op.OperatorVersionID, true, err)
		}
		if err := validateSchemaValue(op.OutputSchema, next); err != nil {
			return nil, executionError(CodeOperatorOutputValidationFailed, "operator output does not match outputSchema", binding.SlotID, op.OperatorVersionID, true, err)
		}
		current = next
	}
	return current, nil
}

func resolveMissing(binding Binding, code string, cause error, result *Result) (any, bool, error) {
	if code == "" {
		code = CodeBindingRequiredValueMissing
	}
	switch binding.MissingPolicy {
	case MissingFallback:
		return binding.FallbackValue, true, nil
	case MissingHide:
		result.Diagnostics = append(result.Diagnostics, Diagnostic{
			Level: "info", Binding: binding.SlotID, Message: "binding value missing; target hidden",
		})
		return nil, false, nil
	default:
		return nil, false, executionError(code, "required binding value is missing", binding.SlotID, 0, false, cause)
	}
}

func assignBindingValue(dataModel map[string]any, refTokens []pathToken, coordinates []int, value any, bindingID string) error {
	if err := assignRef(dataModel, refTokens, coordinates, value); err != nil {
		return executionError(CodeBindingTargetAssignFailed, "cannot assign binding result to refKey", bindingID, 0, false, err)
	}
	return nil
}

type actionPayload struct {
	DataSourceID string `json:"dataSourceId"`
	Path         string `json:"path"`
	ComponentID  string `json:"componentId"`
}

func applyActions(pkg *Package, responses map[string]any, dataModel map[string]any, result *Result) error {
	for _, action := range pkg.Actions {
		var payload actionPayload
		if err := json.Unmarshal(action.Payload, &payload); err != nil {
			return fmt.Errorf("runtime: action %s payload: %w", action.ID, err)
		}
		value, ok := Extract(responses[payload.DataSourceID], payload.Path)
		if !ok {
			return fmt.Errorf("runtime: action %s value missing at %s", action.ID, payload.Path)
		}
		result.Actions = append(result.Actions, ResolvedAction{
			ID: action.ID, Name: action.Name, Type: action.Type, ComponentID: payload.ComponentID, Value: value,
		})
		if !Assign(dataModel, "__actions."+payload.ComponentID+"."+action.Type, value) {
			return fmt.Errorf("runtime: action %s cannot assign component %q", action.ID, payload.ComponentID)
		}
	}
	return nil
}

func extractEntities(response any, itemsPath string) []map[string]any {
	if strings.TrimSpace(itemsPath) == "" {
		return nil
	}
	value, ok := Extract(response, itemsPath)
	if !ok {
		return nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	entities := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if entity, ok := item.(map[string]any); ok {
			entities = append(entities, entity)
		}
	}
	return entities
}

func templateDataModel(pkg *Package) map[string]any {
	dataModel := map[string]any{}
	for _, operation := range pkg.Protocol {
		var update struct {
			UpdateDataModel *struct {
				Path  string         `json:"path"`
				Value map[string]any `json:"value"`
			} `json:"updateDataModel"`
		}
		if json.Unmarshal(operation, &update) != nil || update.UpdateDataModel == nil || update.UpdateDataModel.Value == nil {
			continue
		}
		if update.UpdateDataModel.Path == "" || update.UpdateDataModel.Path == "/" {
			dataModel = deepCopyMap(update.UpdateDataModel.Value)
		}
	}
	return dataModel
}

func deepCopyMap(input map[string]any) map[string]any {
	raw, _ := json.Marshal(input)
	output := map[string]any{}
	_ = json.Unmarshal(raw, &output)
	return output
}

func copyEntities(entities []map[string]any) []map[string]any {
	if len(entities) == 0 {
		return nil
	}
	result := make([]map[string]any, 0, len(entities))
	for _, entity := range entities {
		copy := deepCopyMap(entity)
		for key := range copy {
			if strings.HasPrefix(key, "@supplement:") {
				delete(copy, key)
			}
		}
		result = append(result, copy)
	}
	return result
}
