package harness

// event_parity_test.go 是公共 SDK 枚举与 canonical schema 之间的自动一致性
// 门禁（兑现 the public SDK contract "公共 Event 与 canonical schema
// 建立自动一致性门禁"）。任何一侧新增 / 删除枚举值而未同步另一侧时，本文件
// 中的测试必须失败。
//
// 事实源约定：
//   - EventType     ← observability.AllEventTypes()（Event Type Registry）
//   - EventErrorType ← observability.AllEventErrorTypes()
//   - Visibility    ← observability.AllEventVisibilities()
//   - Extension Kind ← kernel.AllExtensionKinds 与 extension 包常量
//   - 终态集合       ← observability.IsTerminalRunEventType

import (
	"sort"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/kernel"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// diffStringSets 返回 want 中缺失于 got 的值（missing）与 got 中多出的值
// （extra），两侧都按字符串比较。
func diffStringSets(want, got []string) (missing, extra []string) {
	wantSet := make(map[string]bool, len(want))
	gotSet := make(map[string]bool, len(got))
	for _, v := range want {
		wantSet[v] = true
	}
	for _, v := range got {
		gotSet[v] = true
	}
	for _, v := range want {
		if !gotSet[v] {
			missing = append(missing, v)
		}
	}
	for _, v := range got {
		if !wantSet[v] {
			extra = append(extra, v)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

// TestEventTypeParityWithCanonicalRegistry 断言 SDK 暴露的 EventType 常量集合
// 与 canonical Event Type Registry 完全一致（双向）。
func TestEventTypeParityWithCanonicalRegistry(t *testing.T) {
	canonical := make([]string, 0)
	for _, et := range observability.AllEventTypes() {
		canonical = append(canonical, string(et))
	}
	sdk := make([]string, 0)
	seen := make(map[string]bool)
	for _, et := range allEventTypes() {
		if seen[string(et)] {
			t.Fatalf("SDK allEventTypes() 存在重复项: %s", et)
		}
		seen[string(et)] = true
		sdk = append(sdk, string(et))
	}
	missing, extra := diffStringSets(canonical, sdk)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("EventType 枚举漂移:\n  SDK 缺失(canonical 有而 SDK 无): %v\n  SDK 多出(SDK 有而 canonical 无): %v", missing, extra)
	}
}

// TestEventErrorTypeParity 断言 EventErrorType 枚举双侧一致。
func TestEventErrorTypeParity(t *testing.T) {
	canonical := make([]string, 0)
	for _, et := range observability.AllEventErrorTypes() {
		canonical = append(canonical, string(et))
	}
	sdk := []string{
		string(EventErrorTimeout), string(EventErrorCancelled), string(EventErrorPermissionDenied),
		string(EventErrorGuardrailBlocked), string(EventErrorSchemaValidation), string(EventErrorUpstream),
		string(EventErrorRateLimited), string(EventErrorResourceExhausted), string(EventErrorInternal),
	}
	missing, extra := diffStringSets(canonical, sdk)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("EventErrorType 枚举漂移:\n  SDK 缺失: %v\n  SDK 多出: %v", missing, extra)
	}
}

// TestVisibilityParity 断言 Visibility 枚举双侧一致。
func TestVisibilityParity(t *testing.T) {
	canonical := make([]string, 0)
	for _, v := range observability.AllEventVisibilities() {
		canonical = append(canonical, string(v))
	}
	sdk := []string{
		string(VisibilityUserVisible), string(VisibilityDebug),
		string(VisibilityInternal), string(VisibilityRestricted),
	}
	missing, extra := diffStringSets(canonical, sdk)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("Visibility 枚举漂移:\n  SDK 缺失: %v\n  SDK 多出: %v", missing, extra)
	}
}

// TestExtensionKindParity 断言 extension.Kind 与 kernel.ExtensionKind 的全部
// 字符串值一致（命名对齐约定第 1 层）。
func TestExtensionKindParity(t *testing.T) {
	kernelKinds := []string{
		string(kernel.ExtIdentityResolver), string(kernel.ExtRunInitializer),
		string(kernel.ExtContextContributor), string(kernel.ExtInputNormalizer),
		string(kernel.ExtToolProvider), string(kernel.ExtOutputValidator),
		string(kernel.ExtProtocolProjector), string(kernel.ExtEventObserver),
		string(kernel.ExtBeforeModelHook), string(kernel.ExtToolCallInterceptor),
	}
	extensionKinds := []string{
		string(extension.KindIdentityResolver), string(extension.KindRunInitializer),
		string(extension.KindContextContributor), string(extension.KindInputNormalizer),
		string(extension.KindToolProvider), string(extension.KindOutputValidator),
		string(extension.KindProtocolProjector), string(extension.KindEventObserver),
		string(extension.KindBeforeModelHook), string(extension.KindToolCallInterceptor),
	}
	missing, extra := diffStringSets(kernelKinds, extensionKinds)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("Extension Kind 枚举漂移:\n  extension 侧缺失: %v\n  extension 侧多出: %v", missing, extra)
	}
}

// TestTerminalEventTypeParity 断言 SDK 的终态判断与 canonical 定义逐类型一致。
func TestTerminalEventTypeParity(t *testing.T) {
	for _, et := range observability.AllEventTypes() {
		want := observability.IsTerminalRunEventType(et)
		got := isTerminalEventType(et)
		if want != got {
			t.Fatalf("终态判断漂移: event_type=%s canonical=%v sdk=%v", et, want, got)
		}
	}
}

// TestSDKContractVersionParity 断言 harness.Version 与 kernel.SDKContractVersion
// 同值（预回合管线在 HTTP 入口携带的 SDKVersion 依赖该一致性）。
func TestSDKContractVersionParity(t *testing.T) {
	if Version != kernel.SDKContractVersion {
		t.Fatalf("SDK 契约线漂移: harness.Version=%q kernel.SDKContractVersion=%q", Version, kernel.SDKContractVersion)
	}
}
