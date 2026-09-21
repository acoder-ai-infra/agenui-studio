package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/AGenUI/agenui-studio/harness/sdk/extension"
)

// ---- 测试替身 ---------------------------------------------------------------

type stubResolver struct {
	out     extension.ResolvedIdentity
	err     error
	panicOn bool
	seen    []extension.IdentityRequest
}

func (s *stubResolver) Resolve(_ context.Context, req extension.IdentityRequest) (extension.ResolvedIdentity, error) {
	s.seen = append(s.seen, req)
	if s.panicOn {
		panic("resolver boom")
	}
	return s.out, s.err
}

type stubInitializer struct {
	out extension.RunInitOutput
	err error
}

func (s *stubInitializer) Initialize(_ context.Context, _ extension.RunInitRequest) (extension.RunInitOutput, error) {
	return s.out, s.err
}

type stubContributor struct {
	fragments []extension.ContextFragment
	err       error
	order     *[]string
	name      string
}

func (s *stubContributor) Contribute(_ context.Context, _ extension.ContribRequest) ([]extension.ContextFragment, error) {
	if s.order != nil {
		*s.order = append(*s.order, s.name)
	}
	return s.fragments, s.err
}

type stubNormalizer struct {
	out   extension.NormalizedInput
	err   error
	calls int
	block time.Duration
}

func (s *stubNormalizer) ID() string { return "stub.normalizer" }

func (s *stubNormalizer) Normalize(ctx context.Context, _ extension.NormalizeRequest) (extension.NormalizedInput, error) {
	s.calls++
	if s.block > 0 {
		select {
		case <-time.After(s.block):
		case <-ctx.Done():
			return extension.NormalizedInput{}, ctx.Err()
		}
	}
	return s.out, s.err
}

func mustTurnPipeline(t *testing.T, entries ...ExtensionEntry) *TurnPipeline {
	t.Helper()
	catalog, err := NewExtensionCatalog(entries)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	return NewTurnPipeline(catalog, TurnEnvironment{
		Environment: "local", SDKVersion: SDKContractVersion, SchemaVersions: CanonicalSchemaVersions(),
	})
}

func textInput(text string) extension.NormalizedMessage {
	return extension.NormalizedMessage{
		Role:  "user",
		Parts: []extension.NormalizedPart{{Kind: "text", Text: text}},
	}
}

// ---- 测试 -------------------------------------------------------------------

