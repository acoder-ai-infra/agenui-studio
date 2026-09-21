package agentregistry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
)

var (
	ErrPromptNotFound            = errors.New("prompt version not found")
	ErrPromptVersionExists       = errors.New("prompt version already exists")
	ErrPromptInvalid             = errors.New("invalid prompt version")
	ErrPromptContentHashMismatch = errors.New("prompt content hash mismatch")
	ErrPromptContentUnavailable  = errors.New("prompt content unavailable")
)

const (
	maxPromptRefRunes        = 512
	maxPromptVersionRunes    = 128
	maxPromptContentRefRunes = 1024
	maxPromptCreatorRunes    = 255
	maxInlinePromptBytes     = 1 << 20
	maxResolvedPromptBytes   = 8 << 20
	maxPromptVariables       = 128
	maxPromptVariableRunes   = 128
)

// PromptKey 精确定位一个不可变的 system prompt 版本；运行链路禁止隐式读取 latest。
type PromptKey struct {
	Ref     string `json:"prompt_ref" yaml:"prompt_ref"`
	Version string `json:"version" yaml:"version"`
}

// PromptVersion 是 PromptStore 的持久化模型。Content 与 ContentRef 必须且只能提供一个。
type PromptVersion struct {
	Ref         string    `json:"prompt_ref" yaml:"prompt_ref"`
	Version     string    `json:"version" yaml:"version"`
	Content     string    `json:"content,omitempty" yaml:"content,omitempty"`
	ContentRef  string    `json:"content_ref,omitempty" yaml:"content_ref,omitempty"`
	ContentHash string    `json:"content_hash" yaml:"content_hash"`
	Variables   []string  `json:"variables,omitempty" yaml:"variables,omitempty"`
	CreatedAt   time.Time `json:"created_at" yaml:"created_at"`
	CreatedBy   string    `json:"created_by,omitempty" yaml:"created_by,omitempty"`
}

// PromptSnapshot 是运行时可冻结、回放的 system prompt 解析结果。
type PromptSnapshot struct {
	Ref         string   `json:"prompt_ref"`
	Version     string   `json:"version"`
	SnapshotRef string   `json:"snapshot_ref"`
	ContentHash string   `json:"content_hash"`
	PromptHash  string   `json:"prompt_hash"`
	Content     string   `json:"-"`
	Variables   []string `json:"variables,omitempty"`
}

type PromptStore interface {
	Create(ctx context.Context, prompt PromptVersion) error
	Get(ctx context.Context, key PromptKey) (PromptVersion, error)
	ListVersions(ctx context.Context, ref string) ([]PromptVersion, error)
}

type PromptResolver interface {
	Resolve(ctx context.Context, key PromptKey) (PromptSnapshot, error)
}

// PromptContentResolver 仅在 PromptVersion 使用外部 content_ref 时需要，例如 Artifact Store。
type PromptContentResolver interface {
	ResolvePromptContent(ctx context.Context, contentRef string) (string, error)
}

type storePromptResolver struct {
	store           PromptStore
	contentResolver PromptContentResolver
}

// PromptStoreFromResolver exposes the already-composed store to the management
// adapter without creating a second store or bypassing the Runtime resolver.
func PromptStoreFromResolver(resolver PromptResolver) PromptStore {
	provider, ok := resolver.(interface{ PromptStore() PromptStore })
	if !ok {
		return nil
	}
	return provider.PromptStore()
}

func (r *storePromptResolver) PromptStore() PromptStore { return r.store }

// RuntimePromptResolver 将 Registry 的存储模型收敛为 Runtime 唯一允许读取的 system prompt 端口。
type RuntimePromptResolver struct {
	resolver PromptResolver
}

func NewRuntimePromptResolver(resolver PromptResolver) (*RuntimePromptResolver, error) {
	if resolver == nil {
		return nil, errors.New("agentregistry: prompt resolver is required")
	}
	return &RuntimePromptResolver{resolver: resolver}, nil
}

