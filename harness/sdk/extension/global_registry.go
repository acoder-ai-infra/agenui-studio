package extension

import (
	"fmt"
	"sync"
)

// global_registry.go 是包级全局扩展注册表（"扩展全集"）。
//
// 与 harness.WithXxxProvider（Engine Options 注册）互补的第二种注册方式：
// 业务方在 init() 中把存量扩展实现一次性注册进全集，创建 Engine 时无需再
// 逐个传 Options。agents.yaml `extensions` / tools.yaml `handler` 按 ID 引用
// 实现时，解析优先级为：Engine Options（ExtensionCatalog）优先，未命中再查
// 全集；两处都没有则 fail-closed。
//
// 收录范围仅限四类配置引用型扩展：BeforeModelHook / ToolCallInterceptor /
// OutputValidator / Tool（FunctionTool）。其余 Engine 级扩展类型
//（identity_resolver、input_normalizer 等）没有按配置引用的语义，不进全集。
//
// 注册契约对齐 sql.Register 惯例：注册应发生在 init() 期；nil 元素、空
// ID(Name)、同键重复注册一律 panic（程序员错误，越早暴露越好）。注册后的
// 全集为进程级只增状态，不提供 Reset。

var (
	globalMu           sync.RWMutex
	globalHooks        = make(map[string]BeforeModelHook)
	globalInterceptors = make(map[string]ToolCallInterceptor)
	globalValidators   = make(map[string]OutputValidator)
	globalTools        = make(map[string]FunctionTool)
)

// RegisterBeforeModelHooks 把一组 BeforeModelHook 注册进全集，
// 键为 impl.ID()。nil 元素 / 空 ID / 重复 ID panic。
func RegisterBeforeModelHooks(impls ...BeforeModelHook) {
	globalMu.Lock()
	defer globalMu.Unlock()
	for _, impl := range impls {
		if impl == nil {
			panic("extension: RegisterBeforeModelHooks received a nil implementation")
		}
		id := impl.ID()
		if id == "" {
			panic("extension: RegisterBeforeModelHooks received an implementation with empty ID")
		}
		if _, exists := globalHooks[id]; exists {
			panic(fmt.Sprintf("extension: before model hook %q is already registered in the global registry", id))
		}
		globalHooks[id] = impl
	}
}

// RegisterToolCallInterceptors 把一组 ToolCallInterceptor 注册进全集，
// 键为 impl.ID()。nil 元素 / 空 ID / 重复 ID panic。
func RegisterToolCallInterceptors(impls ...ToolCallInterceptor) {
	globalMu.Lock()
	defer globalMu.Unlock()
	for _, impl := range impls {
		if impl == nil {
			panic("extension: RegisterToolCallInterceptors received a nil implementation")
		}
		id := impl.ID()
		if id == "" {
			panic("extension: RegisterToolCallInterceptors received an implementation with empty ID")
		}
		if _, exists := globalInterceptors[id]; exists {
			panic(fmt.Sprintf("extension: tool call interceptor %q is already registered in the global registry", id))
		}
		globalInterceptors[id] = impl
	}
}

// RegisterOutputValidators 把一组 OutputValidator 注册进全集，键为
// impl.ID()。nil 元素 / 空 ID / 重复 ID panic。
func RegisterOutputValidators(impls ...OutputValidator) {
	globalMu.Lock()
	defer globalMu.Unlock()
	for _, impl := range impls {
		if impl == nil {
			panic("extension: RegisterOutputValidators received a nil implementation")
		}
		id := impl.ID()
		if id == "" {
			panic("extension: RegisterOutputValidators received an implementation with empty ID")
		}
		if _, exists := globalValidators[id]; exists {
			panic(fmt.Sprintf("extension: output validator %q is already registered in the global registry", id))
		}
		globalValidators[id] = impl
	}
}

// RegisterTools 把一组 FunctionTool 直接注册进全集（无需 ToolProvider 包裹），
// 键为 tool.Name()，与 tools.yaml 的 handler 字段对应。nil 元素 / 空 Name /
// 重复 Name panic。"harness." 前缀属于内置 handler 保留段，由 Composition
// Root 在 Build 时 fail-closed 拒绝。
func RegisterTools(tools ...FunctionTool) {
	globalMu.Lock()
	defer globalMu.Unlock()
	for _, tool := range tools {
		if tool == nil {
			panic("extension: RegisterTools received a nil tool")
		}
		name := tool.Name()
		if name == "" {
			panic("extension: RegisterTools received a tool with empty Name")
		}
		if _, exists := globalTools[name]; exists {
			panic(fmt.Sprintf("extension: function tool %q is already registered in the global registry", name))
		}
		globalTools[name] = tool
	}
}

// LookupBeforeModelHook 按 ID 查询全集中的 BeforeModelHook。
func LookupBeforeModelHook(id string) (BeforeModelHook, bool) {
	globalMu.RLock()
	defer globalMu.RUnlock()
	impl, ok := globalHooks[id]
	return impl, ok
}

// LookupToolCallInterceptor 按 ID 查询全集中的 ToolCallInterceptor。
func LookupToolCallInterceptor(id string) (ToolCallInterceptor, bool) {
	globalMu.RLock()
	defer globalMu.RUnlock()
	impl, ok := globalInterceptors[id]
	return impl, ok
}

// LookupOutputValidator 按 ID 查询全集中的 OutputValidator。
func LookupOutputValidator(id string) (OutputValidator, bool) {
	globalMu.RLock()
	defer globalMu.RUnlock()
	impl, ok := globalValidators[id]
	return impl, ok
}

// GlobalFunctionTools 返回全集中已注册 FunctionTool 表的快照（键为
// tool.Name()）。Composition Root 在 Build 时以它作为 tools.yaml handler
// 查找的兜底层（builtin / Options 优先）。
func GlobalFunctionTools() map[string]FunctionTool {
	globalMu.RLock()
	defer globalMu.RUnlock()
	out := make(map[string]FunctionTool, len(globalTools))
	for name, tool := range globalTools {
		out[name] = tool
	}
	return out
}
