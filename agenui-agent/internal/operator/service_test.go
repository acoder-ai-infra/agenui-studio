package operator

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestNormalizeOperatorInputUsesPublishedTopLevelType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   any
		schema  string
		want    any
		wantErr bool
	}{
		{name: "number stays number", value: float64(6800), schema: `{"type":"number"}`, want: float64(6800)},
		{name: "null stays null", value: nil, schema: `{"type":"null"}`, want: nil},
		{name: "encoded null", value: "null", schema: `{"type":"null"}`, want: nil},
		{name: "null schema rejects number", value: float64(1), schema: `{"type":"null"}`, wantErr: true},
		{name: "null schema rejects encoded number", value: "1", schema: `{"type":"null"}`, wantErr: true},
		{name: "null schema rejects boolean", value: true, schema: `{"type":"null"}`, wantErr: true},
		{name: "null schema rejects array", value: []any{}, schema: `{"type":"null"}`, wantErr: true},
		{name: "null schema rejects object", value: map[string]any{}, schema: `{"type":"null"}`, wantErr: true},
		{name: "strict numeric string", value: "1999", schema: `{"type":"number"}`, want: float64(1999)},
		{name: "distance numeric string", value: "1500", schema: `{"type":"number"}`, want: float64(1500)},
		{name: "display string rejected", value: "19.99元", schema: `{"type":"number"}`, wantErr: true},
		{name: "whitespace rejected", value: " 1999 ", schema: `{"type":"number"}`, wantErr: true},
		{name: "fraction rejected for integer", value: "1.5", schema: `{"type":"integer"}`, wantErr: true},
		{name: "ordinary string preserved", value: "open", schema: `{"type":"string"}`, want: "open"},
		{name: "strict encoded array", value: `[{"title":"示例商品","id":"1"}]`, schema: `{"type":"array"}`, want: []any{map[string]any{"title": "示例商品", "id": "1"}}},
		{name: "non json string rejected for array", value: "not-an-array", schema: `{"type":"array"}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, _, err := normalizeOperatorInput(test.value, json.RawMessage(test.schema))
			if test.wantErr {
				if err == nil {
					t.Fatalf("normalizeOperatorInput(%#v) unexpectedly succeeded: %#v", test.value, got)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("normalizeOperatorInput(%#v) = %#v, %v; want %#v", test.value, got, err, test.want)
			}
		})
	}
}

type detailClientFunc func(context.Context, uint64) (OperatorDetail, error)

func (f detailClientFunc) GetOperator(
	ctx context.Context,
	operatorID uint64,
) (OperatorDetail, error) {
	return f(ctx, operatorID)
}

type executorFunc func(context.Context, OperatorDetail, any, map[string]any) ExecuteResult

func (f executorFunc) Execute(
	ctx context.Context,
	detail OperatorDetail,
	value any,
	extra map[string]any,
) ExecuteResult {
	return f(ctx, detail, value, extra)
}

func TestServiceExecuteSuccess(t *testing.T) {
	registry := NewRegistry()
	registry.Register("js", executorFunc(func(
		ctx context.Context,
		detail OperatorDetail,
		value any,
		extra map[string]any,
	) ExecuteResult {
		return successResult(detail.OperatorVersionID, detail.Language, value, "formatted")
	}))
	service := MustNewService(detailClientFunc(func(
		ctx context.Context,
		operatorID uint64,
	) (OperatorDetail, error) {
		operatorIDText := "101"
		return OperatorDetail{
			ID:                operatorIDText,
			OperatorVersionID: operatorID,
			Status:            "published",
			Language:          "js",
			Code:              "unused",
		}, nil
	}), registry)

	result := service.Execute(context.Background(), ExecuteRequest{
		OperatorID: 101,
		Value:      "raw",
	})
	if !result.Applied || result.Value != "formatted" || result.Error != nil {
		t.Fatalf("expected applied formatted result, got %+v", result)
	}
}

func TestServiceExecuteJSOperatorEndToEnd(t *testing.T) {
	service := MustNewService(detailClientFunc(func(
		ctx context.Context,
		operatorID uint64,
	) (OperatorDetail, error) {
		if operatorID != 9 {
			t.Fatalf("expected operator id 9, got %d", operatorID)
		}
		return OperatorDetail{
			ID:                "9",
			OperatorVersionID: operatorID,
			Status:            "published",
			Language:          "javascript",
			Code:              `function transform(value) { return value + "___"; }`,
		}, nil
	}), DefaultRegistry())

	result := service.Execute(context.Background(), ExecuteRequest{
		OperatorID: 9,
		Value:      "input",
	})
	if result.Error != nil || !result.Applied {
		t.Fatalf("expected applied result, got %+v", result)
	}
	if result.OperatorID != 9 {
		t.Fatalf("expected result operator id 9, got %d", result.OperatorID)
	}
	if result.OriginalValue != "input" || result.Value != "input___" {
		t.Fatalf("expected input___ result, got original=%#v value=%#v", result.OriginalValue, result.Value)
	}
}

func TestServiceExecuteFallbacks(t *testing.T) {
	tests := []struct {
		name     string
		request  ExecuteRequest
		client   DetailClient
		registry Registry
		wantCode string
	}{
		{
			name:     "empty operator id",
			request:  ExecuteRequest{Value: "raw"},
			client:   detailClientFunc(func(context.Context, uint64) (OperatorDetail, error) { return OperatorDetail{}, nil }),
			registry: NewRegistry(),
			wantCode: CodeOperatorIDEmpty,
		},
		{
			name:    "detail not found",
			request: ExecuteRequest{OperatorID: 404, Value: "raw"},
			client: detailClientFunc(func(context.Context, uint64) (OperatorDetail, error) {
				return OperatorDetail{}, ErrOperatorNotFound
			}),
			registry: NewRegistry(),
			wantCode: CodeOperatorNotFound,
		},
		{
			name:    "detail fetch failed",
			request: ExecuteRequest{OperatorID: 500, Value: "raw"},
			client: detailClientFunc(func(context.Context, uint64) (OperatorDetail, error) {
				return OperatorDetail{}, errors.New("boom")
			}),
			registry: NewRegistry(),
			wantCode: CodeOperatorDetailFetchFail,
		},
		{
			name:    "inactive status",
			request: ExecuteRequest{OperatorID: 102, Value: "raw"},
			client: detailClientFunc(func(context.Context, uint64) (OperatorDetail, error) {
				return OperatorDetail{ID: "102", OperatorVersionID: 102, Language: "js", Status: "draft", Code: "x"}, nil
			}),
			registry: DefaultRegistry(),
			wantCode: CodeOperatorNotActive,
		},
		{
			name:    "unsupported language",
			request: ExecuteRequest{OperatorID: 103, Value: "raw"},
			client: detailClientFunc(func(context.Context, uint64) (OperatorDetail, error) {
				return OperatorDetail{ID: "103", OperatorVersionID: 103, Language: "python", Status: "active", Code: "x"}, nil
			}),
			registry: DefaultRegistry(),
			wantCode: CodeUnsupportedLanguage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := MustNewService(tt.client, tt.registry)
			result := service.Execute(context.Background(), tt.request)
			if result.Applied {
				t.Fatalf("expected fallback, got %+v", result)
			}
			if result.Value != tt.request.Value {
				t.Fatalf("expected original value fallback, got %#v", result.Value)
			}
			if result.Error == nil || result.Error.Code != tt.wantCode {
				t.Fatalf("expected code %s, got %+v", tt.wantCode, result.Error)
			}
		})
	}
}

func TestServiceExecuteRecoversPanic(t *testing.T) {
	service := MustNewService(detailClientFunc(func(
		context.Context,
		uint64,
	) (OperatorDetail, error) {
		panic("detail client panic")
	}), DefaultRegistry())

	result := service.Execute(context.Background(), ExecuteRequest{
		OperatorID: 104,
		Value:      "raw",
	})
	if result.Applied || result.Value != "raw" {
		t.Fatalf("expected original fallback, got %+v", result)
	}
	if result.Error == nil || result.Error.Code != CodePanicRecovered {
		t.Fatalf("expected panic recovery, got %+v", result.Error)
	}
}
