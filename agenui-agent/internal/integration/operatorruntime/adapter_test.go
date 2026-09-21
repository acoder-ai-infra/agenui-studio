package operatorruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	bindingoperator "github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/binding/operator"
	platformoperator "github.com/AGenUI/agenui-studio/agenui-agent/internal/operator"
)

func TestAdapterExecutesPublishedJavaScriptByIDAndParameters(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Fatalf("method = %s", request.Method)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"operatorVersionId":205}` {
			t.Fatalf("request body = %s", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"code":1,"message":"success","result":true,"data":{
				"operatorVersionId":205,"version":1,"sourceHash":"sha256:test",
				"language":"javascript","languageVersion":"ES2022",
				"sourceCode":"function transform(value, params) { return {label: String(value) + ' ' + params.unit}; }"
			}
		}`))
	}))
	defer server.Close()

	client, err := platformoperator.NewHTTPDetailClient(platformoperator.HTTPDetailClientConfig{
		Endpoint: server.URL,
		Client:   server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := platformoperator.NewService(client, platformoperator.DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(service)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := bindingoperator.NewRuntimePortExecutionService(adapter)
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := bindingoperator.NewBindingExecutionContext(bindingoperator.BindingExecutionContextConfig{
		TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a", RunID: "run-a",
		AgentID: "agenui_binder", ToolCallID: "tool-a", IdempotencyKey: "idem-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := execution.ExecuteOperator(context.Background(), trusted, bindingoperator.OperatorExecutionCommand{
		OperatorID:  205,
		SampleValue: json.RawMessage(`1200`), Params: json.RawMessage(`{"unit":"m"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output) != `{"label":"1200 m"}` {
		t.Fatalf("output = %s", result.Output)
	}
}

func TestAdapterMapsPlatformFailuresWithoutReturningFallbackValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code string
		want error
	}{
		{name: "not found", code: platformoperator.CodeOperatorNotFound, want: bindingoperator.ErrUnknownOperator},
		{name: "execution failed", code: platformoperator.CodeJSExecuteFailed, want: bindingoperator.ErrExecutionFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, err := New(executorFunc(func(context.Context, platformoperator.ExecuteRequest) platformoperator.ExecuteResult {
				return platformoperator.ExecuteResult{
					OperatorID: 7, Value: map[string]any{"untrusted": true}, Applied: false,
					Error: &platformoperator.ErrorDetail{Code: test.code, Message: "safe", Cause: "secret endpoint"},
				}
			}))
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.RunOperator(context.Background(), validRuntimeRequest(7))
			if !errors.Is(err, test.want) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestAdapterHonorsAuthoritativeCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	adapter, err := New(executorFunc(func(context.Context, platformoperator.ExecuteRequest) platformoperator.ExecuteResult {
		cancel()
		return platformoperator.ExecuteResult{OperatorID: 8, Applied: true, Value: "late"}
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.RunOperator(ctx, validRuntimeRequest(8))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestAdapterBoundsConcurrentJavaScriptExecutions(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	adapter, err := newWithConcurrency(executorFunc(func(
		_ context.Context,
		request platformoperator.ExecuteRequest,
	) platformoperator.ExecuteResult {
		calls.Add(1)
		return platformoperator.ExecuteResult{OperatorID: request.OperatorID, Applied: true, Value: "ok"}
	}), 1)
	if err != nil {
		t.Fatal(err)
	}
	// Occupy the sole process slot without invoking the executor. The request
	// must wait on the semaphore and honor cancellation before running JS.
	adapter.slots <- struct{}{}
	blocked, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, runErr := adapter.RunOperator(blocked, validRuntimeRequest(2))
		result <- runErr
	}()
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked error = %v", err)
	}
	<-adapter.slots
	if calls.Load() != 0 {
		t.Fatalf("executor calls = %d", calls.Load())
	}
}

type executorFunc func(context.Context, platformoperator.ExecuteRequest) platformoperator.ExecuteResult

func (function executorFunc) Execute(
	ctx context.Context,
	request platformoperator.ExecuteRequest,
) platformoperator.ExecuteResult {
	return function(ctx, request)
}

func validRuntimeRequest(operatorID int64) bindingoperator.OperatorRuntimeRequest {
	return bindingoperator.OperatorRuntimeRequest{
		Scope: bindingoperator.OperatorRuntimeScope{
			TenantID: "tenant", UserID: "user", SessionID: "session", RunID: "run",
			AgentID: "agenui_binder", ToolCallID: "tool",
		},
		InvocationID: "execution", IdempotencyKey: "idempotency",
		RequestFingerprint: "sha256:" + strings.Repeat("a", 64),
		OperatorID:         operatorID, SampleValue: json.RawMessage(`1`), Params: json.RawMessage(`{}`), MaxOutputBytes: 1 << 20,
	}
}