func (r *RuntimePromptResolver) ResolveSystemPrompt(ctx context.Context, req agentruntime.SystemPromptResolveRequest) (agentruntime.SystemPromptSnapshot, error) {
	snapshot, err := r.resolver.Resolve(ctx, PromptKey{Ref: req.Ref, Version: req.Version})
	if err != nil {
		return agentruntime.SystemPromptSnapshot{}, err
	}
	if req.ExpectedPromptHash != "" && req.ExpectedPromptHash != snapshot.PromptHash ||
		req.ExpectedSnapshotRef != "" && req.ExpectedSnapshotRef != snapshot.SnapshotRef ||
		req.ExpectedContentHash != "" && req.ExpectedContentHash != snapshot.ContentHash {
		return agentruntime.SystemPromptSnapshot{}, agentruntime.ErrSystemPromptSnapshotMismatch
	}
	return agentruntime.SystemPromptSnapshot{
		Ref:         snapshot.Ref,
		Version:     snapshot.Version,
		SnapshotRef: snapshot.SnapshotRef,
		ContentHash: snapshot.ContentHash,
		PromptHash:  snapshot.PromptHash,
		Content:     snapshot.Content,
		Variables:   cloneStrings(snapshot.Variables),
	}, nil
}

func NewPromptResolver(store PromptStore, contentResolver ...PromptContentResolver) (PromptResolver, error) {
	if store == nil {
		return nil, errors.New("agentregistry: prompt store is required")
	}
	if len(contentResolver) > 1 {
		return nil, errors.New("agentregistry: at most one prompt content resolver is allowed")
	}
	resolver := &storePromptResolver{store: store}
	if len(contentResolver) == 1 {
		if contentResolver[0] == nil {
			return nil, errors.New("agentregistry: prompt content resolver is nil")
		}
		resolver.contentResolver = contentResolver[0]
	}
	return resolver, nil
}

func (r *storePromptResolver) Resolve(ctx context.Context, key PromptKey) (PromptSnapshot, error) {
	key, err := normalizePromptKey(key)
	if err != nil {
		return PromptSnapshot{}, err
	}
	prompt, err := r.store.Get(ctx, key)
	if err != nil {
		return PromptSnapshot{}, err
	}
	if err := validateStoredPromptVersion(prompt); err != nil {
		return PromptSnapshot{}, err
	}
	if prompt.Ref != key.Ref || prompt.Version != key.Version {
		return PromptSnapshot{}, fmt.Errorf("%w: prompt identity mismatch", ErrStoreCorrupt)
	}
	content := prompt.Content
	contentRef := prompt.ContentRef
	if content == "" {
		if r.contentResolver == nil {
			return PromptSnapshot{}, fmt.Errorf("%w: external content resolver is not configured", ErrPromptContentUnavailable)
		}
		content, err = r.contentResolver.ResolvePromptContent(ctx, contentRef)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return PromptSnapshot{}, ctxErr
			}
			if errors.Is(err, context.Canceled) {
				return PromptSnapshot{}, context.Canceled
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return PromptSnapshot{}, context.DeadlineExceeded
			}
			// content_ref 可能包含内部路径或鉴权信息，错误链不得把它带到日志。
			return PromptSnapshot{}, fmt.Errorf("%w: external content resolver failed", ErrPromptContentUnavailable)
		}
		if err := validateResolvedPromptContent(content); err != nil {
			return PromptSnapshot{}, err
		}
	}
	if err := verifyPromptContentHash(content, prompt.ContentHash, true); err != nil {
		return PromptSnapshot{}, err
	}
	promptHash, err := computePromptHash(key, prompt.ContentHash, prompt.Variables)
	if err != nil {
		return PromptSnapshot{}, err
	}
	return PromptSnapshot{
		Ref:         key.Ref,
		Version:     key.Version,
		SnapshotRef: promptSnapshotRef(key, promptHash),
		ContentHash: prompt.ContentHash,
		PromptHash:  promptHash,
		Content:     content,
		Variables:   cloneStrings(prompt.Variables),
	}, nil
}

// MemoryPromptStore 是本地开发与单测 backend；线上多实例应注入 SQLPromptStore。
type MemoryPromptStore struct {
	mu      sync.RWMutex
	prompts map[PromptKey]PromptVersion
}

func NewMemoryPromptStore() *MemoryPromptStore {
	return &MemoryPromptStore{prompts: make(map[PromptKey]PromptVersion)}
}

func (s *MemoryPromptStore) Create(ctx context.Context, prompt PromptVersion) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	prepared, err := preparePromptVersion(prompt, time.Now())
	if err != nil {
		return err
	}
	key := PromptKey{Ref: prepared.Ref, Version: prepared.Version}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if existing, ok := s.prompts[key]; ok {
		if samePromptVersion(existing, prepared) {
			return nil
		}
		return promptVersionConflict(key)
	}
	s.prompts[key] = clonePromptVersion(prepared)
	return nil
}

