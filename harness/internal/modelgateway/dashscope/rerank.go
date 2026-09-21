// Package dashscope 提供 DashScope 原生协议的评估类 provider。
// rerank（gte-rerank 系列）不在 OpenAI compatible-mode 下提供，需走
// /api/v1/services/rerank/text-rerank/text-rerank 原生端点，故独立成包。
package dashscope

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

const rerankPath = "/api/v1/services/rerank/text-rerank/text-rerank"

// RerankAdapter 是 DashScope 原生 rerank provider。它同时实现 ChatProvider
// 形状以进入 Facade.Providers 注册表，但不承接 chat 调用。
type RerankAdapter struct {
	id      string
	baseURL string
	apiKey  string
	http    *http.Client
}

var (
	_ mg.ChatProvider   = (*RerankAdapter)(nil)
	_ mg.RerankProvider = (*RerankAdapter)(nil)
)

// NewRerank 构建指向 baseURL（如 https://dashscope.aliyuncs.com）的原生
// rerank 适配器。超时由 ctx 驱动，客户端不设全局 Timeout。
func NewRerank(id, baseURL, apiKey string) *RerankAdapter {
	if id == "" {
		id = "dashscope_rerank"
	}
	return &RerankAdapter{
		id:      id,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{},
	}
}

func (a *RerankAdapter) ID() string { return a.id }

// InvokeChat 满足注册表形状；rerank provider 不服务 chat，fail closed。
func (a *RerankAdapter) InvokeChat(context.Context, mg.AdapterRequest) (mg.AdapterStream, error) {
	return nil, &mg.AdapterError{
		Message:   "dashscope rerank provider does not serve chat",
		Class:     mg.ErrorProvider4xx,
		Retryable: false,
	}
}

// Rerank 调用 DashScope 原生 text-rerank 端点，把结果按相关性降序归一为
// mg.RerankResult（Index 指向入参 Documents 的下标）。
func (a *RerankAdapter) Rerank(ctx context.Context, req mg.RerankRequest) (*mg.RerankResponse, error) {
	if strings.TrimSpace(req.Query) == "" || len(req.Documents) == 0 {
		return nil, &mg.AdapterError{Message: "rerank query and documents are required", Class: mg.ErrorProvider4xx, Retryable: false}
	}
	body, err := json.Marshal(map[string]any{
		"model": req.ModelHint,
		"input": map[string]any{
			"query":     req.Query,
			"documents": req.Documents,
		},
		"parameters": map[string]any{
			"return_documents": false,
			"top_n":            len(req.Documents),
		},
	})
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+rerankPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if a.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	}
	resp, err := a.http.Do(httpReq)
	if err != nil {
		return nil, &mg.AdapterError{Message: err.Error(), Class: mg.ErrorNetwork, Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		class := mg.ErrorProvider4xx
		if resp.StatusCode == http.StatusTooManyRequests {
			class = mg.ErrorRateLimited
		} else if resp.StatusCode >= 500 {
			class = mg.ErrorProvider5xx
		}
		return nil, &mg.AdapterError{
			Message:    fmt.Sprintf("dashscope rerank http %d: %s", resp.StatusCode, string(errBody)),
			Class:      class,
			HTTPStatus: resp.StatusCode,
			Retryable:  retryable,
		}
	}
	var parsed struct {
		Output struct {
			Results []struct {
				Index          int     `json:"index"`
				RelevanceScore float64 `json:"relevance_score"`
			} `json:"results"`
		} `json:"output"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, &mg.AdapterError{Message: "invalid rerank response: " + err.Error(), Class: mg.ErrorProvider5xx, Retryable: false}
	}
	results := make([]mg.RerankResult, 0, len(parsed.Output.Results))
	for _, item := range parsed.Output.Results {
		if item.Index < 0 || item.Index >= len(req.Documents) {
			return nil, &mg.AdapterError{
				Message:   fmt.Sprintf("rerank result index %d out of range", item.Index),
				Class:     mg.ErrorProvider5xx,
				Retryable: false,
			}
		}
		results = append(results, mg.RerankResult{Index: item.Index, Score: item.RelevanceScore})
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	return &mg.RerankResponse{
		RequestID: req.RequestID,
		Provider:  a.id,
		Model:     req.ModelHint,
		Results:   results,
		Usage:     mg.ModelUsage{PromptTokens: parsed.Usage.TotalTokens, Source: mg.UsageSourceGateway},
	}, nil
}
