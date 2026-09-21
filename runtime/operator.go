package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dop251/goja"
	"github.com/evanw/esbuild/pkg/api"
)

const (
	defaultJSTimeout     = 200 * time.Millisecond
	defaultMaxInputBytes = 64 * 1024
	defaultMaxOutBytes   = 64 * 1024
)

// ExecutorConfig tunes the embedded JS operator runtime. Zero values apply
// safe defaults. The open-source runtime executes operators in-process; hosts
// with high-QPS isolation requirements should run this library inside a
// dedicated worker process.
type ExecutorConfig struct {
	Timeout        time.Duration
	MaxInputBytes  int
	MaxOutputBytes int
}

// JSExecutor executes versioned JavaScript or TypeScript operators. Each call
// builds a fresh VM: operators are untrusted user content and must not share
// state. TypeScript is translated to JavaScript before Goja executes it.
type JSExecutor struct {
	cfg       ExecutorConfig
	beforeRun func()
}

func NewJSExecutor(cfg ExecutorConfig) *JSExecutor {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultJSTimeout
	}
	if cfg.MaxInputBytes <= 0 {
		cfg.MaxInputBytes = defaultMaxInputBytes
	}
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = defaultMaxOutBytes
	}
	return &JSExecutor{cfg: cfg}
}

// Execute runs op.Entry(value, params) from op.SourceCode and returns the result.
// The total semantics apply: on any failure the original value is returned
// together with a structured error, so one bad operator never breaks the
// whole card.
func (e *JSExecutor) Execute(ctx context.Context, op Operator, value any, params map[string]any) (any, error) {
	if e == nil {
		return value, failOperator(CodeOperatorExecutionFailed, fmt.Errorf("runtime: JS executor is unavailable"))
	}
	if ctx == nil {
		ctx = context.Background()
	}
	operatorLabel := fmt.Sprintf("%d", op.OperatorVersionID)
	if op.Language != "" && op.Language != "javascript" && op.Language != "js" && op.Language != "typescript" && op.Language != "ts" {
		return value, failOperator(CodeOperatorExecutionFailed, fmt.Errorf("runtime: operator %s: unsupported language %q", operatorLabel, op.Language))
	}
	if op.SourceCode == "" || op.Entry == "" {
		return value, failOperator(CodeOperatorExecutionFailed, fmt.Errorf("runtime: operator %s: sourceCode and entry are required", operatorLabel))
	}
	code := op.SourceCode
	if op.Language == "typescript" || op.Language == "ts" {
		transformed := api.Transform(code, api.TransformOptions{
			Loader:     api.LoaderTS,
			Target:     api.ES2015,
			Sourcefile: "operator-" + operatorLabel + ".ts",
		})
		if len(transformed.Errors) > 0 {
			return value, failOperator(CodeOperatorExecutionFailed, fmt.Errorf("runtime: operator %s TypeScript transform: %s", operatorLabel, transformed.Errors[0].Text))
		}
		code = string(transformed.Code)
	}
	rawInput, err := json.Marshal(value)
	if err != nil {
		return value, failOperator(CodeOperatorInputValidationFailed, fmt.Errorf("runtime: operator %s: input is not JSON: %w", operatorLabel, err))
	}
	if len(rawInput) > e.cfg.MaxInputBytes {
		return value, failOperator(CodeOperatorInputValidationFailed, fmt.Errorf("runtime: operator %s: input exceeds %d bytes", operatorLabel, e.cfg.MaxInputBytes))
	}

	vm := goja.New()
	_ = vm.Set("value", value) // JSON-compatible values are assignable on a fresh Runtime.
	if params == nil {
		params = map[string]any{}
	}
	if _, err := json.Marshal(params); err != nil {
		return value, failOperator(CodeOperatorParamsValidationFailed, err)
	}
	_ = vm.Set("params", params) // JSON-compatible values are assignable on a fresh Runtime.

	done := make(chan struct{})
	var result any
	var runErr error
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				runErr = fmt.Errorf("runtime: operator %s panicked: %v", operatorLabel, r)
			}
		}()
		if e.beforeRun != nil {
			e.beforeRun()
		}
		if _, err := vm.RunString(code); err != nil {
			runErr = fmt.Errorf("runtime: operator %s compile: %w", operatorLabel, err)
			return
		}
		entry := vm.Get(op.Entry)
		if entry == nil || goja.IsUndefined(entry) || goja.IsNull(entry) {
			runErr = fmt.Errorf("runtime: operator %s: entry %q not defined", operatorLabel, op.Entry)
			return
		}
		call, ok := goja.AssertFunction(entry)
		if !ok {
			runErr = fmt.Errorf("runtime: operator %s: entry %q is not a function", operatorLabel, op.Entry)
			return
		}
		out, err := call(goja.Undefined(), vm.Get("value"), vm.Get("params"))
		if err != nil {
			runErr = failOperator(reportedOperatorCode(err), fmt.Errorf("runtime: operator %s execute: %w", operatorLabel, err))
			return
		}
		result = out.Export()
	}()

	select {
	case <-done:
	case <-time.After(e.cfg.Timeout):
		vm.Interrupt("timeout")
		return value, failOperator(CodeOperatorTimeout, fmt.Errorf("runtime: operator %s timed out after %s", operatorLabel, e.cfg.Timeout))
	case <-ctx.Done():
		vm.Interrupt("cancelled")
		return value, failOperator(CodeOperatorExecutionFailed, ctx.Err())
	}
	if runErr != nil {
		var failure *operatorFailure
		if errors.As(runErr, &failure) {
			return value, runErr
		}
		return value, failOperator(CodeOperatorExecutionFailed, runErr)
	}
	if _, promise := result.(*goja.Promise); promise {
		return value, failOperator(CodeOperatorOutputValidationFailed, fmt.Errorf("runtime: operator %s: promise output is unsupported", operatorLabel))
	}
	rawOutput, err := json.Marshal(result)
	if err != nil {
		return value, failOperator(CodeOperatorOutputValidationFailed, fmt.Errorf("runtime: operator %s: output is not JSON: %w", operatorLabel, err))
	}
	if len(rawOutput) > e.cfg.MaxOutputBytes {
		return value, failOperator(CodeOperatorOutputTooLarge, fmt.Errorf("runtime: operator %s: output exceeds %d bytes", operatorLabel, e.cfg.MaxOutputBytes))
	}
	return result, nil
}

func reportedOperatorCode(err error) string {
	var exception *goja.Exception
	if !errors.As(err, &exception) {
		return CodeOperatorExecutionFailed
	}
	object, ok := exception.Value().Export().(map[string]any)
	if !ok {
		return CodeOperatorExecutionFailed
	}
	code, _ := object["code"].(string)
	switch code {
	case CodePickNotFound, CodePickNotUnique:
		return code
	default:
		return CodeOperatorExecutionFailed
	}
}
