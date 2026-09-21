package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
	contextpkg "github.com/AGenUI/agenui-studio/harness/internal/context"
	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

// turn_prepare.go 是 HTTP 入口对 kernel 预回合扩展管线（R2a）的接入点：三个
// 开 Turn 的 handler（/ai/chat、/runs、/agents/{id}/chat）在 OpenTurn 之前调用
// prepareTurnInput，使 HTTP 与 SDK 两个入口穿过同一条治理链。
//
// hosted 模式默认没有扩展注册（TurnPreparer 为 nil 或无阶段），此路径零成本
// 直通；受益方是嵌入式宿主通过 kernel.Handler 暴露的 HTTP 面。

// preparedTurn 是 prepareTurnInput 的产出。
type preparedTurn struct {
	// Preview 是（可能被 InputNormalizer 改写后的）用户输入预览。
	Preview string
	// Fragments 是 ContextContributor 产出、待透传进 OpenTurn 的业务片段。
	Fragments []contextpkg.ContextFragment
}

// prepareTurnInput 用单段文本输入执行预回合管线，并把解析后的身份写回 tc。
// 管线未配置时原样直通。
func (d *Deps) prepareTurnInput(ctx context.Context, tc *observability.TraceContext, agentID, preview string) (preparedTurn, error) {
	out := preparedTurn{Preview: preview}
	if d == nil || d.TurnPreparer == nil || !d.TurnPreparer.HasStages() {
		return out, nil
	}
	result, err := d.TurnPreparer.PrepareTurn(ctx, kernel.PrepareTurnRequest{
		Identity: kernel.TurnIdentity{
			TenantID:  tc.TenantID,
			UserID:    tc.UserID,
			SessionID: tc.SessionID,
			AgentID:   agentID,
		},
		Input: extension.NormalizedMessage{
			Role:  "user",
			Parts: []extension.NormalizedPart{{Kind: "text", Text: preview}},
		},
	})
	if err != nil {
		return preparedTurn{}, err
	}
	// IdentityResolver 是宿主注册的受信 Go 代码（信任链高于调用方声明），其
	// 非空输出覆盖 trace 身份，随后所有存储写入按解析后作用域进行。
	tc.TenantID = result.Identity.TenantID
	tc.UserID = result.Identity.UserID
	tc.SessionID = result.Identity.SessionID
	out.Preview = kernel.NormalizedMessagePreview(result.Input)
	out.Fragments = kernel.ContextFragmentsForTurn(result.Fragments)
	return out, nil
}

// writeTurnPipelineError 把预回合管线错误映射为 HTTP 响应：注册无效类
// （类型不匹配 / panic / 空产出）返回 400；扩展自身的业务拒绝返回 422。
func writeTurnPipelineError(w http.ResponseWriter, err error) {
	if errors.Is(err, kernel.ErrTurnPipelineInvalid) {
		writeError(w, http.StatusBadRequest, "TURN_PIPELINE_INVALID", err.Error())
		return
	}
	writeError(w, http.StatusUnprocessableEntity, "TURN_PIPELINE_REJECTED", err.Error())
}
