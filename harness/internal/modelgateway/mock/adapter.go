// Package mock 提供 MockProvider —— 模型网关的离线 CI/测试后端。
// 它是脚本化的:chunk 序列、usage、错误都可确定性注入。仅供 dev/test。
package mock

import (
	"context"
	"sync"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// MockProvider 是脚本化的 ChatProvider。
type MockProvider struct {
	id string

	mu      sync.Mutex
	scripts []Script // 每次 InvokeChat 消费一个脚本
	calls   int
}

var _ mg.ChatProvider = (*MockProvider)(nil)

// New 构建一个 MockProvider。每次 InvokeChat 顺序消费下一个脚本;脚本用尽后复用最后一个
// (一个脚本即可服务无限次尝试)。
func New(id string, scripts ...Script) *MockProvider {
	if id == "" {
		id = "mock_provider"
	}
	return &MockProvider{id: id, scripts: scripts}
}

func (m *MockProvider) ID() string { return m.id }

func (m *MockProvider) InvokeChat(_ context.Context, _ mg.AdapterRequest) (mg.AdapterStream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var script Script
	switch {
	case len(m.scripts) == 0:
		script = Script{}
	case m.calls < len(m.scripts):
		script = m.scripts[m.calls]
	default:
		script = m.scripts[len(m.scripts)-1]
	}
	m.calls++

	if script.InvokeErr != nil {
		return nil, script.InvokeErr
	}
	return &scriptStream{chunks: script.Chunks}, nil
}

// Calls 报告 InvokeChat 被调用的次数(测试辅助)。
func (m *MockProvider) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *MockProvider) Embed(_ context.Context, req mg.EmbeddingRequest) (*mg.EmbeddingResponse, error) {
	out := make([][]float64, 0, len(req.Texts))
	for _, text := range req.Texts {
		out = append(out, []float64{float64(len([]rune(text)))})
	}
	return &mg.EmbeddingResponse{
		RequestID: req.RequestID, Provider: m.id, Model: "mock-embedding",
		Embeddings: out,
		Usage:      mg.ModelUsage{PromptTokens: len(req.Texts), Source: mg.UsageSourceEstimated},
	}, nil
}

func (m *MockProvider) Rerank(_ context.Context, req mg.RerankRequest) (*mg.RerankResponse, error) {
	results := make([]mg.RerankResult, 0, len(req.Documents))
	for i := range req.Documents {
		results = append(results, mg.RerankResult{Index: i, Score: float64(len(req.Documents) - i)})
	}
	return &mg.RerankResponse{
		RequestID: req.RequestID, Provider: m.id, Model: "mock-rerank",
		Results: results,
		Usage:   mg.ModelUsage{PromptTokens: len(req.Documents) + 1, Source: mg.UsageSourceEstimated},
	}, nil
}

func (m *MockProvider) Judge(_ context.Context, req mg.JudgeRequest) (*mg.JudgeResponse, error) {
	score := 0.0
	if req.Output != "" {
		score = 1.0
	}
	return &mg.JudgeResponse{
		RequestID: req.RequestID, Provider: m.id, Model: "mock-judge",
		Score: score, Reason: "mock judge",
		Usage: mg.ModelUsage{PromptTokens: 1, CompletionTokens: 1, Source: mg.UsageSourceEstimated},
	}, nil
}