func (s *MemoryPromptStore) Get(ctx context.Context, key PromptKey) (PromptVersion, error) {
	if err := ctx.Err(); err != nil {
		return PromptVersion{}, err
	}
	key, err := normalizePromptKey(key)
	if err != nil {
		return PromptVersion{}, err
	}
	s.mu.RLock()
	if err := ctx.Err(); err != nil {
		s.mu.RUnlock()
		return PromptVersion{}, err
	}
	prompt, ok := s.prompts[key]
	s.mu.RUnlock()
	if !ok {
		return PromptVersion{}, ErrPromptNotFound
	}
	if err := validateStoredPromptVersion(prompt); err != nil {
		return PromptVersion{}, err
	}
	return clonePromptVersion(prompt), nil
}

func (s *MemoryPromptStore) ListVersions(ctx context.Context, inputRef string) ([]PromptVersion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ref, err := normalizePromptRef(inputRef)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	versions := make([]PromptVersion, 0)
	for key, prompt := range s.prompts {
		if key.Ref != ref {
			continue
		}
		if err := validateStoredPromptVersion(prompt); err != nil {
			return nil, err
		}
		versions = append(versions, clonePromptVersion(prompt))
	}
	sort.Slice(versions, func(i, j int) bool {
		if versions[i].CreatedAt.Equal(versions[j].CreatedAt) {
			return versions[i].Version > versions[j].Version
		}
		return versions[i].CreatedAt.After(versions[j].CreatedAt)
	})
	return versions, nil
}

// PromptContentHash 对最终 system prompt 原文字节求哈希，不能用 ref 或 version 代替。
func PromptContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func preparePromptVersion(input PromptVersion, now time.Time) (PromptVersion, error) {
	prompt := clonePromptVersion(input)
	key, err := normalizePromptKey(PromptKey{Ref: prompt.Ref, Version: prompt.Version})
	if err != nil {
		return PromptVersion{}, err
	}
	prompt.Ref, prompt.Version = key.Ref, key.Version
	prompt.ContentRef = strings.TrimSpace(prompt.ContentRef)
	prompt.ContentHash = strings.TrimSpace(prompt.ContentHash)
	prompt.CreatedBy = strings.TrimSpace(prompt.CreatedBy)
	if !utf8.ValidString(prompt.ContentRef) {
		return PromptVersion{}, fmt.Errorf("%w: content_ref must be valid UTF-8", ErrPromptInvalid)
	}
	if utf8.RuneCountInString(prompt.ContentRef) > maxPromptContentRefRunes {
		return PromptVersion{}, fmt.Errorf("%w: content_ref exceeds %d characters", ErrPromptInvalid, maxPromptContentRefRunes)
	}
	if !utf8.ValidString(prompt.CreatedBy) {
		return PromptVersion{}, fmt.Errorf("%w: created_by must be valid UTF-8", ErrPromptInvalid)
	}
	if utf8.RuneCountInString(prompt.CreatedBy) > maxPromptCreatorRunes {
		return PromptVersion{}, fmt.Errorf("%w: created_by exceeds %d characters", ErrPromptInvalid, maxPromptCreatorRunes)
	}
	prompt.Variables, err = normalizePromptVariables(prompt.Variables)
	if err != nil {
		return PromptVersion{}, err
	}
	if (prompt.Content == "") == (prompt.ContentRef == "") {
		return PromptVersion{}, fmt.Errorf("%w: content and content_ref must contain exactly one value", ErrPromptInvalid)
	}
	if prompt.Content != "" {
		if err := validateInlinePromptContent(prompt.Content, false); err != nil {
			return PromptVersion{}, err
		}
		want := PromptContentHash(prompt.Content)
		if prompt.ContentHash == "" {
			prompt.ContentHash = want
		} else if err := verifyPromptContentHash(prompt.Content, prompt.ContentHash, false); err != nil {
			return PromptVersion{}, err
		}
	} else if !validPromptHash(prompt.ContentHash) {
		return PromptVersion{}, fmt.Errorf("%w: content_hash must be sha256 for external content", ErrPromptInvalid)
	}
	if prompt.CreatedAt.IsZero() {
		prompt.CreatedAt = now
	}
	prompt.CreatedAt = prompt.CreatedAt.UTC()
	if prompt.CreatedAt.UnixMilli() < 0 {
		return PromptVersion{}, fmt.Errorf("%w: created_at must not precede Unix epoch", ErrPromptInvalid)
	}
	return prompt, nil
}

