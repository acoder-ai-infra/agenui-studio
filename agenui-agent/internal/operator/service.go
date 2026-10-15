package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
)

type DetailClient interface {
	GetOperator(ctx context.Context, operatorID uint64) (OperatorDetail, error)
}

type Service struct {
	client   DetailClient
	registry Registry
}

func NewService(client DetailClient, registry Registry) (*Service, error) {
	if client == nil {
		return nil, errors.New("operator service: detail client is required")
	}
	if registry.executors == nil {
		registry = DefaultRegistry()
	}
	return &Service{client: client, registry: registry}, nil
}

func MustNewService(client DetailClient, registry Registry) *Service {
	service, err := NewService(client, registry)
	if err != nil {
		panic(err)
	}
	return service
}

func (s *Service) Execute(ctx context.Context, req ExecuteRequest) (result ExecuteResult) {
	if ctx == nil {
		ctx = context.Background()
	}
	operatorID := req.OperatorID
	operatorIDText := strconv.FormatUint(operatorID, 10)
	original := req.Value
	defer func() {
		if recovered := recover(); recovered != nil {
			result = fallbackResult(
				operatorID,
				result.Language,
				original,
				CodePanicRecovered,
				"operator execution recovered from panic",
				fmt.Sprint(recovered),
			)
		}
	}()

	if operatorID == 0 {
		return fallbackResult(
			0,
			"",
			original,
			CodeOperatorIDEmpty,
			"operator id is empty",
			"",
		)
	}
	if s == nil || s.client == nil {
		return fallbackResult(
			operatorID,
			"",
			original,
			CodeOperatorDetailFetchFail,
			"operator service is unavailable",
			"",
		)
	}
	if err := ctx.Err(); err != nil {
		return fallbackResult(
			operatorID,
			"",
			original,
			CodeOperatorDetailFetchFail,
			"operator detail fetch canceled",
			err.Error(),
		)
	}

	detail, err := s.client.GetOperator(ctx, operatorID)
	if err != nil {
		code := CodeOperatorDetailFetchFail
		message := "operator detail fetch failed"
		if errors.Is(err, ErrOperatorNotFound) {
			code = CodeOperatorNotFound
			message = "operator detail not found"
		}
		return fallbackResult(operatorID, "", original, code, message, err.Error())
	}
	if detail.ID == "" {
		detail.ID = operatorIDText
	}
	if detail.OperatorVersionID == 0 {
		detail.OperatorVersionID = operatorID
	}
	detail.Language = normalizeLanguage(detail.Language)
	if !operatorStatusExecutable(detail.Status) {
		return fallbackResult(
			operatorID,
			detail.Language,
			original,
			CodeOperatorNotActive,
			"operator is not executable",
			"status="+detail.Status,
		)
	}
	normalized, expectedType, normalizeErr := normalizeOperatorInput(original, detail.InputSchema)
	if normalizeErr != nil {
		return fallbackResult(
			operatorID,
			detail.Language,
			original,
			CodeOperatorInputType,
			"operator input type does not match the published schema",
			fmt.Sprintf("expected=%s actual=%s", expectedType, operatorJSONType(original)),
		)
	}
	executor, ok := s.registry.Get(detail.Language)
	if !ok {
		return fallbackResult(
			operatorID,
			detail.Language,
			original,
			CodeUnsupportedLanguage,
			"operator language is not supported",
			detail.Language,
		)
	}
	return executor.Execute(ctx, detail, normalized, req.Context)
}

// normalizeOperatorInput enforces the published top-level input type before
// operator execution. Numeric strings are accepted only for number/integer
// schemas and only when the complete string is a valid JSON number. This keeps
// legacy model-authored dry-runs recoverable without coercing arbitrary display
// text such as "19.99元".
func normalizeOperatorInput(value any, schema json.RawMessage) (any, string, error) {
	expected := operatorSchemaType(schema)
	if expected == "" {
		return value, "", nil
	}
	// Some models double-encode tool values even after receiving a typed source
	// example. Decode that representation only when the selected operator's
	// published schema requires a non-string type, and only when the entire value
	// is canonical JSON matching that type.
	if text, ok := value.(string); ok && expected != "string" {
		if text == "" || text != strings.TrimSpace(text) {
			return nil, expected, errors.New("encoded JSON sample is not canonical")
		}
		decoder := json.NewDecoder(strings.NewReader(text))
		var decoded any
		if err := decoder.Decode(&decoded); err != nil {
			return nil, expected, err
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return nil, expected, errors.New("encoded JSON sample contains trailing data")
		}
		if !operatorValueMatchesType(decoded, expected) {
			return nil, expected, errors.New("encoded JSON sample does not match the published input type")
		}
		return decoded, expected, nil
	}
	if !operatorValueMatchesType(value, expected) {
		return nil, expected, errors.New("value does not match the published input type")
	}
	return value, expected, nil
}

func operatorSchemaType(schema json.RawMessage) string {
	var document struct {
		Type string `json:"type"`
	}
	if len(schema) == 0 || json.Unmarshal(schema, &document) != nil {
		return ""
	}
	switch document.Type {
	case "number", "integer", "string", "boolean", "array", "object", "null":
		return document.Type
	default:
		return ""
	}
}

func operatorValueMatchesType(value any, expected string) bool {
	if value == nil {
		return expected == "null"
	}
	switch expected {
	case "null":
		return false
	case "number":
		return isOperatorNumber(value)
	case "integer":
		if !isOperatorNumber(value) {
			return false
		}
		converted := reflect.ValueOf(value)
		switch converted.Kind() {
		case reflect.Float32, reflect.Float64:
			current := converted.Float()
			return !math.IsInf(current, 0) && !math.IsNaN(current) && math.Trunc(current) == current
		default:
			return true
		}
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "array":
		kind := reflect.ValueOf(value).Kind()
		return kind == reflect.Slice || kind == reflect.Array
	case "object":
		return reflect.ValueOf(value).Kind() == reflect.Map
	default:
		return true
	}
}

func isOperatorNumber(value any) bool {
	current := reflect.ValueOf(value)
	switch current.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	case reflect.Float32, reflect.Float64:
		number := current.Float()
		return !math.IsInf(number, 0) && !math.IsNaN(number)
	default:
		return false
	}
}

func operatorJSONType(value any) string {
	if value == nil {
		return "null"
	}
	if isOperatorNumber(value) {
		return "number"
	}
	switch reflect.ValueOf(value).Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	default:
		return reflect.TypeOf(value).String()
	}
}

func (s *Service) ExecuteValue(
	ctx context.Context,
	operatorID uint64,
	value any,
) ExecuteResult {
	return s.Execute(ctx, ExecuteRequest{OperatorID: operatorID, Value: value})
}

func operatorStatusExecutable(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "active", "published", "enabled", "online":
		return true
	default:
		return false
	}
}
