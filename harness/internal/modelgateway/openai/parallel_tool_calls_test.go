package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

// parallel_tool_calls_test.go 覆盖 A5：ModelOptions.ParallelToolCalls 非 nil 时
// 序列化到 openai 兼容请求体；nil 时不下发该 provider 参数。

func captureRequestBody(t *testing.T, options mg.ModelOptions) map[string]any {
	t.Helper()
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	a := New("t", srv.URL, "k")
	stream, err := a.InvokeChat(context.Background(), mg.AdapterRequest{
		Model:    "m",
		Messages: []mg.ChatMessage{{Role: "user", Content: "hi"}},
		Options:  options,
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	stream.Close()

	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("bad request body %q: %v", gotBody, err)
	}
	return body
}

func TestParallelToolCallsSerializedWhenSet(t *testing.T) {
	yes := true
	body := captureRequestBody(t, mg.ModelOptions{ParallelToolCalls: &yes})
	if got, ok := body["parallel_tool_calls"]; !ok || got != true {
		t.Fatalf("parallel_tool_calls=true must be serialized, got %v (present=%v)", got, ok)
	}

	no := false
	body = captureRequestBody(t, mg.ModelOptions{ParallelToolCalls: &no})
	if got, ok := body["parallel_tool_calls"]; !ok || got != false {
		t.Fatalf("parallel_tool_calls=false must be serialized, got %v (present=%v)", got, ok)
	}
}

func TestParallelToolCallsOmittedWhenNil(t *testing.T) {
	body := captureRequestBody(t, mg.ModelOptions{})
	if _, ok := body["parallel_tool_calls"]; ok {
		t.Fatal("nil ParallelToolCalls must not emit the provider parameter")
	}
}