func validateStoredPromptVersion(prompt PromptVersion) error {
	key, err := normalizePromptKey(PromptKey{Ref: prompt.Ref, Version: prompt.Version})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStoreCorrupt, err)
	}
	if key.Ref != prompt.Ref || key.Version != prompt.Version {
		return fmt.Errorf("%w: non-canonical prompt identity", ErrStoreCorrupt)
	}
	if (prompt.Content == "") == (prompt.ContentRef == "") {
		return fmt.Errorf("%w: content and content_ref must contain exactly one value", ErrStoreCorrupt)
	}
	if !utf8.ValidString(prompt.ContentRef) || prompt.ContentRef != strings.TrimSpace(prompt.ContentRef) {
		return fmt.Errorf("%w: non-canonical content_ref", ErrStoreCorrupt)
	}
	if utf8.RuneCountInString(prompt.ContentRef) > maxPromptContentRefRunes ||
		!utf8.ValidString(prompt.CreatedBy) || prompt.CreatedBy != strings.TrimSpace(prompt.CreatedBy) ||
		utf8.RuneCountInString(prompt.CreatedBy) > maxPromptCreatorRunes {
		return fmt.Errorf("%w: prompt metadata exceeds storage contract", ErrStoreCorrupt)
	}
	if prompt.CreatedAt.IsZero() || prompt.CreatedAt.UnixMilli() < 0 {
		return fmt.Errorf("%w: invalid prompt created_at", ErrStoreCorrupt)
	}
	variables, err := normalizePromptVariables(prompt.Variables)
	if err != nil || !reflect.DeepEqual(variables, prompt.Variables) {
		return fmt.Errorf("%w: invalid or non-canonical prompt variables", ErrStoreCorrupt)
	}
	if prompt.Content != "" {
		if err := validateInlinePromptContent(prompt.Content, true); err != nil {
			return err
		}
		return verifyPromptContentHash(prompt.Content, prompt.ContentHash, true)
	}
	if !validPromptHash(prompt.ContentHash) {
		return fmt.Errorf("%w: invalid external prompt content_hash", ErrStoreCorrupt)
	}
	return nil
}

func normalizePromptKey(key PromptKey) (PromptKey, error) {
	ref, err := normalizePromptRef(key.Ref)
	if err != nil {
		return PromptKey{}, err
	}
	key.Ref = ref
	key.Version = strings.TrimSpace(key.Version)
	if key.Version == "" {
		return PromptKey{}, fmt.Errorf("%w: version is required", ErrPromptInvalid)
	}
	if !utf8.ValidString(key.Version) {
		return PromptKey{}, fmt.Errorf("%w: version must be valid UTF-8", ErrPromptInvalid)
	}
	if utf8.RuneCountInString(key.Version) > maxPromptVersionRunes {
		return PromptKey{}, fmt.Errorf("%w: version exceeds %d characters", ErrPromptInvalid, maxPromptVersionRunes)
	}
	return key, nil
}

func normalizePromptRef(input string) (string, error) {
	ref := strings.TrimSpace(input)
	if ref == "" {
		return "", fmt.Errorf("%w: prompt_ref is required", ErrPromptInvalid)
	}
	if utf8.RuneCountInString(ref) > maxPromptRefRunes {
		return "", fmt.Errorf("%w: prompt_ref exceeds %d characters", ErrPromptInvalid, maxPromptRefRunes)
	}
	if !utf8.ValidString(ref) || !canonicalPromptRef(ref) {
		return "", fmt.Errorf("%w: prompt_ref must be a canonical prompt:// URI", ErrPromptInvalid)
	}
	return ref, nil
}

