package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// 验证 /embeddings 请求形状、鉴权头，以及乱序 data 按 index 归位。
func TestAdapterEmbed(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "text-embedding-v3",
			"data": []map[string]any{
				{"index": 1, "embedding": []float64{0.3, 0.4}},
				{"index": 0, "embedding": []float64{0.1, 0.2}},
			},
			"usage": map[string]int{"prompt_tokens": 7, "total_tokens": 7},
		})
	}))
	defer srv.Close()

	adapter := New("dashscope", srv.URL, "sk-test")
	resp, err := adapter.Embed(context.Background(), mg.EmbeddingRequest{
		RequestID: "e1", ModelHint: "text-embedding-v3", Texts: []string{"你好", "world"},
	})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if gotPath != "/embeddings" || gotAuth != "Bearer sk-test" {
		t.Fatalf("request shape: path=%s auth=%s", gotPath, gotAuth)
	}
	if gotBody["model"] != "text-embedding-v3" {
		t.Fatalf("model not forwarded: %#v", gotBody)
	}
	if len(resp.Embeddings) != 2 || resp.Embeddings[0][0] != 0.1 || resp.Embeddings[1][0] != 0.3 {
		t.Fatalf("embeddings must be re-ordered by index: %#v", resp.Embeddings)
	}
	if resp.Provider != "dashscope" || resp.Usage.PromptTokens != 7 {
		t.Fatalf("response meta: %+v", resp)
	}
}

// 4xx 不可重试、429 可重试；不服务的模型 fail closed 到调用方。
func TestAdapterEmbedHTTPError(t *testing.T) {
	status := http.StatusBadRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"unknown model"}`))
	}))
	defer srv.Close()

	adapter := New("p", srv.URL, "k")
	_, err := adapter.Embed(context.Background(), mg.EmbeddingRequest{ModelHint: "nope", Texts: []string{"a"}})
	var aerr *mg.AdapterError
	if !errors.As(err, &aerr) || aerr.Retryable || aerr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("want non-retryable 4xx adapter error, got %v", err)
	}

	status = http.StatusTooManyRequests
	_, err = adapter.Embed(context.Background(), mg.EmbeddingRequest{ModelHint: "m", Texts: []string{"a"}})
	if !errors.As(err, &aerr) || !aerr.Retryable || aerr.Class != mg.ErrorRateLimited {
		t.Fatalf("want retryable rate-limited error, got %v", err)
	}
}

// 响应条数与输入不一致必须报错，不允许静默截断。
func TestAdapterEmbedSizeMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":  []map[string]any{{"index": 0, "embedding": []float64{0.1}}},
			"usage": map[string]int{"total_tokens": 3},
		})
	}))
	defer srv.Close()

	adapter := New("p", srv.URL, "k")
	if _, err := adapter.Embed(context.Background(), mg.EmbeddingRequest{ModelHint: "m", Texts: []string{"a", "b"}}); err == nil {
		t.Fatal("size mismatch must fail closed")
	}
}
