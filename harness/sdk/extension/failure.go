package extension

import "time"

// FailurePolicy 告诉 kernel 如何处理扩展内部的失败。它在注册时由 ID + YAML
// 配置设定，并在 Run 期间冻结。
type FailurePolicy string

const (
	// FailClosed：任何错误都会中止所在管道阶段，并以 canonical 的
	// *_failed 事件让 Run 失败。安全关键扩展（guardrail、output validator、
	// tool ACL）必须使用 FailClosed。
	FailClosed FailurePolicy = "fail_closed"

	// FailOpen：kernel 记录错误但继续管道阶段。只有那些只做便利性工作的
	// 扩展（event observer、尽力而为的 context contributor）可以设为
	// FailOpen；默认是 FailClosed。
	FailOpen FailurePolicy = "fail_open"
)

// Policy 是扩展注册在运行期的配置投影。kernel 在决定如何调用某个实现时
// 看到的正是它。
type Policy struct {
	// ID 是稳定的、人类可读的标识符，YAML 配置引用它，Registry 里以它为
	// 主键，Fingerprint 计算也以它为锚点。
	ID string
	// Kind 是本 Policy 适用的管道阶段。
	Kind Kind
	// Order 是同一 Kind 内的确定性执行顺序（数值小的优先）。只允许一个
	// 活跃实现的 Kind（例如 InputNormalizer）忽略 Order，恒用零。
	Order int
	// Failure 决定 fail-closed / fail-open 语义。
	Failure FailurePolicy
	// Timeout 约束单次 Invoke 调用；零值使用 kernel 默认（用户输入类
	// 扩展 2 秒，tool provider 30 秒）。
	Timeout time.Duration
	// Fingerprint 是实现身份与冻结配置的内容 hash。它由 Registry 计算，
	// 记录在 Run 上，并在 Resume 时校验。若实现以新的 Fingerprint 被重新
	// 部署，Resume 会以 ErrCapabilityDrift fail closed。
	Fingerprint string
}