func normalizePromptVariables(input []string) ([]string, error) {
	if len(input) == 0 {
		return []string{}, nil
	}
	if len(input) > maxPromptVariables {
		return nil, fmt.Errorf("%w: prompt variables exceed %d entries", ErrPromptInvalid, maxPromptVariables)
	}
	seen := make(map[string]struct{}, len(input))
	variables := make([]string, 0, len(input))
	for _, inputVariable := range input {
		if !utf8.ValidString(inputVariable) {
			return nil, fmt.Errorf("%w: prompt variable must be valid UTF-8", ErrPromptInvalid)
		}
		variable := strings.TrimSpace(inputVariable)
		if variable == "" {
			return nil, fmt.Errorf("%w: prompt variable is empty", ErrPromptInvalid)
		}
		if utf8.RuneCountInString(variable) > maxPromptVariableRunes {
			return nil, fmt.Errorf("%w: prompt variable exceeds %d characters", ErrPromptInvalid, maxPromptVariableRunes)
		}
		if _, duplicate := seen[variable]; duplicate {
			return nil, fmt.Errorf("%w: duplicate prompt variable %q", ErrPromptInvalid, variable)
		}
		seen[variable] = struct{}{}
		variables = append(variables, variable)
	}
	sort.Strings(variables)
	return variables, nil
}

func canonicalPromptRef(ref string) bool {
	if !strings.HasPrefix(ref, "prompt://") {
		return false
	}
	parsed, err := url.Parse(ref)
	return err == nil && parsed.Scheme == "prompt" && parsed.Host != "" && parsed.User == nil &&
		parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == "" && parsed.String() == ref
}

func validateInlinePromptContent(content string, stored bool) error {
	if utf8.ValidString(content) && len(content) <= maxInlinePromptBytes {
		return nil
	}
	cause := ErrPromptInvalid
	if stored {
		cause = ErrStoreCorrupt
	}
	if !utf8.ValidString(content) {
		return fmt.Errorf("%w: prompt content must be valid UTF-8", cause)
	}
	return fmt.Errorf("%w: inline prompt content exceeds %d bytes", cause, maxInlinePromptBytes)
}

func validateResolvedPromptContent(content string) error {
	switch {
	case content == "":
		return fmt.Errorf("%w: external content is empty", ErrPromptContentUnavailable)
	case !utf8.ValidString(content):
		return fmt.Errorf("%w: external content must be valid UTF-8", ErrPromptContentUnavailable)
	case len(content) > maxResolvedPromptBytes:
		return fmt.Errorf("%w: external content exceeds %d bytes", ErrPromptContentUnavailable, maxResolvedPromptBytes)
	default:
		return nil
	}
}

func verifyPromptContentHash(content, hash string, stored bool) error {
	want := PromptContentHash(content)
	if hash == want {
		return nil
	}
	if stored {
		return fmt.Errorf("%w: %w: got %q, want %q", ErrPromptContentHashMismatch, ErrStoreCorrupt, hash, want)
	}
	return fmt.Errorf("%w: got %q, want %q", ErrPromptContentHashMismatch, hash, want)
}

func validPromptHash(hash string) bool {
	if len(hash) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(hash, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(hash, "sha256:"))
	return err == nil
}

func computePromptHash(key PromptKey, contentHash string, variables []string) (string, error) {
	// PromptHash 同时绑定逻辑身份、正文哈希和变量契约，避免只 hash ref 造成配置漂移漏检。
	return hashJSON("prompt_hash", struct {
		Ref         string   `json:"prompt_ref"`
		Version     string   `json:"version"`
		ContentHash string   `json:"content_hash"`
		Variables   []string `json:"variables"`
	}{key.Ref, key.Version, contentHash, variables})
}

func promptSnapshotRef(key PromptKey, promptHash string) string {
	return strings.TrimSuffix(key.Ref, "/") + "@" + key.Version + "/snapshot/" + promptHash
}

func samePromptVersion(left, right PromptVersion) bool {
	return left.Ref == right.Ref &&
		left.Version == right.Version &&
		left.Content == right.Content &&
		left.ContentRef == right.ContentRef &&
		left.ContentHash == right.ContentHash &&
		reflect.DeepEqual(left.Variables, right.Variables)
}

func promptVersionConflict(key PromptKey) error {
	return fmt.Errorf("%w: %w: %s@%s", ErrPromptVersionExists, ErrStoreConflict, key.Ref, key.Version)
}

func clonePromptVersion(prompt PromptVersion) PromptVersion {
	prompt.Variables = cloneStrings(prompt.Variables)
	return prompt
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append(make([]string, 0, len(values)), values...)
}

var _ PromptStore = (*MemoryPromptStore)(nil)
var _ PromptResolver = (*storePromptResolver)(nil)
var _ agentruntime.SystemPromptResolver = (*RuntimePromptResolver)(nil)
