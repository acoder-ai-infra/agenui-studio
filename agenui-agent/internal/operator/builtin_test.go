package operator

import (
	"context"
	"reflect"
	"testing"
)

func TestBuiltinExecutorSurfaceFetchArrayJoin(t *testing.T) {
	executor := SurfaceFetchBuiltinExecutor()
	result := executor.Execute(context.Background(), OperatorDetail{
		ID:       "array_join",
		Language: "builtin",
		Config: map[string]any{
			"extractField": "text",
			"separator":    "|",
		},
	}, []any{
		map[string]any{"text": "4.9分"},
		map[string]any{"text": "川菜"},
		map[string]any{"other": "skip"},
		map[string]any{"text": "¥98/人"},
	}, nil)
	if !result.Applied || result.Error != nil {
		t.Fatalf("expected success, got %+v", result)
	}
	if result.Value != "4.9分|川菜|¥98/人" {
		t.Fatalf("value = %#v", result.Value)
	}
}

func TestBuiltinExecutorSurfaceFetchArrayJoinPassthrough(t *testing.T) {
	executor := SurfaceFetchBuiltinExecutor()
	result := executor.Execute(context.Background(), OperatorDetail{
		ID:       "array_join",
		Language: "builtin",
		Config:   map[string]any{"extractField": "text"},
	}, "not-array", nil)
	if !result.Applied || result.Value != "not-array" {
		t.Fatalf("expected passthrough, got %+v", result)
	}
}

func TestBuiltinExecutorSurfaceFetchOmitEmpty(t *testing.T) {
	executor := SurfaceFetchBuiltinExecutor()
	for _, value := range []any{nil, "", 0, float64(0), false, []any{}, map[string]any{}} {
		result := executor.Execute(context.Background(), OperatorDetail{
			ID: "omit_empty", Language: "builtin",
		}, value, nil)
		if !result.Applied || !IsOmitValue(result.Value) {
			t.Fatalf("expected omit sentinel for %#v, got %+v", value, result)
		}
	}
	result := executor.Execute(context.Background(), OperatorDetail{
		ID: "omit_empty", Language: "builtin",
	}, "hello", nil)
	if !result.Applied || result.Value != "hello" {
		t.Fatalf("expected non-empty passthrough, got %+v", result)
	}
}

func TestBuiltinExecutorUsesEntryThenCodeThenID(t *testing.T) {
	executor := NewBuiltinExecutor(map[string]BuiltinFunc{
		"custom": func(ctx context.Context, input BuiltinInput) (any, error) {
			return input.Name + ":" + input.Value.(string), nil
		},
	})
	for _, detail := range []OperatorDetail{
		{ID: "ignored", Language: "builtin", Entry: "custom", Code: "none"},
		{ID: "ignored", Language: "builtin", Code: "custom"},
		{ID: "custom", Language: "builtin"},
	} {
		result := executor.Execute(context.Background(), detail, "ok", nil)
		if !result.Applied || result.Value != "custom:ok" {
			t.Fatalf("unexpected result for %+v: %+v", detail, result)
		}
	}
}

func TestBuiltinExecutorFallbacks(t *testing.T) {
	tests := []struct {
		name     string
		detail   OperatorDetail
		value    any
		wantCode string
	}{
		{
			name: "missing builtin",
			detail: OperatorDetail{
				ID: "unknown", Language: "builtin",
			},
			value:    "raw",
			wantCode: CodeBuiltinMissing,
		},
		{
			name: "result invalid",
			detail: OperatorDetail{
				ID: "array_join", Language: "builtin",
				Config: map[string]any{"extractField": "text"},
				Limits: Limits{MaxOutputBytes: 4},
			},
			value: []any{
				map[string]any{"text": "too-long"},
			},
			wantCode: CodeBuiltinResultInvalid,
		},
	}

	executor := SurfaceFetchBuiltinExecutor()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := executor.Execute(context.Background(), tt.detail, tt.value, nil)
			if result.Applied || !reflect.DeepEqual(result.Value, tt.value) {
				t.Fatalf("expected original fallback, got %+v", result)
			}
			if result.Error == nil || result.Error.Code != tt.wantCode {
				t.Fatalf("expected code %s, got %+v", tt.wantCode, result.Error)
			}
		})
	}
}

func TestServiceExecuteBuiltinFromDefaultRegistry(t *testing.T) {
	service := MustNewService(detailClientFunc(func(
		ctx context.Context,
		operatorID uint64,
	) (OperatorDetail, error) {
		return OperatorDetail{
			ID:                "205",
			OperatorVersionID: operatorID,
			Status:            "published",
			Language:          "builtin",
			Entry:             "array_join",
			Config: map[string]any{
				"extractField": "name",
				"separator":    " - ",
			},
		}, nil
	}), DefaultRegistry())

	result := service.ExecuteValue(context.Background(), 205, []any{
		map[string]any{"name": "A"},
		map[string]any{"name": "B"},
	})
	if !result.Applied || result.Value != "A - B" {
		t.Fatalf("expected builtin array_join value, got %+v", result)
	}
}
