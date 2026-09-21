package operator

import (
	"context"
	"fmt"
	"strings"
)

type BuiltinFunc func(context.Context, BuiltinInput) (any, error)

type BuiltinInput struct {
	OperatorID uint64
	Name       string
	Value      any
	Config     map[string]any
	Context    map[string]any
	Detail     OperatorDetail
}

type BuiltinExecutor struct {
	functions map[string]BuiltinFunc
}

func SurfaceFetchBuiltinExecutor() *BuiltinExecutor {
	return NewBuiltinExecutor(SurfaceFetchBuiltinFunctions())
}

func SurfaceFetchBuiltinFunctions() map[string]BuiltinFunc {
	return map[string]BuiltinFunc{
		"array_join": builtinArrayJoin,
		"omit_empty": builtinOmitEmpty,
	}
}

func NewBuiltinExecutor(functions map[string]BuiltinFunc) *BuiltinExecutor {
	merged := make(map[string]BuiltinFunc, len(functions))
	for name, function := range functions {
		name = normalizeBuiltinName(name)
		if name != "" && function != nil {
			merged[name] = function
		}
	}
	return &BuiltinExecutor{functions: merged}
}

func (e *BuiltinExecutor) Execute(
	ctx context.Context,
	detail OperatorDetail,
	value any,
	extra map[string]any,
) (result ExecuteResult) {
	if ctx == nil {
		ctx = context.Background()
	}
	operatorID := operatorVersionID(detail)
	language := normalizeLanguage(detail.Language)
	defer func() {
		if recovered := recover(); recovered != nil {
			result = fallbackResult(
				operatorID,
				language,
				value,
				CodePanicRecovered,
				"builtin operator execution recovered from panic",
				fmt.Sprint(recovered),
			)
		}
	}()
	name := builtinName(detail)
	function, ok := e.functions[normalizeBuiltinName(name)]
	if !ok {
		return fallbackResult(
			operatorID,
			language,
			value,
			CodeBuiltinMissing,
			"builtin operator is not registered",
			name,
		)
	}
	output, err := function(ctx, BuiltinInput{
		OperatorID: operatorID,
		Name:       name,
		Value:      value,
		Config:     detail.Config,
		Context:    extra,
		Detail:     detail,
	})
	if err != nil {
		return fallbackResult(
			operatorID,
			language,
			value,
			CodeBuiltinExecuteFailed,
			"builtin operator execution failed",
			err.Error(),
		)
	}
	if err := ensureJSONSize(output, effectiveBuiltinMaxOutputBytes(detail)); err != nil {
		return fallbackResult(
			operatorID,
			language,
			value,
			CodeBuiltinResultInvalid,
			"builtin operator result is not json serializable or exceeds size limit",
			err.Error(),
		)
	}
	return successResult(operatorID, language, value, output)
}

func builtinName(detail OperatorDetail) string {
	for _, candidate := range []string{detail.Entry, detail.Code, detail.ID} {
		if text := strings.TrimSpace(candidate); text != "" {
			return text
		}
	}
	return ""
}

func normalizeBuiltinName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

type OmitValue struct{}

var omitValue = &OmitValue{}

func IsOmitValue(value any) bool {
	return value == omitValue
}

func builtinArrayJoin(ctx context.Context, input BuiltinInput) (any, error) {
	arr, ok := input.Value.([]any)
	if !ok {
		return input.Value, nil
	}
	extractField := configString(input.Config, "extractField", "extract_field")
	parts := make([]string, 0, len(arr))
	for _, elem := range arr {
		m, ok := elem.(map[string]any)
		if !ok {
			continue
		}
		if extractField == "" {
			continue
		}
		v, exists := m[extractField]
		if !exists || v == nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%v", v))
	}
	separator := configString(input.Config, "separator")
	if separator == "" {
		separator = " | "
	}
	return strings.Join(parts, separator), nil
}

func builtinOmitEmpty(ctx context.Context, input BuiltinInput) (any, error) {
	if isBuiltinZeroValue(input.Value) {
		return omitValue, nil
	}
	return input.Value, nil
}

func isBuiltinZeroValue(value any) bool {
	if value == nil {
		return true
	}
	switch typed := value.(type) {
	case string:
		return typed == ""
	case float64:
		return typed == 0
	case float32:
		return typed == 0
	case int:
		return typed == 0
	case int64:
		return typed == 0
	case bool:
		return !typed
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	default:
		return false
	}
}

func configString(config map[string]any, keys ...string) string {
	if config == nil {
		return ""
	}
	for _, key := range keys {
		if text, ok := config[key].(string); ok {
			return text
		}
	}
	return ""
}

func effectiveBuiltinMaxOutputBytes(detail OperatorDetail) int {
	if detail.Limits.MaxOutputBytes > 0 {
		return detail.Limits.MaxOutputBytes
	}
	return defaultMaxOutputBytes
}
