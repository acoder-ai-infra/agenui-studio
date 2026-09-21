package operator

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestGojaExecutorExecuteSuccess(t *testing.T) {
	executor := NewGojaExecutor(GojaExecutorConfig{})
	result := executor.Execute(context.Background(), OperatorDetail{
		ID: "format-price", Language: "js", Code: `
function transform(value, ctx) {
  return ctx.prefix + value.amount.toFixed(2);
}`,
	}, map[string]any{"amount": 12.5}, map[string]any{"prefix": "￥"})

	if !result.Applied || result.Error != nil {
		t.Fatalf("expected success, got %+v", result)
	}
	if result.Value != "￥12.50" {
		t.Fatalf("expected formatted value, got %#v", result.Value)
	}
}

func TestGojaExecutorExecuteExpressionFunction(t *testing.T) {
	executor := NewGojaExecutor(GojaExecutorConfig{})
	result := executor.Execute(context.Background(), OperatorDetail{
		ID:       "suffix",
		Language: "javascript",
		Code:     `(function(value) { return value + "!"; })`,
	}, "ok", nil)

	if !result.Applied || result.Value != "ok!" {
		t.Fatalf("expected expression function success, got %+v", result)
	}
}

func TestGojaExecutorTranspilesTypeScriptBeforeExecution(t *testing.T) {
	t.Parallel()

	executor := NewGojaExecutor(GojaExecutorConfig{})
	result := executor.Execute(context.Background(), OperatorDetail{
		ID:       "204",
		Language: "typescript",
		Entry:    "run",
		Code: `type Distance = { meters: number };
function run(value: Distance): string {
  return (value.meters / 1000).toFixed(1) + "km";
}`,
	}, map[string]any{"meters": 1250}, nil)

	if !result.Applied || result.Error != nil || result.Value != "1.3km" {
		t.Fatalf("expected transpiled TypeScript success, got %+v", result)
	}
}

func TestGojaExecutorRejectsInvalidTypeScript(t *testing.T) {
	t.Parallel()

	result := NewGojaExecutor(GojaExecutorConfig{}).Execute(
		context.Background(),
		OperatorDetail{ID: "205", Language: "ts", Entry: "run", Code: `function run(value: {`},
		"raw",
		nil,
	)
	if result.Applied || result.Value != "raw" || result.Error == nil || result.Error.Code != CodeJSCompileFailed {
		t.Fatalf("expected TypeScript transform failure, got %+v", result)
	}
}

func TestGojaExecutorRemovesAmbientAndNondeterministicCapabilities(t *testing.T) {
	t.Parallel()

	executor := NewGojaExecutor(GojaExecutorConfig{})
	result := executor.Execute(context.Background(), OperatorDetail{
		ID: "capabilities", Language: "javascript", Code: `
function transform(value, ctx) {
  return {
    date: typeof Date,
    random: typeof Math.random,
    fetch: typeof fetch,
    require: typeof require,
    process: typeof process,
    contextKeys: Object.keys(ctx || {}).length
  };
}`,
	}, map[string]any{"value": 1}, nil)

	if !result.Applied || result.Error != nil {
		t.Fatalf("expected success, got %+v", result)
	}
	got, ok := result.Value.(map[string]any)
	if !ok {
		t.Fatalf("result value = %#v", result.Value)
	}
	for _, name := range []string{"date", "random", "fetch", "require", "process"} {
		if got[name] != "undefined" {
			t.Fatalf("%s capability = %#v", name, got[name])
		}
	}
	if got["contextKeys"] != int64(0) {
		t.Fatalf("context keys = %#v", got["contextKeys"])
	}
}

func TestGojaExecutorMaterializesObjectsWithStableKeyOrder(t *testing.T) {
	executor := NewGojaExecutor(GojaExecutorConfig{})
	detail := OperatorDetail{
		ID: "stable-keys", Language: "javascript", Code: `
function transform(value) {
  return Object.keys(value).join(",");
}`,
	}
	value := map[string]any{"z": 1, "a": 2, "middle": 3}
	for index := 0; index < 100; index++ {
		result := executor.Execute(context.Background(), detail, value, nil)
		if !result.Applied || result.Error != nil {
			t.Fatalf("iteration %d: expected success, got %+v", index, result)
		}
		if result.Value != "a,middle,z" {
			t.Fatalf("iteration %d: key order = %#v", index, result.Value)
		}
	}
}

