package extension

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Registry 向外部报告的错误。调用方使用 errors.Is 判断。
var (
	// ErrDuplicateID 在同一个 Registry 中重复以同一 ID 调用 Register 时
	// 返回。受信注册必须唯一。
	ErrDuplicateID = errors.New("extension: duplicate id")
	// ErrUnknownID 在 Resolve 时搜不到任何已注册实现的 ID 时返回。
	ErrUnknownID = errors.New("extension: unknown id")
	// ErrKindMismatch 在 Resolve 时发现实现声明的 Kind 与调用方预期不一致
	// 时返回。
	ErrKindMismatch = errors.New("extension: kind mismatch")
)

// Implementation 携带每一个已注册实现的元数据。Impl 为 interface{} 类型，
// 确保同一个 registry 可以容纳异构类型；各 Kind 专属的 accessor
// （RegisterIdentityResolver 等）在取回时就完成类型校验。
type Implementation struct {
	// ID 与 Policy.ID 一致，是主键。
	ID string
	// Kind 标识本实现参与的管道阶段。
	Kind Kind
	// Impl 是具体实现。消费方通过 Registry 上各 Kind 专属的 accessor
	// 取回，不必手写类型断言。
	Impl any
	// Fingerprint 是 (ID, Kind, build fingerprint, config hash) 的内容 hash，
	// 由 Registry.Freeze 设置；调用方不应直接写进该字段。
	Fingerprint string
}

// Registry 是受信实现目录。宿主在启动时注册 Go 实现；YAML binding 后续
// 按 ID 选中子集。
//
// 并发语义：
//   - Register / RegisterX 在 Freeze 之前可以在任意 goroutine 调用。
//   - Freeze 之后所有读都无锁并以 ID 为键；不接受任何写。
//
// 一个 Engine 搭配一个 Registry 是常规用法；宿主通过强类型的
// harness.WithIdentityResolver / WithRunInitializer / ... 向 Registry 馈送。
type Registry struct {
	mu     sync.RWMutex
	items  map[string]Implementation
	frozen bool
}

// NewRegistry 创建一个空 Registry。
func NewRegistry() *Registry {
	return &Registry{items: make(map[string]Implementation)}
}

// Register 以稳定 ID 注册一个受信实现。ID 重复会以 ErrDuplicateID 失败。
// Freeze 之后再 Register 会报错：Freeze 标记着本次构建中契约已冻结。
func (r *Registry) Register(id string, kind Kind, impl any) error {
	if id == "" {
		return fmt.Errorf("%w: id must be non-empty", ErrDuplicateID)
	}
	if impl == nil {
		return fmt.Errorf("extension: impl for %q must not be nil", id)
	}
	if PhaseOf(kind) == 0 {
		return fmt.Errorf("extension: kind %q is not registered", kind)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return errors.New("extension: registry is frozen")
	}
	if _, exists := r.items[id]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateID, id)
	}
	r.items[id] = Implementation{ID: id, Kind: kind, Impl: impl}
	return nil
}

// Freeze 锁定 Registry 并为每个注册盖上 fingerprint。fingerprint 由 ID + kind
// + 调用方提供的 build seed（git revision、module 版本等）派生。测试
// 时可传空 seed。
func (r *Registry) Freeze(buildSeed string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return
	}
	// 确定性顺序，保证 BuildReport 中 fingerprint 稳定。
	ids := make([]string, 0, len(r.items))
	for id := range r.items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		item := r.items[id]
		item.Fingerprint = fingerprint(buildSeed, item.ID, string(item.Kind))
		r.items[id] = item
	}
	r.frozen = true
}

// Resolve 返回 id 对应的 Implementation，同时校验它的 Kind。
func (r *Registry) Resolve(id string, kind Kind) (Implementation, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	item, ok := r.items[id]
	if !ok {
		return Implementation{}, fmt.Errorf("%w: %q", ErrUnknownID, id)
	}
	if item.Kind != kind {
		return Implementation{}, fmt.Errorf("%w: %q is %s, want %s", ErrKindMismatch, id, item.Kind, kind)
	}
	return item, nil
}

// Snapshot 以确定性顺序拷贝全部已注册 Implementation，kernel 将它投影到
// BuildReport。
func (r *Registry) Snapshot() []Implementation {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.items))
	for id := range r.items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Implementation, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.items[id])
	}
	return out
}

// fingerprint 是身份元组的小型确定性 hash。调用方传入 buildSeed
// （git rev、module tag），确保重新发布产生不同的 fingerprint，Resume 在
// 漂移时会 fail closed。
func fingerprint(buildSeed, id, kind string) string {
	h := sha256.New()
	h.Write([]byte(buildSeed))
	h.Write([]byte("\x00"))
	h.Write([]byte(id))
	h.Write([]byte("\x00"))
	h.Write([]byte(kind))
	sum := h.Sum(nil)
	return "sha256:" + hex.EncodeToString(sum[:16])
}