// TestTurnPipelinePassthroughWithoutStages 验证空目录时输入原样直通。
func TestTurnPipelinePassthroughWithoutStages(t *testing.T) {
	pipeline := NewTurnPipeline(nil, TurnEnvironment{})
	if pipeline.HasStages() {
		t.Fatal("nil catalog must report no stages")
	}
	req := PrepareTurnRequest{
		Identity: TurnIdentity{TenantID: "t", UserID: "u"},
		Input:    textInput("hello"),
	}
	result, err := pipeline.PrepareTurn(context.Background(), req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if result.Identity != req.Identity || result.Input.Parts[0].Text != "hello" {
		t.Fatalf("passthrough mutated request: %+v", result)
	}
}

// TestTurnPipelineIdentityFoldAndBusinessFieldsStable 验证 resolver 折叠覆盖
// 规则，以及 BusinessXxx 恒为调用方原始身份。
func TestTurnPipelineIdentityFoldAndBusinessFieldsStable(t *testing.T) {
	first := &stubResolver{out: extension.ResolvedIdentity{TenantID: "tenant-a", SessionID: "sess-a"}}
	second := &stubResolver{out: extension.ResolvedIdentity{TenantID: "tenant-b"}}
	pipeline := mustTurnPipeline(t,
		ExtensionEntry{ID: "first", Kind: ExtIdentityResolver, Order: 1, Implementation: first},
		ExtensionEntry{ID: "second", Kind: ExtIdentityResolver, Order: 2, Implementation: second},
	)
	result, err := pipeline.PrepareTurn(context.Background(), PrepareTurnRequest{
		Identity: TurnIdentity{TenantID: "caller-tenant", UserID: "caller-user"},
		Metadata: map[string]string{"source": "test"},
		Input:    textInput("hi"),
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// 后注册者覆盖先注册者；未覆盖字段保留。
	if result.Identity.TenantID != "tenant-b" || result.Identity.SessionID != "sess-a" || result.Identity.UserID != "caller-user" {
		t.Fatalf("identity fold wrong: %+v", result.Identity)
	}
	// 每个 resolver 看到的 Business 字段都是调用方原始身份。
	if second.seen[0].BusinessTenantID != "caller-tenant" {
		t.Fatalf("BusinessTenantID drifted: %q", second.seen[0].BusinessTenantID)
	}
	// 但 Ctx 携带折叠后的当前身份。
	if second.seen[0].Ctx.TenantID != "tenant-a" {
		t.Fatalf("Ctx.TenantID should carry folded identity, got %q", second.seen[0].Ctx.TenantID)
	}
	if second.seen[0].Metadata["source"] != "test" {
		t.Fatalf("metadata not propagated: %+v", second.seen[0].Metadata)
	}
}

// TestTurnPipelineIdentityFailClosed 验证 resolver 错误 / panic / 类型不匹配
// 都 fail-closed。
func TestTurnPipelineIdentityFailClosed(t *testing.T) {
	sentinel := errors.New("resolver refused")
	cases := []struct {
		name    string
		impl    any
		wantIs  error
		wantInv bool
	}{
		{name: "error", impl: &stubResolver{err: sentinel}, wantIs: sentinel},
		{name: "panic", impl: &stubResolver{panicOn: true}, wantInv: true},
		{name: "type_mismatch", impl: struct{}{}, wantInv: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pipeline := mustTurnPipeline(t, ExtensionEntry{ID: "r", Kind: ExtIdentityResolver, Implementation: tc.impl})
			_, err := pipeline.PrepareTurn(context.Background(), PrepareTurnRequest{
				Identity: TurnIdentity{TenantID: "t"}, Input: textInput("x"),
			})
			if err == nil {
				t.Fatal("expected fail-closed error")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("business error chain lost: %v", err)
			}
			if tc.wantInv && !errors.Is(err, ErrTurnPipelineInvalid) {
				t.Fatalf("expected ErrTurnPipelineInvalid, got %v", err)
			}
		})
	}
}

// TestTurnPipelineRunInitializerMergesScopedDataAndRefs 验证 ScopedData 合并、
// 空 key 拒绝与 ArtifactRefs 追加为 parts。
func TestTurnPipelineRunInitializerMergesScopedDataAndRefs(t *testing.T) {
	init := &stubInitializer{out: extension.RunInitOutput{
		ScopedData: map[string]extension.ScopedDataEntry{
			"page_state": {Source: "biz", Value: []byte(`{"step":1}`)},
		},
		ArtifactRefs: []extension.ArtifactRef{{ID: "art_1", MIME: "image/png", Hash: "h1"}},
	}}
	pipeline := mustTurnPipeline(t, ExtensionEntry{ID: "init", Kind: ExtRunInitializer, Implementation: init})
	result, err := pipeline.PrepareTurn(context.Background(), PrepareTurnRequest{
		Identity: TurnIdentity{TenantID: "t"}, Input: textInput("x"),
		ScopedData: map[string]extension.ScopedDataEntry{"seed": {Source: "caller", Value: []byte(`1`)}},
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, ok := result.ScopedData["seed"]; !ok {
		t.Fatal("seed scoped data lost")
	}
	if _, ok := result.ScopedData["page_state"]; !ok {
		t.Fatal("initializer scoped data not merged")
	}
	last := result.Input.Parts[len(result.Input.Parts)-1]
	if last.Kind != "artifact_ref" || last.Ref.ID != "art_1" || last.MIME != "image/png" {
		t.Fatalf("artifact ref not appended as part: %+v", last)
	}

	bad := &stubInitializer{out: extension.RunInitOutput{
		ScopedData: map[string]extension.ScopedDataEntry{"": {Source: "biz"}},
	}}
	pipeline = mustTurnPipeline(t, ExtensionEntry{ID: "bad", Kind: ExtRunInitializer, Implementation: bad})
	if _, err := pipeline.PrepareTurn(context.Background(), PrepareTurnRequest{Input: textInput("x")}); !errors.Is(err, ErrTurnPipelineInvalid) {
		t.Fatalf("empty ScopedData key must fail closed, got %v", err)
	}
}

// TestTurnPipelineContributorsOrderedAndDigest 验证 contributor 按 Order 执行、
// fragment 拼接与 metadata 摘要写入。
func TestTurnPipelineContributorsOrderedAndDigest(t *testing.T) {
	var order []string
	pipeline := mustTurnPipeline(t,
		ExtensionEntry{ID: "b", Kind: ExtContextContributor, Order: 2, Implementation: &stubContributor{
			name: "b", order: &order,
			fragments: []extension.ContextFragment{{Kind: "k2", Source: extension.FragmentSourceKnowledge, Text: "t2"}},
		}},
		ExtensionEntry{ID: "a", Kind: ExtContextContributor, Order: 1, Implementation: &stubContributor{
			name: "a", order: &order,
			fragments: []extension.ContextFragment{{Kind: "k1", Source: extension.FragmentSourceBusiness, Text: "t1"}},
		}},
	)
	result, err := pipeline.PrepareTurn(context.Background(), PrepareTurnRequest{Input: textInput("x")})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("contributor order wrong: %v", order)
	}
	if len(result.Fragments) != 2 || result.Fragments[0].Kind != "k1" {
		t.Fatalf("fragments wrong: %+v", result.Fragments)
	}
	if result.Input.Metadata["harness.context_contributions"] != "2" {
		t.Fatalf("contribution metadata missing: %+v", result.Input.Metadata)
	}
	if result.Input.Metadata["harness.context_fragment_digest"] == "" {
		t.Fatal("fragment digest missing")
	}
}

// TestTurnPipelineNormalizerReplacesInputOnce 验证单 normalizer 执行一次、
// 替换输入并记录 metadata；空 parts 输出 fail-closed；错误链透传。
func TestTurnPipelineNormalizerReplacesInputOnce(t *testing.T) {
	norm := &stubNormalizer{out: extension.NormalizedInput{
		Message:    textInput("NORM|hello"),
		SourceRefs: []extension.SourceRef{{Kind: "scoped_data", Key: "src"}},
	}}
	pipeline := mustTurnPipeline(t, ExtensionEntry{ID: "norm", Kind: ExtInputNormalizer, Implementation: norm})
	result, err := pipeline.PrepareTurn(context.Background(), PrepareTurnRequest{Input: textInput("hello")})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if norm.calls != 1 {
		t.Fatalf("normalizer calls = %d; want 1", norm.calls)
	}
	if result.Input.Parts[0].Text != "NORM|hello" {
		t.Fatalf("input not replaced: %+v", result.Input.Parts)
	}
	if result.NormalizerID != "norm" || result.Input.Metadata["harness.input_normalizer"] != "norm" {
		t.Fatalf("normalizer metadata missing: %+v", result.Input.Metadata)
	}
	if result.Input.Metadata["harness.input_source_refs"] == "" {
		t.Fatal("source refs digest missing")
	}

	empty := &stubNormalizer{out: extension.NormalizedInput{}}
	pipeline = mustTurnPipeline(t, ExtensionEntry{ID: "empty", Kind: ExtInputNormalizer, Implementation: empty})
	if _, err := pipeline.PrepareTurn(context.Background(), PrepareTurnRequest{Input: textInput("x")}); !errors.Is(err, ErrTurnPipelineInvalid) {
		t.Fatalf("empty normalizer output must fail closed, got %v", err)
	}

	sentinel := errors.New("normalizer refused")
	failing := &stubNormalizer{err: sentinel}
	pipeline = mustTurnPipeline(t, ExtensionEntry{ID: "fail", Kind: ExtInputNormalizer, Implementation: failing})
	if _, err := pipeline.PrepareTurn(context.Background(), PrepareTurnRequest{Input: textInput("x")}); !errors.Is(err, sentinel) {
		t.Fatalf("normalizer business error chain lost: %v", err)
	}
}

// chainNormalizer 基于输入变换：给首个 text part 加前缀，并申报一个 SourceRef。
type chainNormalizer struct {
	id     string
	prefix string
	refs   []extension.SourceRef
}

func (c *chainNormalizer) ID() string { return c.id }

func (c *chainNormalizer) Normalize(_ context.Context, req extension.NormalizeRequest) (extension.NormalizedInput, error) {
	msg := req.RawInput
	if len(msg.Parts) > 0 {
		msg.Parts[0].Text = c.prefix + msg.Parts[0].Text
	}
	return extension.NormalizedInput{Message: msg, SourceRefs: c.refs}, nil
}

// TestTurnPipelineNormalizerChain 验证多 normalizer 按注册序链式执行：
// 前一个输出作为后一个输入、NormalizerID 记录执行链、SourceRefs 累积去重。
func TestTurnPipelineNormalizerChain(t *testing.T) {
	na := &chainNormalizer{id: "na", prefix: "A|", refs: []extension.SourceRef{
		{Kind: "scoped_data", Key: "shared", Hash: "h1"},
	}}
	nb := &chainNormalizer{id: "nb", prefix: "B|", refs: []extension.SourceRef{
		{Kind: "scoped_data", Key: "shared", Hash: "h1"}, // 与 na 重复，应去重
		{Kind: "artifact", Key: "art_9", Hash: "h2"},
	}}
	pipeline := mustTurnPipeline(t,
		ExtensionEntry{ID: "na", Kind: ExtInputNormalizer, Implementation: na},
		ExtensionEntry{ID: "nb", Kind: ExtInputNormalizer, Implementation: nb},
	)
	result, err := pipeline.PrepareTurn(context.Background(), PrepareTurnRequest{Input: textInput("hello")})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if got := result.Input.Parts[0].Text; got != "B|A|hello" {
		t.Fatalf("chain output = %q; want B|A|hello", got)
	}
	if result.NormalizerID != "na,nb" || result.Input.Metadata["harness.input_normalizer"] != "na,nb" {
		t.Fatalf("normalizer chain metadata wrong: id=%q metadata=%+v", result.NormalizerID, result.Input.Metadata)
	}
	var refs []extension.SourceRef
	if err := json.Unmarshal([]byte(result.Input.Metadata["harness.input_source_refs"]), &refs); err != nil {
		t.Fatalf("source refs digest unmarshal: %v", err)
	}
	if len(refs) != 2 || refs[0].Key != "shared" || refs[1].Key != "art_9" {
		t.Fatalf("source refs not deduped/accumulated: %+v", refs)
	}
}

// TestTurnPipelineEntryTimeoutApplies 验证 per-entry 超时约束慢扩展。
func TestTurnPipelineEntryTimeoutApplies(t *testing.T) {
	slow := &stubNormalizer{block: 5 * time.Second, out: extension.NormalizedInput{Message: textInput("x")}}
	pipeline := mustTurnPipeline(t, ExtensionEntry{
		ID: "slow", Kind: ExtInputNormalizer, Timeout: 50 * time.Millisecond, Implementation: slow,
	})
	start := time.Now()
	_, err := pipeline.PrepareTurn(context.Background(), PrepareTurnRequest{Input: textInput("x")})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout not applied; took %v", elapsed)
	}
}

// TestNormalizedMessagePreview 验证预览摊平语义与 SDK renderPreview 对齐。
func TestNormalizedMessagePreview(t *testing.T) {
	msg := extension.NormalizedMessage{Parts: []extension.NormalizedPart{
		{Kind: "text", Text: "hello"},
		{Kind: "json", JSON: []byte(`{"a":1}`)},
		{Kind: "artifact_ref", Ref: extension.ArtifactRef{ID: "art_9"}},
		{Kind: "inline_binary", Inline: []byte{1, 2, 3}},
	}}
	got := NormalizedMessagePreview(msg)
	want := "hello\n{\"a\":1}\n[artifact:art_9]\n[inline:3 bytes]"
	if got != want {
		t.Fatalf("preview = %q; want %q", got, want)
	}
}
