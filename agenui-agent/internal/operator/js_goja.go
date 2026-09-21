package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/evanw/esbuild/pkg/api"
)

type GojaExecutorConfig struct {
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
	MaxInputBytes  int
	MaxOutputBytes int
}

type GojaExecutor struct {
	defaultTimeout time.Duration
	maxTimeout     time.Duration
	maxInputBytes  int
	maxOutputBytes int
}

func NewGojaExecutor(config GojaExecutorConfig) *GojaExecutor {
	defaultTimeout := config.DefaultTimeout
	if defaultTimeout <= 0 {
		defaultTimeout = defaultJSTimeout
	}
	maxTimeout := config.MaxTimeout
	if maxTimeout <= 0 {
		maxTimeout = maxJSTimeout
	}
	maxInputBytes := config.MaxInputBytes
	if maxInputBytes <= 0 {
		maxInputBytes = defaultMaxInputBytes
	}
	maxOutputBytes := config.MaxOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultMaxOutputBytes
	}
	return &GojaExecutor{
		defaultTimeout: defaultTimeout,
		maxTimeout:     maxTimeout,
		maxInputBytes:  maxInputBytes,
		maxOutputBytes: maxOutputBytes,
	}
}

func (e *GojaExecutor) Execute(
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
				"js operator execution recovered from panic",
				fmt.Sprint(recovered),
			)
		}
	}()
	code := strings.TrimSpace(detail.Code)
	if code == "" {
		return fallbackResult(
			operatorID,
			language,
			value,
			CodeOperatorCodeEmpty,
			"operator code is empty",
			"",
		)
	}
	if language == "typescript" || language == "ts" {
		transformed := api.Transform(code, api.TransformOptions{
			Loader:     api.LoaderTS,
			Target:     api.ES2015,
			Sourcefile: fmt.Sprintf("operator-%d.ts", operatorID),
		})
		if len(transformed.Errors) > 0 {
			return fallbackResult(
				operatorID,
				language,
				value,
				CodeJSCompileFailed,
				"typescript operator transpilation failed",
				transformed.Errors[0].Text,
			)
		}
		code = string(transformed.Code)
	}
	if err := ensureJSONSize(value, effectiveMaxInputBytes(e, detail)); err != nil {
		return fallbackResult(
			operatorID,
			language,
			value,
			CodeJSResultInvalid,
			"operator input is not json serializable or exceeds size limit",
			err.Error(),
		)
	}
	valueJSON, err := json.Marshal(value)
	if err != nil {
		return fallbackResult(
			operatorID,
			language,
			value,
			CodeJSResultInvalid,
			"operator input is not json serializable",
			err.Error(),
		)
	}
	extraJSON, err := json.Marshal(extra)
	if err != nil {
		return fallbackResult(
			operatorID,
			language,
			value,
			CodeJSResultInvalid,
			"operator context is not json serializable",
			err.Error(),
		)
	}

	runtime := goja.New()
	if err := hardenGojaRuntime(runtime); err != nil {
		return fallbackResult(
			operatorID,
			language,
			value,
			CodeJSCompileFailed,
			"js operator runtime initialization failed",
			"runtime hardening failed",
		)
	}
	stopContext := context.AfterFunc(ctx, func() {
		runtime.Interrupt(errContextCanceled)
	})
	defer stopContext()

	timeout := effectiveTimeout(e, detail)
	timer := time.AfterFunc(timeout, func() {
		runtime.Interrupt(errJSTimeout)
	})
	defer timer.Stop()

	scriptValue, err := runtime.RunString(code)
	if err != nil {
		return fallbackForJSError(operatorID, language, value, CodeJSCompileFailed, err)
	}
	callable, ok := goja.AssertFunction(scriptValue)
	if !ok {
		entry := strings.TrimSpace(detail.Entry)
		if entry == "" {
			entry = "transform"
		}
		callable, ok = goja.AssertFunction(runtime.Get(entry))
		if !ok {
			return fallbackResult(
				operatorID,
				language,
				value,
				CodeJSEntryMissing,
				"js operator entry function is missing",
				entry,
			)
		}
	}

	valueArgument, err := parseJSONInRuntime(runtime, valueJSON)
	if err != nil {
		return fallbackResult(
			operatorID,
			language,
			value,
			CodeJSResultInvalid,
			"operator input cannot be materialized",
			"runtime JSON.parse failed",
		)
	}
	extraArgument, err := parseJSONInRuntime(runtime, extraJSON)
	if err != nil {
		return fallbackResult(
			operatorID,
			language,
			value,
			CodeJSResultInvalid,
			"operator context cannot be materialized",
			"runtime JSON.parse failed",
		)
	}
	output, err := callable(
		goja.Undefined(),
		valueArgument,
		extraArgument,
	)
	if err != nil {
		return fallbackForJSError(operatorID, language, value, CodeJSExecuteFailed, err)
	}
	exported := output.Export()
	// The worker contract is deliberately synchronous. Accepting a Promise here
	// would either serialize it as an empty object or make success depend on an
	// event loop that this isolated runtime does not expose. Reject every Promise
	// state so a published operator cannot masquerade as a completed transform.
	if _, isPromise := exported.(*goja.Promise); isPromise {
		return fallbackResult(
			operatorID,
			language,
			value,
			CodeJSResultInvalid,
			"operator result must be synchronous json",
			"promise results are unsupported",
		)
	}
	if err := ensureJSONSize(exported, effectiveMaxOutputBytes(e, detail)); err != nil {
		return fallbackResult(
			operatorID,
			language,
			value,
			CodeJSResultInvalid,
			"operator result is not json serializable or exceeds size limit",
			err.Error(),
		)
	}
	return successResult(operatorID, language, value, exported)
}

