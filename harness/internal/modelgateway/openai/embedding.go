package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

var _ mg.EmbeddingProvider = (*Adapter)(nil)

// Embed 实现 mg.EmbeddingProvider：走 OpenAI 兼容 /embeddings 协议
// （DashScope compatible-mode 的 text-embedding-v3 等同款形状）。
// 模型由调用方 ModelHint 显式指定；provider 对不服务的模型返回 4xx，
// 网关层不做降级。
func (a *Adapter) Embed(ctx context.Context, req mg.EmbeddingRequest) (*mg.EmbeddingResponse, error) {
	if len(req.Texts) == 0 {
		return nil, &mg.AdapterError{Message: "embedding input texts are required", Class: mg.ErrorProvider4xx, Retryable: false}
	}
	body, err := json.Marshal(map[string]any{
		"model": req.ModelHint,
		"input": req.Texts,
	})
	if err != nil {
		return nil, err
	}
	resp, err := a.client.PostJSON(ctx, "/embeddings", body)
	if err != nil {
		return nil, &mg.AdapterError{Message: err.Error(), Class: mg.ErrorNetwork, Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, embeddingHTTPError(resp.StatusCode, errBody)
	}
	var parsed struct {
		Model string `json:"model"`
		Data  []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, &mg.AdapterError{Message: "invalid embeddings response: " + err.Error(), Class: mg.ErrorProvider5xx, Retryable: false}
	}
	if len(parsed.Data) != len(req.Texts) {
		return nil, &mg.AdapterError{
			Message:   fmt.Sprintf("embeddings response size mismatch: want %d got %d", len(req.Texts), len(parsed.Data)),
			Class:     mg.ErrorProvider5xx,
			Retryable: false,
		}
	}
	// 按 index 归位：OpenAI 协议不保证 data 有序。
	sort.Slice(parsed.Data, func(i, j int) bool { return parsed.Data[i].Index < parsed.Data[j].Index })
	embeddings := make([][]float64, 0, len(parsed.Data))
	for _, item := range parsed.Data {
		embeddings = append(embeddings, item.Embedding)
	}
	promptTokens := parsed.Usage.PromptTokens
	if promptTokens == 0 {
		promptTokens = parsed.Usage.TotalTokens
	}
	model := parsed.Model
	if model == "" {
		model = req.ModelHint
	}
	return &mg.EmbeddingResponse{
		RequestID:  req.RequestID,
		Provider:   a.id,
		Model:      model,
		Embeddings: embeddings,
		Usage:      mg.ModelUsage{PromptTokens: promptTokens, Source: mg.UsageSourceGateway},
	}, nil
}

// embeddingHTTPError 复用 chat 路径的错误分级约定：429/5xx 可重试，其余 4xx 不重试。
func embeddingHTTPError(status int, body []byte) *mg.AdapterError {
	retryable := status == http.StatusTooManyRequests || status >= 500
	class := mg.ErrorProvider4xx
	if status == http.StatusTooManyRequests {
		class = mg.ErrorRateLimited
	} else if status >= 500 {
		class = mg.ErrorProvider5xx
	}
	return &mg.AdapterError{
		Message:    fmt.Sprintf("openai embeddings http %d: %s", status, string(body)),
		Class:      class,
		HTTPStatus: status,
		Retryable:  retryable,
	}
}
