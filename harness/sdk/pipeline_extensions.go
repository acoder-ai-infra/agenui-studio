package harness

import (
	"sort"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// pipeline_extensions.go 只保留 SDK DTO 与 extension DTO 之间的纯转换助手。
//
// 扩展的执行机制已全部下沉（R2/R3 重构）：
//   - IdentityResolver / RunInitializer / ContextContributor / InputNormalizer
//     由 kernel.TurnPipeline 在 OpenTurn 之前执行（engineImpl.prepareTurn 调用，
//     与 HTTP 入口共享同一治理链）；
//   - ToolProvider 的实现在 Composition Root 合并进 Tool Gateway handler 表；
//   - OutputValidator 桥接为 runtime.before_response hook；
//   - EventObserver 在服务端事件发布点 fan-out（internal/app 的 observedBroker）。
//
// SDK 客户端仅存的扩展消费是 ProtocolProjector（调用方视图投影）与
// OutputValidator 的结果视图填充（见 pipeline_validators.go）。

// scopedDataToExtension 把 SDK 的 run 级 ScopedData 投影为 extension DTO。
func scopedDataToExtension(in map[string]ScopedDataItem) map[string]extension.ScopedDataEntry {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]extension.ScopedDataEntry, len(in))
	for k, v := range in {
		out[k] = extension.ScopedDataEntry{
			Source:     v.Source,
			Visibility: string(v.Visibility),
			Value:      v.Value,
			Ref:        v.Ref,
			Hash:       v.Hash,
		}
	}
	return out
}

// mergeScopedDataFromExtension 把管线产出的 ScopedData 条目合并回 SDK 形状。
func mergeScopedDataFromExtension(target map[string]ScopedDataItem, additions map[string]extension.ScopedDataEntry) error {
	for k, v := range additions {
		if k == "" {
			return wrapInvalidRequest("turn pipeline produced empty ScopedData key")
		}
		target[k] = ScopedDataItem{
			Source:     v.Source,
			Visibility: Visibility(v.Visibility),
			Value:      v.Value,
			Ref:        v.Ref,
			Hash:       v.Hash,
		}
	}
	return nil
}

// scopedDataToStorage 把 prepareTurn 合并后的 Run 级 ScopedData 投影为
// storage 透传 DTO（按 key 排序保证确定性），随 OpenTurn 交给 dispatcher
// 填入 RunRequest.ScopedData。Value 拷贝一份，避免调用方后续改写共享底层字节。
func scopedDataToStorage(in map[string]ScopedDataItem) []storage.ScopedDataItem {
	if len(in) == 0 {
		return nil
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]storage.ScopedDataItem, 0, len(keys))
	for _, k := range keys {
		v := in[k]
		out = append(out, storage.ScopedDataItem{
			Key:        k,
			Source:     v.Source,
			Visibility: string(v.Visibility),
			Value:      cloneRaw(v.Value),
			Ref:        v.Ref,
			Hash:       v.Hash,
		})
	}
	return out
}