// parseJSONInRuntime avoids exposing Go map iteration order to JavaScript.
// json.Marshal produces a stable key order and JSON.parse creates an ordinary
// ECMAScript object with that insertion order, so exact retries observe the
// same Object.keys/for..in/JSON.stringify behavior.
func parseJSONInRuntime(runtime *goja.Runtime, raw []byte) (goja.Value, error) {
	if runtime == nil || !json.Valid(raw) {
		return nil, errors.New("invalid runtime JSON")
	}
	jsonObject := runtime.Get("JSON").ToObject(runtime)
	if jsonObject == nil {
		return nil, errors.New("goja JSON object is unavailable")
	}
	parse, ok := goja.AssertFunction(jsonObject.Get("parse"))
	if !ok {
		return nil, errors.New("goja JSON.parse is unavailable")
	}
	return parse(goja.Undefined(), runtime.ToValue(string(raw)))
}

// hardenGojaRuntime removes ambient capabilities that would make a published
// transform non-replayable or able to discover process/network facilities.
// Operators receive only the explicit value and ctx arguments.
func hardenGojaRuntime(runtime *goja.Runtime) error {
	if runtime == nil {
		return errors.New("goja runtime is nil")
	}
	for _, name := range []string{
		"Date", "fetch", "XMLHttpRequest", "WebSocket", "require", "process",
	} {
		if err := runtime.Set(name, goja.Undefined()); err != nil {
			return err
		}
	}
	mathObject := runtime.Get("Math").ToObject(runtime)
	if mathObject == nil {
		return errors.New("goja Math object is unavailable")
	}
	return mathObject.Set("random", goja.Undefined())
}

func fallbackForJSError(
	operatorID uint64,
	language string,
	value any,
	defaultCode string,
	err error,
) ExecuteResult {
	code := defaultCode
	message := "js operator execution failed"
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		switch {
		case errors.Is(interrupted.Unwrap(), errJSTimeout):
			code = CodeJSExecuteTimeout
			message = "js operator execution timeout"
		case errors.Is(interrupted.Unwrap(), errContextCanceled):
			code = CodeOperatorDetailFetchFail
			message = "js operator execution canceled"
		}
	} else if defaultCode == CodeJSCompileFailed {
		message = "js operator compile failed"
	}
	return fallbackResult(operatorID, language, value, code, message, err.Error())
}

func ensureJSONSize(value any, limit int) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if limit > 0 && len(encoded) > limit {
		return fmt.Errorf("json size %d exceeds limit %d", len(encoded), limit)
	}
	return nil
}

func effectiveTimeout(e *GojaExecutor, detail OperatorDetail) time.Duration {
	timeout := e.defaultTimeout
	if detail.Limits.TimeoutMS > 0 {
		timeout = time.Duration(detail.Limits.TimeoutMS) * time.Millisecond
	}
	if timeout <= 0 {
		timeout = defaultJSTimeout
	}
	maxTimeout := e.maxTimeout
	if maxTimeout <= 0 {
		maxTimeout = maxJSTimeout
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}
	return timeout
}

func effectiveMaxInputBytes(e *GojaExecutor, detail OperatorDetail) int {
	if detail.Limits.MaxInputBytes > 0 {
		return detail.Limits.MaxInputBytes
	}
	if e.maxInputBytes > 0 {
		return e.maxInputBytes
	}
	return defaultMaxInputBytes
}

func effectiveMaxOutputBytes(e *GojaExecutor, detail OperatorDetail) int {
	if detail.Limits.MaxOutputBytes > 0 {
		return detail.Limits.MaxOutputBytes
	}
	if e.maxOutputBytes > 0 {
		return e.maxOutputBytes
	}
	return defaultMaxOutputBytes
}
