package modelgateway_test

import (
	"context"
	"sync"
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
	"github.com/AGenUI/agenui-studio/harness/internal/modelgateway/mock"
	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

// fakeUsageLedger 捕获评估类调用的用量记录。
type fakeUsageLedger struct {
	mu      sync.Mutex
	records []*storage.ModelUsageRecord
}

func (l *fakeUsageLedger) Record(_ context.Context, r *storage.ModelUsageRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r)
	return nil
}

func (l *fakeUsageLedger) List(context.Context, storage.ModelUsageQuery) ([]*storage.ModelUsageRecord, error) {
	return nil, nil
}

func (l *fakeUsageLedger) Summary(context.Context, storage.ModelUsageQuery) (storage.ModelUsageSummary, error) {
	return storage.ModelUsageSummary{}, nil
}

// 评估类调用（embedding/rerank）必须写入用量账本，并带上租户/trace 归属。
func TestFacadeEvaluationRecordsUsage(t *testing.T) {
	ledger := &fakeUsageLedger{}
	gw := &mg.Facade{
		Providers:   map[string]mg.ChatProvider{"mock": mock.New("mock")},
		UsageLedger: ledger,
	}
	trace := observability.TraceContext{TenantID: "t1", TraceID: "tr1", SessionID: "s1", RunID: "run1"}
	if _, err := gw.Embedding(context.Background(), mg.EmbeddingRequest{
		RequestID: "e1", Trace: trace, AgentID: "agent_a", Texts: []string{"abc"}, ModelHint: "mock-embed",
	}); err != nil {
		t.Fatalf("embedding: %v", err)
	}
	if _, err := gw.Rerank(context.Background(), mg.RerankRequest{
		RequestID: "r1", Trace: trace, AgentID: "agent_a", Query: "q", Documents: []string{"a"}, ModelHint: "mock-rerank",
	}); err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(ledger.records) != 2 {
		t.Fatalf("want 2 usage records, got %d", len(ledger.records))
	}
	if ledger.records[0].ID != "e1:embedding" || ledger.records[0].TenantID != "t1" || ledger.records[0].AgentID != "agent_a" {
		t.Fatalf("embedding usage record = %+v", ledger.records[0])
	}
	if ledger.records[1].ID != "r1:rerank" || ledger.records[1].RunID != "run1" {
		t.Fatalf("rerank usage record = %+v", ledger.records[1])
	}
}

// provider 选择必须按名称序确定性命中，不依赖 map 迭代序。
func TestFacadeEvaluationDeterministicProvider(t *testing.T) {
	gw := &mg.Facade{Providers: map[string]mg.ChatProvider{
		"z_mock": mock.New("z_mock"),
		"a_mock": mock.New("a_mock"),
	}}
	resp, err := gw.Embedding(context.Background(), mg.EmbeddingRequest{
		RequestID: "e1", Texts: []string{"x"}, ModelHint: "m",
	})
	if err != nil {
		t.Fatalf("embedding: %v", err)
	}
	if resp.Provider != "a_mock" {
		t.Fatalf("provider selection must be name-ordered, got %s", resp.Provider)
	}
}