func TestGojaExecutorExecuteFallbacks(t *testing.T) {
	tests := []struct {
		name     string
		detail   OperatorDetail
		wantCode string
	}{
		{
			name: "empty code",
			detail: OperatorDetail{
				ID: "op", Language: "js",
			},
			wantCode: CodeOperatorCodeEmpty,
		},
		{
			name: "compile failed",
			detail: OperatorDetail{
				ID: "op", Language: "js", Code: `function {`,
			},
			wantCode: CodeJSCompileFailed,
		},
		{
			name: "entry missing",
			detail: OperatorDetail{
				ID: "op", Language: "js", Code: `var x = 1;`,
			},
			wantCode: CodeJSEntryMissing,
		},
		{
			name: "execute failed",
			detail: OperatorDetail{
				ID: "op", Language: "js", Code: `function transform() { throw new Error("bad"); }`,
			},
			wantCode: CodeJSExecuteFailed,
		},
		{
			name: "invalid result",
			detail: OperatorDetail{
				ID: "op", Language: "js", Code: `function transform() { return function() {}; }`,
			},
			wantCode: CodeJSResultInvalid,
		},
	}

	executor := NewGojaExecutor(GojaExecutorConfig{})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := executor.Execute(context.Background(), tt.detail, "raw", nil)
			if result.Applied || result.Value != "raw" {
				t.Fatalf("expected original fallback, got %+v", result)
			}
			if result.Error == nil || result.Error.Code != tt.wantCode {
				t.Fatalf("expected code %s, got %+v", tt.wantCode, result.Error)
			}
		})
	}
}

func TestGojaExecutorRejectsPromiseResults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code string
	}{
		{name: "fulfilled", code: `function transform() { return Promise.resolve({ok: true}); }`},
		{name: "rejected", code: `function transform() { return Promise.reject(new Error("bad")); }`},
		{name: "pending", code: `function transform() { return new Promise(function() {}); }`},
	}
	executor := NewGojaExecutor(GojaExecutorConfig{})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := executor.Execute(context.Background(), OperatorDetail{
				ID: "promise-" + test.name, Language: "javascript", Code: test.code,
			}, map[string]any{"input": "unchanged"}, nil)
			if result.Applied || result.Error == nil || result.Error.Code != CodeJSResultInvalid {
				t.Fatalf("expected synchronous-result rejection, got %+v", result)
			}
		})
	}
}

func TestGojaExecutorExecuteTimeout(t *testing.T) {
	executor := NewGojaExecutor(GojaExecutorConfig{
		DefaultTimeout: 10 * time.Millisecond,
		MaxTimeout:     20 * time.Millisecond,
	})
	result := executor.Execute(context.Background(), OperatorDetail{
		ID: "loop", Language: "js", Code: `function transform() { while (true) {} }`,
	}, "raw", nil)

	if result.Applied || result.Value != "raw" {
		t.Fatalf("expected timeout fallback, got %+v", result)
	}
	if result.Error == nil || result.Error.Code != CodeJSExecuteTimeout {
		t.Fatalf("expected timeout error, got %+v", result.Error)
	}
}

func TestGojaExecutorConcurrentExecute(t *testing.T) {
	executor := NewGojaExecutor(GojaExecutorConfig{})
	detail := OperatorDetail{
		ID:       "plus-one",
		Language: "js",
		Code:     `function transform(value) { return value + 1; }`,
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := executor.Execute(context.Background(), detail, i, nil)
			if !result.Applied {
				t.Errorf("expected success for %d, got %+v", i, result)
				return
			}
			if got := int(result.Value.(int64)); got != i+1 {
				t.Errorf("expected %d, got %d", i+1, got)
			}
		}()
	}
	wg.Wait()
}
