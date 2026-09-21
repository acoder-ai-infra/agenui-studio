package agentregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/harness/internal/agentruntime"
)

func TestMemoryPromptStoreKeepsVersionsImmutableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryPromptStore()
	variables := []string{"user_profile", "current_date"}
	prompt := PromptVersion{
		Ref: "prompt://travel/system", Version: "v1", Content: "你是旅行规划助手。",
		Variables: variables, CreatedBy: "publisher",
	}
	if err := store.Create(ctx, prompt); err != nil {
		t.Fatalf("create prompt: %v", err)
	}

	// 相同版本的重复发布是幂等操作，不会覆盖第一次发布的元数据。
	if err := store.Create(ctx, prompt); err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	variables[0] = "tampered"
	got, err := store.Get(ctx, PromptKey{Ref: prompt.Ref, Version: prompt.Version})
	if err != nil {
		t.Fatalf("get prompt: %v", err)
	}
	if got.ContentHash != PromptContentHash(prompt.Content) {
		t.Fatalf("content hash = %q", got.ContentHash)
	}
	if strings.Join(got.Variables, ",") != "current_date,user_profile" {
		t.Fatalf("variables were not canonicalized: %#v", got.Variables)
	}
	got.Variables[0] = "tampered"
	again, _ := store.Get(ctx, PromptKey{Ref: prompt.Ref, Version: prompt.Version})
	if again.Variables[0] == "tampered" {
		t.Fatal("get leaked mutable variable slice")
	}

	changed := prompt
	changed.Content = "你是另一个助手。"
	if err := store.Create(ctx, changed); !errors.Is(err, ErrPromptVersionExists) || !errors.Is(err, ErrStoreConflict) {
		t.Fatalf("changed immutable version error = %v", err)
	}
}

func TestMemoryPromptStoreListsNewestVersionsWithoutLeakingState(t *testing.T) {
	store := NewMemoryPromptStore()
	ref := "prompt://managed/system"
	for _, prompt := range []PromptVersion{
		{Ref: ref, Version: "v1", Content: "first", Variables: []string{"name"}, CreatedAt: time.Unix(1, 0)},
		{Ref: ref, Version: "v2", Content: "second", Variables: []string{"name"}, CreatedAt: time.Unix(2, 0)},
	} {
		if err := store.Create(context.Background(), prompt); err != nil {
			t.Fatalf("create %s: %v", prompt.Version, err)
		}
	}
	versions, err := store.ListVersions(context.Background(), ref)
	if err != nil {
		t.Fatalf("list prompt versions: %v", err)
	}
	if len(versions) != 2 || versions[0].Version != "v2" || versions[1].Version != "v1" {
		t.Fatalf("versions are not newest first: %#v", versions)
	}
	versions[0].Variables[0] = "tampered"
	again, err := store.ListVersions(context.Background(), ref)
	if err != nil || again[0].Variables[0] != "name" {
		t.Fatalf("list leaked mutable state: versions=%#v err=%v", again, err)
	}
	missing, err := store.ListVersions(context.Background(), "prompt://missing/system")
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing prompt versions = %#v, %v", missing, err)
	}
}

func TestMemoryPromptStoreConcurrentCreateIsAtomic(t *testing.T) {
	const workers = 32
	t.Run("same payload is idempotent", func(t *testing.T) {
		store := NewMemoryPromptStore()
		prompt := PromptVersion{Ref: "prompt://concurrent/system", Version: "v1", Content: "same"}
		errs := make(chan error, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- store.Create(context.Background(), prompt)
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("idempotent create: %v", err)
			}
		}
	})

	t.Run("different payloads have one winner", func(t *testing.T) {
		store := NewMemoryPromptStore()
		errs := make(chan error, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs <- store.Create(context.Background(), PromptVersion{
					Ref: "prompt://concurrent/system", Version: "v1", Content: fmt.Sprintf("body-%d", i),
				})
			}(i)
		}
		wg.Wait()
		close(errs)
		succeeded, conflicted := 0, 0
		for err := range errs {
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ErrPromptVersionExists), errors.Is(err, ErrStoreConflict):
				conflicted++
			default:
				t.Fatalf("unexpected create error: %v", err)
			}
		}
		if succeeded != 1 || conflicted != workers-1 {
			t.Fatalf("create results: succeeded=%d conflicted=%d", succeeded, conflicted)
		}
	})
}

