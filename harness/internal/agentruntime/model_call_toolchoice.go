package agentruntime

import "context"

// model_call_toolchoice.go 承载 BeforeModelHook 逐轮下发的 ToolChoice 覆盖。
//
// eino 路径下 ToolChoice 不在 ChatModelAgentState 中（它是 model.Option，
// 非 state 字段），无法像 ToolInfos 那样在 BeforeModelRewriteState 里持久化。
// 因此 Composition Root 的 hook middleware 把本轮 ToolChoice 塞进返回的 ctx，
// 由 EinoChatModelProxy 在装配 ModelInvokeRequest.Options 时读取并覆盖。
// native 路径直接改写 ModelInvokeRequest.Options.ToolChoice，不经此 ctx。

type modelCallToolChoiceContextKey struct{}

// WithModelCallToolChoice 把本轮 ToolChoice 覆盖写入 ctx（空值不覆盖，调用方
// 应仅在非空时调用）。
func WithModelCallToolChoice(ctx context.Context, choice string) context.Context {
	return context.WithValue(ctx, modelCallToolChoiceContextKey{}, choice)
}

// ModelCallToolChoiceFrom 读取 ctx 中的 ToolChoice 覆盖。
func ModelCallToolChoiceFrom(ctx context.Context) (string, bool) {
	choice, ok := ctx.Value(modelCallToolChoiceContextKey{}).(string)
	return choice, ok && choice != ""
}
