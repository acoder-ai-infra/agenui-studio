package harness

import (
	"context"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// extensionValidatorBinding pairs a registered OutputValidator implementation
// with its ExtensionEntry metadata so the stream fan-out can invoke it when
// a final_response event flows through.
//
// 治理职责已下沉（R2c）：OutputValidator 的强制拦截由 runtime 的
// before_response hook 承担（final_response 落盘前）。OutputRetry 会由
// Harness 子 Agent 执行器按预算创建下一次模型尝试；OutputFail 才会使 Run
// 失败。本文件仅作为 Execution.OutputValidation() 的客户端视图填充：
// 在流中观察到 final_response 时重新执行 validator 以便调用方读取结果，
// 不再具备任何拦截能力。
type extensionValidatorBinding struct {
	id        string
	timeout   time.Duration
	validator extension.OutputValidator
}

// eventValidators projects OutputValidator entries into per-run bindings.
func (e *engineImpl) eventValidators() []extensionValidatorBinding {
	if e == nil || e.kernel == nil || e.kernel.Extensions == nil {
		return nil
	}
	entries := e.kernel.Extensions.ByKind(kernel.ExtOutputValidator)
	if len(entries) == 0 {
		return nil
	}
	out := make([]extensionValidatorBinding, 0, len(entries))
	for _, entry := range entries {
		v, ok := entry.Implementation.(extension.OutputValidator)
		if !ok {
			continue
		}
		out = append(out, extensionValidatorBinding{
			id: entry.ID, timeout: entry.Timeout, validator: v,
		})
	}
	return out
}

// maybeRunValidators fires OutputValidator implementations exactly when the
// canonical stream sees a final_response event. Non-Accept results are stored
// on the Execution's validation cell so callers can inspect them without
// touching internal state.
func maybeRunValidators(ctx context.Context, validators []extensionValidatorBinding, ev Event, cell *executionOutputValidation) {
	if len(validators) == 0 || cell == nil {
		return
	}
	if ev.EventType != EventFinalResponse {
		return
	}
	text := ""
	if len(ev.PayloadPreview) > 0 {
		text = string(ev.PayloadPreview)
	}
	req := extension.OutputValidateRequest{Text: text, Attempt: 1}
	for _, ob := range validators {
		reqCtx := ctx
		if ob.timeout > 0 {
			var cancel context.CancelFunc
			reqCtx, cancel = context.WithTimeout(ctx, ob.timeout)
			defer cancel()
		}
		res, err := runValidatorSafely(reqCtx, ob.validator, req)
		if err != nil {
			cell.set(extension.OutputValidateResult{Action: extension.OutputFail, Reason: err.Error()})
			return
		}
		if res.Action != extension.OutputAccept {
			cell.set(res)
			return
		}
	}
	cell.set(extension.OutputValidateResult{Action: extension.OutputAccept})
}

// runValidatorSafely wraps a validator call with panic recovery so a
// misbehaving validator cannot terminate the SDK stream fan-out.
func runValidatorSafely(ctx context.Context, v extension.OutputValidator, req extension.OutputValidateRequest) (res extension.OutputValidateResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = ErrInvalidRequest
		}
	}()
	res, err = v.Validate(ctx, req)
	return
}
