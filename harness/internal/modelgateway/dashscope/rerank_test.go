package dashscope

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// 验证原生 text-rerank 请求形状与结果按相关性降序归一。
func TestRerankAdapter(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output": map[string]any{
				"results": []map[string]any{
					{"index": 0, "relevance_score": 0.2},
					{"index": 2, "relevance_score": 0.9},
					{"index": 1, "relevance_score": 0.5},
				},
			},
			"usage":      map[string]int{"total_tokens": 11},
			"request_id": "ds-1",
		})
	}))
	defer srv.Close()

	adapter := NewRerank("gte", srv.URL, "sk-test")
	resp, err := adapter.Rerank(context.Background(), mg.RerankRequest{
		RequestID: "r1", ModelHint: "gte-rerank-v2", Query: "查询", Documents: []string{"d0", "d1", "d2"},
	})
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if gotPath != rerankPath || gotAuth != "Bearer sk-test" {
		t.Fatalf("request shape: path=%s auth=%s", gotPath, gotAuth)
	}
	if gotBody["model"] != "gte-rerank-v2" {
		t.Fatalf("model not forwarded: %#v", gotBody)
	}
	if len(resp.Results) != 3 || resp.Results[0].Index != 2 || resp.Results[1].Index != 1 || resp.Results[2].Index != 0 {
		t.Fatalf("results must be sorted by score desc: %#v", resp.Results)
	}
	if resp.Usage.PromptTokens != 11 || resp.Provider != "gte" {
		t.Fatalf("response meta: %+v", resp)
	}
}

// 越界 index 与非 200 响应均 fail closed。
func TestRerankAdapterFailClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output": map[string]any{"results": []map[string]any{{"index": 9, "relevance_score": 0.9}}},
		})
	}))
	defer srv.Close()
	adapter := NewRerank("gte", srv.URL, "k")
	if _, err := adapter.Rerank(context.Background(), mg.RerankRequest{ModelHint: "m", Query: "q", Documents: []string{"a"}}); err == nil {
		t.Fatal("out-of-range index must fail closed")
	}

	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv500.Close()
	adapter = NewRerank("gte", srv500.URL, "k")
	_, err := adapter.Rerank(context.Background(), mg.RerankRequest{ModelHint: "m", Query: "q", Documents: []string{"a"}})
	var aerr *mg.AdapterError
	if !errors.As(err, &aerr) || !aerr.Retryable || aerr.Class != mg.ErrorProvider5xx {
		t.Fatalf("want retryable 5xx adapter error, got %v", err)
	}
}

// rerank provider 不承接 chat 调用。
func TestRerankAdapterRejectsChat(t *testing.T) {
	adapter := NewRerank("gte", "http://unused", "k")
	if _, err := adapter.InvokeChat(context.Background(), mg.AdapterRequest{}); err == nil {
		t.Fatal("chat via rerank provider must fail closed")
	}
}
