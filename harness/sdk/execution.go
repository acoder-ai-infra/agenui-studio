package harness

import (
	"sync"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// Execution 是 Start / Resume 返回的句柄，将 Run 的稳定身份与其
// EventStream 捕绑到一起，便于调用方按顺序消耗事件，同时保留一个
// 引用用于 GetRun / GetResult / Cancel。
type Execution interface {
	// Handle 返回本次执行冻结后的 RunHandle，包含 canonical Identity
	//（含服务器分配的 RunID / SessionID）以及 Run 调度时使用的
	// AgentBindingID。
	Handle() RunHandle

	// Events 返回 EventStream。可多次调用；每个调用者看到的是同一个
	// 底层流（也就是说，事件只会投递一次）。需要一条完全独立的
	// 新流时请使用 Engine.Subscribe。
	Events() EventStream

	// OutputValidation 返回 SDK 在流中看到 final_response 事件时观察到的
	// 最后一个 OutputValidator 结果；若未注册 validator、或尚未观察到
	// final_response，则返回零值。它在流 fan-out 内部自动填充。
	OutputValidation() extension.OutputValidateResult

	// ProjectedFrames 返回已注册的 ProtocolProjector 在流耗尽过程中产出
	// 的全部 frame。Frame 按流顺序叠加；若调用方需在 Close 后保留使用，
	// 请防御性拷贝该 slice。
	ProjectedFrames() []extension.Frame
}

// executionOutputValidation 是 Execution.OutputValidation 后面的私有存储档。
// 由 sync.Mutex 保护，使事件流可写、调用方可读。
type executionOutputValidation struct {
	mu     sync.Mutex
	result extension.OutputValidateResult
	valid  bool
}

func (v *executionOutputValidation) set(r extension.OutputValidateResult) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.result = r
	v.valid = true
}

func (v *executionOutputValidation) get() extension.OutputValidateResult {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.result
}

// executionFrames 存放已注册的 ProtocolProjector 在流耗尽过程中产出的
// frame 列表。它对并发读写安全：流 fan-out 追加，调用方读。
type executionFrames struct {
	mu     sync.Mutex
	frames []extension.Frame
}

func (f *executionFrames) append(frames ...extension.Frame) {
	if len(frames) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = append(f.frames, frames...)
}

func (f *executionFrames) snapshot() []extension.Frame {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]extension.Frame, len(f.frames))
	copy(out, f.frames)
	return out
}

// RunHandle 是 Start / Resume 返回的 Run 稳定身份，供 SDK 调用方存储、
// 以便后续查找 Run。
type RunHandle struct {
	// Identity 是冻结后的身份（调用方留空时的 RunID / SessionID 会由
	// 服务器分配并回填）。
	Identity Identity
	// AgentBindingID 是本 Run 的 canonical BindingID。
	AgentBindingID string
	// ConfigSnapshotRef 指向冻结后的 Agent 配置快照。
	ConfigSnapshotRef string
	// TraceID 是携带本 Run 的 OTel 兼容 trace id。
	TraceID string
	// InputManifest 是本轮用户输入 Parts 的冻结清单。Resume 时为空
	//（Resume 基于已冻结的原始 manifest 工作）。
	InputManifest InputManifest
}