func TestPromptResolverBuildsFrozenRuntimeSnapshot(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryPromptStore()
	prompt := PromptVersion{
		Ref: "prompt://travel/system", Version: "v4", Content: "你是旅行规划助手。",
		Variables: []string{"current_date", "user_profile"},
	}
	if err := store.Create(ctx, prompt); err != nil {
		t.Fatalf("create prompt: %v", err)
	}
	resolver, err := NewPromptResolver(store)
	if err != nil {
		t.Fatalf("new resolver: %v", err)
	}
	snapshot, err := resolver.Resolve(ctx, PromptKey{Ref: prompt.Ref, Version: prompt.Version})
	if err != nil {
		t.Fatalf("resolve prompt: %v", err)
	}
	if snapshot.Content != prompt.Content || snapshot.ContentHash != PromptContentHash(prompt.Content) {
		t.Fatalf("resolved prompt = %#v", snapshot)
	}
	if snapshot.PromptHash == "" || snapshot.SnapshotRef == "" {
		t.Fatalf("snapshot is not frozen: %#v", snapshot)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if strings.Contains(string(encoded), prompt.Content) {
		t.Fatalf("serialized snapshot leaked system prompt body: %s", encoded)
	}

	runtimeResolver, err := NewRuntimePromptResolver(resolver)
	if err != nil {
		t.Fatalf("new runtime resolver: %v", err)
	}
	runtimeSnapshot, err := runtimeResolver.ResolveSystemPrompt(ctx, agentruntime.SystemPromptResolveRequest{
		Ref: prompt.Ref, Version: prompt.Version,
	})
	if err != nil {
		t.Fatalf("resolve runtime prompt: %v", err)
	}
	if runtimeSnapshot.PromptHash != snapshot.PromptHash || runtimeSnapshot.Content != prompt.Content {
		t.Fatalf("runtime adapter drift: %#v", runtimeSnapshot)
	}
	_, err = runtimeResolver.ResolveSystemPrompt(ctx, agentruntime.SystemPromptResolveRequest{
		Ref: prompt.Ref, Version: prompt.Version, ExpectedPromptHash: "sha256:stale",
	})
	if !errors.Is(err, agentruntime.ErrSystemPromptSnapshotMismatch) {
		t.Fatalf("runtime adapter accepted stale prompt hash: %v", err)
	}
	_, err = runtimeResolver.ResolveSystemPrompt(ctx, agentruntime.SystemPromptResolveRequest{
		Ref: prompt.Ref, Version: prompt.Version, ExpectedContentHash: PromptContentHash("stale"),
	})
	if !errors.Is(err, agentruntime.ErrSystemPromptSnapshotMismatch) {
		t.Fatalf("runtime adapter accepted stale content hash: %v", err)
	}
}

func TestPromptResolverLoadsAndVerifiesExternalContent(t *testing.T) {
	ctx := context.Background()
	content := "只使用受信任工具。"
	store := NewMemoryPromptStore()
	prompt := PromptVersion{
		Ref: "prompt://guard/system", Version: "v1",
		ContentRef: "artifact://prompt/guard/v1", ContentHash: PromptContentHash(content),
	}
	if err := store.Create(ctx, prompt); err != nil {
		t.Fatalf("create external prompt: %v", err)
	}

	withoutContent, _ := NewPromptResolver(store)
	if _, err := withoutContent.Resolve(ctx, PromptKey{Ref: prompt.Ref, Version: prompt.Version}); !errors.Is(err, ErrPromptContentUnavailable) {
		t.Fatalf("missing content resolver error = %v", err)
	} else if strings.Contains(err.Error(), prompt.ContentRef) {
		t.Fatalf("content_ref leaked in missing resolver error: %v", err)
	}
	resolver, _ := NewPromptResolver(store, promptContentResolverFunc(func(_ context.Context, ref string) (string, error) {
		if ref != prompt.ContentRef {
			t.Fatalf("content ref = %q", ref)
		}
		return content, nil
	}))
	snapshot, err := resolver.Resolve(ctx, PromptKey{Ref: prompt.Ref, Version: prompt.Version})
	if err != nil || snapshot.Content != content {
		t.Fatalf("external snapshot = %#v, %v", snapshot, err)
	}

	tampered, _ := NewPromptResolver(store, promptContentResolverFunc(func(context.Context, string) (string, error) {
		return "tampered", nil
	}))
	if _, err := tampered.Resolve(ctx, PromptKey{Ref: prompt.Ref, Version: prompt.Version}); !errors.Is(err, ErrPromptContentHashMismatch) || !errors.Is(err, ErrStoreCorrupt) {
		t.Fatalf("tampered external content error = %v", err)
	}
}

func TestPromptStoreRejectsInvalidDataAndDetectsCorruption(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryPromptStore()
	invalidUTF8 := string([]byte{0xff})
	tests := []PromptVersion{
		{Ref: "", Version: "v1", Content: "body"},
		{Ref: "artifact://x", Version: "v1", Content: "body"},
		{Ref: "prompt://x/path with space", Version: "v1", Content: "body"},
		{Ref: invalidUTF8, Version: "v1", Content: "body"},
		{Ref: "prompt://x", Version: "", Content: "body"},
		{Ref: "prompt://x", Version: invalidUTF8, Content: "body"},
		{Ref: "prompt://x", Version: "v1"},
		{Ref: "prompt://x", Version: "v1", Content: "body", ContentRef: "artifact://body"},
		{Ref: "prompt://x", Version: "v1", Content: invalidUTF8},
		{Ref: "prompt://x", Version: "v1", ContentRef: invalidUTF8, ContentHash: PromptContentHash("body")},
		{Ref: "prompt://x", Version: "v1", Content: "body", ContentHash: PromptContentHash("other")},
		{Ref: "prompt://x", Version: "v1", Content: "body", Variables: []string{"name", "name"}},
		{Ref: "prompt://x", Version: "v1", Content: "body", Variables: []string{invalidUTF8}},
		{Ref: "prompt://x", Version: "v1", Content: "body", Variables: []string{strings.Repeat("v", maxPromptVariableRunes+1)}},
		{Ref: "prompt://x", Version: "v1", Content: "body", Variables: make([]string, maxPromptVariables+1)},
		{Ref: "prompt://x/" + strings.Repeat("界", maxPromptRefRunes), Version: "v1", Content: "body"},
		{Ref: "prompt://x", Version: strings.Repeat("版", maxPromptVersionRunes+1), Content: "body"},
		{Ref: "prompt://x", Version: "v1", ContentRef: strings.Repeat("路", maxPromptContentRefRunes+1), ContentHash: PromptContentHash("body")},
		{Ref: "prompt://x", Version: "v1", Content: "body", CreatedBy: strings.Repeat("人", maxPromptCreatorRunes+1)},
		{Ref: "prompt://x", Version: "v1", Content: "body", CreatedBy: invalidUTF8},
		{Ref: "prompt://x", Version: "v1", Content: strings.Repeat("x", maxInlinePromptBytes+1)},
	}
	for i, prompt := range tests {
		if err := store.Create(ctx, prompt); err == nil {
			t.Fatalf("case %d accepted invalid prompt: %#v", i, prompt)
		}
	}

	valid := PromptVersion{Ref: "prompt://x", Version: "v1", Content: "body"}
	if err := store.Create(ctx, valid); err != nil {
		t.Fatalf("create valid prompt: %v", err)
	}
	key := PromptKey{Ref: valid.Ref, Version: valid.Version}
	store.mu.Lock()
	corrupt := store.prompts[key]
	corrupt.Content = "tampered"
	store.prompts[key] = corrupt
	store.mu.Unlock()
	if _, err := store.Get(ctx, key); !errors.Is(err, ErrPromptContentHashMismatch) || !errors.Is(err, ErrStoreCorrupt) {
		t.Fatalf("stored corruption error = %v", err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Get(canceled, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled get error = %v", err)
	}
}

func TestPromptStoreAcceptsContractBoundaries(t *testing.T) {
	ctx := context.Background()
	variables := make([]string, maxPromptVariables)
	for i := range variables {
		variables[i] = fmt.Sprintf("variable_%03d", i)
	}
	prefix := "prompt://x/"
	prompt := PromptVersion{
		Ref:       prefix + strings.Repeat("x", maxPromptRefRunes-utf8.RuneCountInString(prefix)),
		Version:   strings.Repeat("版", maxPromptVersionRunes),
		Content:   strings.Repeat("x", maxInlinePromptBytes),
		Variables: variables,
		CreatedBy: strings.Repeat("人", maxPromptCreatorRunes),
	}
	store := NewMemoryPromptStore()
	if err := store.Create(ctx, prompt); err != nil {
		t.Fatalf("create prompt at contract boundaries: %v", err)
	}

	external := PromptVersion{
		Ref: "prompt://external/boundary", Version: "v1",
		ContentRef: strings.Repeat("路", maxPromptContentRefRunes), ContentHash: PromptContentHash("body"),
		Variables: []string{strings.Repeat("v", maxPromptVariableRunes)},
	}
	if err := store.Create(ctx, external); err != nil {
		t.Fatalf("create external prompt at contract boundaries: %v", err)
	}
}

func TestPromptResolverValidatesExternalContentWithoutLeakingContentRef(t *testing.T) {
	ctx := context.Background()
	secretRef := "artifact://private/prompt?token=secret"
	store := NewMemoryPromptStore()
	if err := store.Create(ctx, PromptVersion{
		Ref: "prompt://external/system", Version: "v1", ContentRef: secretRef,
		ContentHash: PromptContentHash("expected"),
	}); err != nil {
		t.Fatalf("create external prompt: %v", err)
	}

	tests := []struct {
		name    string
		resolve func(context.Context, string) (string, error)
	}{
		{name: "resolver error", resolve: func(context.Context, string) (string, error) {
			return "", errors.New("read failed for " + secretRef)
		}},
		{name: "empty", resolve: func(context.Context, string) (string, error) { return "", nil }},
		{name: "invalid utf8", resolve: func(context.Context, string) (string, error) {
			return string([]byte{0xff}), nil
		}},
		{name: "oversized", resolve: func(context.Context, string) (string, error) {
			return strings.Repeat("x", maxResolvedPromptBytes+1), nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver, err := NewPromptResolver(store, promptContentResolverFunc(test.resolve))
			if err != nil {
				t.Fatalf("new resolver: %v", err)
			}
			_, err = resolver.Resolve(ctx, PromptKey{Ref: "prompt://external/system", Version: "v1"})
			if !errors.Is(err, ErrPromptContentUnavailable) {
				t.Fatalf("external content error = %v", err)
			}
			if strings.Contains(err.Error(), secretRef) {
				t.Fatalf("content_ref leaked in error: %v", err)
			}
		})
	}

	canceled, err := NewPromptResolver(store, promptContentResolverFunc(func(context.Context, string) (string, error) {
		return "", fmt.Errorf("resolver failed for %s: %w", secretRef, context.Canceled)
	}))
	if err != nil {
		t.Fatalf("new canceled resolver: %v", err)
	}
	_, err = canceled.Resolve(ctx, PromptKey{Ref: "prompt://external/system", Version: "v1"})
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), secretRef) {
		t.Fatalf("canceled resolver error = %v", err)
	}
}

func TestPromptHashBindsIdentityContentAndVariables(t *testing.T) {
	base := PromptKey{Ref: "prompt://x", Version: "v1"}
	contentHash := PromptContentHash("body")
	first, _ := computePromptHash(base, contentHash, []string{"a"})
	tests := []struct {
		key       PromptKey
		hash      string
		variables []string
	}{
		{PromptKey{Ref: "prompt://y", Version: "v1"}, contentHash, []string{"a"}},
		{PromptKey{Ref: "prompt://x", Version: "v2"}, contentHash, []string{"a"}},
		{base, PromptContentHash("other"), []string{"a"}},
		{base, contentHash, []string{"b"}},
	}
	for _, test := range tests {
		got, _ := computePromptHash(test.key, test.hash, test.variables)
		if got == first {
			t.Fatalf("prompt hash ignored input: %#v", test)
		}
	}
}

type promptContentResolverFunc func(context.Context, string) (string, error)

func (f promptContentResolverFunc) ResolvePromptContent(ctx context.Context, ref string) (string, error) {
	return f(ctx, ref)
}
