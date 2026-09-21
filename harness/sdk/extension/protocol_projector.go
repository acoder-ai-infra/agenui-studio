package extension

import (
	"context"
	"encoding/json"
	"time"
)

// Frame 是 ProtocolProjector.Project 产出的一帧业务协议帧。它有意保持通用，
// 让 SSE / WebSocket / gRPC 等不同 transport 可以共用同一个 projector；
// 具体序列化由外层 transport 决定。
type Frame struct {
	// Kind 是业务帧名（如 "steps_init"、"step_status"、"delta"）。对 Harness
	// 而言是不透明的。
	Kind string
	// Sequence 与源 canonical Event 的 Sequence 一致，便于断线重连对齐；
	// Projectors receive the canonical sequence from the source event.
	Sequence int64
	// Payload 是序列化后的业务 payload；推荐 JSON，但 transport 特定的
	// projector 可以替换为二进制。
	Payload json.RawMessage
	// Visibility 与源 Event 的 Visibility 一致，由 transport 强制执行。
	Visibility string
	// CreatedAt 与源 Event 的 CreatedAt 一致，便于按时间顺序 replay。
	CreatedAt time.Time
}

// ProtocolEvent 是 ProtocolProjector 收到的 canonical AgentEvent 只读投影。
// 它是一份拷贝，防止 projector 修改事实链。
type ProtocolEvent struct {
	EventID    string
	Sequence   int64
	SessionID  string
	RunID      string
	StepID     string
	AgentID    string
	EventType  string
	Visibility string
	Payload    json.RawMessage
	PayloadRef string
	Usage      json.RawMessage
	Error      *ProtocolEventError
	CreatedAt  time.Time
}

// ProtocolEventError 是 canonical EventError 在扩展层的镜像。
type ProtocolEventError struct {
	Code      string
	Type      string
	Message   string
	Retryable bool
}

// ProtocolProjector 把 canonical AgentEvent 投影为零到多帧业务协议帧。它
// 只读；projector 不得伪造 canonical event。如果 projector 想发出一个与任何
// AgentEvent 都不对应的事件，业务需要升级 transport schema；SDK 不会把业务
// 语义伪装成假的 AgentEvent。
type ProtocolProjector interface {
	Project(ctx context.Context, ev ProtocolEvent) ([]Frame, error)
}
