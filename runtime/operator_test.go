package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func bareOperator(source string) Operator {
	op := moneyOperator()
	op.SourceCode = source
	op.SourceHash = hashContent(source)
	return op
}

func requireOperatorFailure(t *testing.T, err error, code string) {
	t.Helper()
	var failure *operatorFailure
	if !errors.As(err, &failure) || failure.code != code {
		t.Fatalf("error=%T %v, want %s", err, err, code)
	}
}

func TestJSExecutorSuccessJavaScriptAndTypeScript(t *testing.T) {
	executor := NewJSExecutor(ExecutorConfig{})
	result, err := executor.Execute(nil, bareOperator(`function run(value) { return value * 2; }`), float64(4), nil)
	if err != nil || result != int64(8) {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	typescript := bareOperator(`interface Params { suffix?: string }
function run(value: number, params?: Params): string { return String(value) + (params?.suffix || ""); }`)
	typescript.Language = "typescript"
	result, err = executor.Execute(context.Background(), typescript, float64(4), map[string]any{"suffix": "!"})
	if err != nil || result != "4!" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func TestJSExecutorConfigurationDefaultsAndOverrides(t *testing.T) {
	defaults := NewJSExecutor(ExecutorConfig{})
	if defaults.cfg.Timeout != defaultJSTimeout || defaults.cfg.MaxInputBytes != defaultMaxInputBytes || defaults.cfg.MaxOutputBytes != defaultMaxOutBytes {
		t.Fatalf("defaults=%#v", defaults.cfg)
	}
	custom := NewJSExecutor(ExecutorConfig{Timeout: time.Second, MaxInputBytes: 12, MaxOutputBytes: 34})
	if custom.cfg.Timeout != time.Second || custom.cfg.MaxInputBytes != 12 || custom.cfg.MaxOutputBytes != 34 {
		t.Fatalf("custom=%#v", custom.cfg)
	}
}

func TestNilJSExecutorReturnsError(t *testing.T) {
	var executor *JSExecutor
	_, err := executor.Execute(context.Background(), bareOperator(`function run(v){return v}`), "x", nil)
	requireOperatorFailure(t, err, CodeOperatorExecutionFailed)
}

func TestJSExecutorRejectsDefinitionAndCompileFailures(t *testing.T) {
	executor := NewJSExecutor(ExecutorConfig{})
	tests := []struct {
		name string
		op   Operator
	}{
		{"language", func() Operator { op := bareOperator(`function run(v){return v}`); op.Language = "python"; return op }()},
		{"source", func() Operator { op := bareOperator(""); op.SourceCode = ""; return op }()},
		{"entry", func() Operator { op := bareOperator(`function run(v){return v}`); op.Entry = ""; return op }()},
		{"invalid typescript", func() Operator { op := bareOperator(`function run(value: ) {}`); op.Language = "ts"; return op }()},
		{"compile", bareOperator(`function run( {`)},
		{"entry missing", func() Operator { op := bareOperator(`var x = 1;`); op.Entry = "run"; return op }()},
		{"entry not function", bareOperator(`var run = 1;`)},
		{"execute", bareOperator(`function run() { throw new Error("boom"); }`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := map[string]any{"x": 1}
			result, err := executor.Execute(context.Background(), test.op, original, nil)
			if err == nil || !jsonEqual(result, original) {
				t.Fatalf("result=%#v error=%v", result, err)
			}
			requireOperatorFailure(t, err, CodeOperatorExecutionFailed)
		})
	}
}

func TestJSExecutorInputOutputLimitsAndJSON(t *testing.T) {
	inputExecutor := NewJSExecutor(ExecutorConfig{MaxInputBytes: 3})
	_, err := inputExecutor.Execute(context.Background(), bareOperator(`function run(v){return v}`), "long", nil)
	requireOperatorFailure(t, err, CodeOperatorInputValidationFailed)
	_, err = NewJSExecutor(ExecutorConfig{}).Execute(context.Background(), bareOperator(`function run(v){return v}`), make(chan int), nil)
	requireOperatorFailure(t, err, CodeOperatorInputValidationFailed)
	_, err = NewJSExecutor(ExecutorConfig{}).Execute(context.Background(), bareOperator(`function run(v){return v}`), 1, map[string]any{"bad": make(chan int)})
	requireOperatorFailure(t, err, CodeOperatorParamsValidationFailed)
	outputExecutor := NewJSExecutor(ExecutorConfig{MaxOutputBytes: 3})
	_, err = outputExecutor.Execute(context.Background(), bareOperator(`function run(){return "long"}`), 1, nil)
	requireOperatorFailure(t, err, CodeOperatorOutputTooLarge)
	_, err = NewJSExecutor(ExecutorConfig{}).Execute(context.Background(), bareOperator(`function run(){return function(){}}`), 1, nil)
	requireOperatorFailure(t, err, CodeOperatorOutputValidationFailed)
	_, err = NewJSExecutor(ExecutorConfig{}).Execute(context.Background(), bareOperator(`function run(){return Promise.resolve(1)}`), 1, nil)
	requireOperatorFailure(t, err, CodeOperatorOutputValidationFailed)
}

func TestJSExecutorRecoversPanic(t *testing.T) {
	executor := NewJSExecutor(ExecutorConfig{})
	executor.beforeRun = func() { panic("boom") }
	result, err := executor.Execute(context.Background(), bareOperator(`function run(v){return v}`), "original", nil)
	if result != "original" {
		t.Fatalf("result=%#v", result)
	}
	requireOperatorFailure(t, err, CodeOperatorExecutionFailed)
}

func TestJSExecutorTimeoutAndCancellation(t *testing.T) {
	loop := bareOperator(`function run(){while(true){}}`)
	_, err := NewJSExecutor(ExecutorConfig{Timeout: time.Millisecond}).Execute(context.Background(), loop, 1, nil)
	requireOperatorFailure(t, err, CodeOperatorTimeout)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = NewJSExecutor(ExecutorConfig{Timeout: time.Second}).Execute(ctx, loop, 1, nil)
	requireOperatorFailure(t, err, CodeOperatorExecutionFailed)
}

func TestOperatorFailureHelpers(t *testing.T) {
	base := errors.New("boom")
	failure := failOperator(CodeOperatorExecutionFailed, base)
	var typed *operatorFailure
	if !errors.As(failure, &typed) || !errors.Is(failure, base) || typed.Error() != "boom" || typed.Unwrap() != base {
		t.Fatalf("failure=%#v", failure)
	}
	var nilFailure *operatorFailure
	if nilFailure.Error() == "" || nilFailure.Unwrap() != nil {
		t.Fatal("nil failure helpers")
	}
	err := &ExecutionError{Code: CodeOperatorExecutionFailed}
	if !strings.Contains(err.Error(), CodeOperatorExecutionFailed) {
		t.Fatalf("error=%s", err)
	}
	err.Message, err.Cause = "failed", "cause"
	if !strings.Contains(err.Error(), "failed: cause") {
		t.Fatalf("error=%s", err)
	}
	var nilExecution *ExecutionError
	if nilExecution.Error() == "" {
		t.Fatal("nil execution error")
	}
}

func TestJSOperatorReportedErrorCodes(t *testing.T) {
	executor := NewJSExecutor(ExecutorConfig{})
	for _, test := range []struct {
		name string
		code string
		want string
	}{
		{"pick missing", CodePickNotFound, CodePickNotFound},
		{"pick duplicate", CodePickNotUnique, CodePickNotUnique},
		{"unknown", "OTHER", CodeOperatorExecutionFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			op := bareOperator(`function run(){throw {code:"` + test.code + `"};}`)
			_, err := executor.Execute(context.Background(), op, 1, nil)
			requireOperatorFailure(t, err, test.want)
		})
	}
	_, err := executor.Execute(context.Background(), bareOperator(`function run(){throw "bad";}`), 1, nil)
	requireOperatorFailure(t, err, CodeOperatorExecutionFailed)
	if reportedOperatorCode(errors.New("plain")) != CodeOperatorExecutionFailed {
		t.Fatal("plain error code")
	}
}
