package toolgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type httpExecutorRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn httpExecutorRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type httpExecutorDeadlineBody struct {
	ctx context.Context
}

func (body *httpExecutorDeadlineBody) Read(_ []byte) (int, error) {
	<-body.ctx.Done()
	return 0, errors.New("response body interrupted")
}

func (body *httpExecutorDeadlineBody) Close() error {
	return nil
}

func newHTTPExecutorClient(roundTrip httpExecutorRoundTripFunc) *http.Client {
	return &http.Client{Transport: roundTrip}
}

func newHTTPExecutorResponse(status int, headers http.Header, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     headers.Clone(),
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func TestHTTPExecutorPostsValidatedArgumentsToFixedEndpoint(t *testing.T) {
	const endpoint = "https://tools.example.test/v1/search?source=definition"
	var gotMethod string
	var gotURL string
	var gotBody string
	client := newHTTPExecutorClient(func(req *http.Request) (*http.Response, error) {
		gotMethod = req.Method
		gotURL = req.URL.String()
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		gotBody = string(body)
		return newHTTPExecutorResponse(http.StatusOK, http.Header{
			"Content-Type": []string{"application/json; charset=utf-8"},
			"X-Response":   []string{"fixed-endpoint"},
		}, []byte(`{"count":1}`)), nil
	})

	executor := NewHTTPExecutor(client)
	raw, err := executor.Execute(context.Background(), &ToolDefinition{
		Name: "search_http",
		Type: ToolTypeHTTP,
		HTTP: &HTTPToolSpec{
			Method:       http.MethodPost,
			URL:          endpoint,
			ResponseMode: "json",
			Timeout:      time.Second,
		},
	}, ToolCallRequest{
		ToolCallID: "tc-http-1",
		Arguments:  []byte(`{"query":"hotel"}`),
	})
	if err != nil {
		t.Fatalf("execute http tool: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s", gotMethod)
	}
	if gotURL != endpoint {
		t.Fatalf("url = %s", gotURL)
	}
	if gotBody != `{"query":"hotel"}` {
		t.Fatalf("body = %s", gotBody)
	}
	if string(raw.Data) != `{"count":1}` {
		t.Fatalf("raw data = %s", raw.Data)
	}
	if raw.MimeType != "application/json" {
		t.Fatalf("mime type = %s", raw.MimeType)
	}
	if raw.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d", raw.StatusCode)
	}
	if raw.Headers["X-Response"] != "fixed-endpoint" {
		t.Fatalf("response headers = %#v", raw.Headers)
	}
}

func TestHTTPExecutorMapsValidatedGETArgumentsToQuery(t *testing.T) {
	client := newHTTPExecutorClient(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Fatalf("method = %q, want GET", req.Method)
		}
		if req.URL.String() != "https://tools.example.test/search?existing=1&page=2&tag=a&tag=b" {
			t.Fatalf("GET URL = %s", req.URL)
		}
		if (req.Body != nil && req.Body != http.NoBody) || req.Header.Get("Content-Type") != "" {
			t.Fatalf("GET request unexpectedly carried body=%T content-type=%q", req.Body, req.Header.Get("Content-Type"))
		}
		return newHTTPExecutorResponse(http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}, []byte(`{"ok":true}`)), nil
	})
	_, err := NewHTTPExecutor(client).Execute(context.Background(), &ToolDefinition{
		Name: "search", Type: ToolTypeHTTP,
		HTTP: &HTTPToolSpec{Method: "  get  ", URL: "https://tools.example.test/search?existing=1", ResponseMode: "json"},
	}, ToolCallRequest{Arguments: json.RawMessage(`{"page":2,"tag":["a","b"]}`)})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestHTTPExecutorRejectsResponseBeyondHardLimit(t *testing.T) {
	client := newHTTPExecutorClient(func(*http.Request) (*http.Response, error) {
		return newHTTPExecutorResponse(http.StatusOK, http.Header{"Content-Type": []string{"application/octet-stream"}}, []byte("12345")), nil
	})
	executor := NewHTTPExecutorWithLimit(client, 4)
	_, err := executor.Execute(context.Background(), &ToolDefinition{
		Type: ToolTypeHTTP,
		HTTP: &HTTPToolSpec{Method: http.MethodGet, URL: "https://tools.example.test/large", ResponseMode: "bytes"},
	}, ToolCallRequest{})
	if !IsErrorType(err, ErrorTypeResultTooLarge) {
		t.Fatalf("Execute() error = %v, want result_too_large", err)
	}
}

func TestHTTPExecutorAllowsTrustedConfiguredAuthorization(t *testing.T) {
	const (
		endpoint           = "https://tools.example.test/v1/private-search"
		definitionAuth     = "Bearer definition-owned-credential"
		metadataCredential = "request-metadata-credential-must-not-be-forwarded"
	)
	var gotMethod string
	var gotURL string
	var gotBody string
	var gotHeaders http.Header
	client := newHTTPExecutorClient(func(req *http.Request) (*http.Response, error) {
		gotMethod = req.Method
		gotURL = req.URL.String()
		gotHeaders = req.Header.Clone()
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		gotBody = string(body)
		return newHTTPExecutorResponse(http.StatusOK, http.Header{
			"Content-Type": []string{"application/json"},
		}, []byte(`{"ok":true}`)), nil
	})

	executor := NewHTTPExecutor(client)
	_, err := executor.Execute(context.Background(), &ToolDefinition{
		Name: "private_search_http",
		Type: ToolTypeHTTP,
		HTTP: &HTTPToolSpec{
			Method: http.MethodPut,
			URL:    endpoint,
			Write:  true,
			Headers: map[string]string{
				"Authorization": definitionAuth,
				"X-Definition":  "trusted-definition-header",
			},
			ResponseMode: "json",
		},
	}, ToolCallRequest{
		ToolCallID: "tc-http-authorization",
		Arguments:  []byte(`{"query":"museum"}`),
		Metadata: map[string]string{
			"Authorization": metadataCredential,
			"credential":    metadataCredential,
		},
	})
	if err != nil {
		t.Fatalf("execute http tool: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Fatalf("method = %s", gotMethod)
	}
	if gotURL != endpoint {
		t.Fatalf("url = %s", gotURL)
	}
	if gotBody != `{"query":"museum"}` {
		t.Fatalf("body = %s", gotBody)
	}
	if gotHeaders.Get("Content-Type") != "application/json" {
		t.Fatalf("content type = %q", gotHeaders.Get("Content-Type"))
	}
	if gotHeaders.Get("X-Definition") != "trusted-definition-header" {
		t.Fatalf("definition header = %q", gotHeaders.Get("X-Definition"))
	}
	if gotHeaders.Get("Authorization") != definitionAuth {
		t.Fatalf("authorization = %q", gotHeaders.Get("Authorization"))
	}
	for key, values := range gotHeaders {
		for _, value := range values {
			if strings.Contains(value, metadataCredential) {
				t.Fatalf("request metadata credential reached header %q", key)
			}
		}
	}
}

func TestHTTPExecutorReturnsBytesForBytesMode(t *testing.T) {
	responseBody := []byte{'b', 'i', 'n', 'a', 'r', 'y', 0, 'p', 'a', 'y', 'l', 'o', 'a', 'd'}
	wantBody := append([]byte(nil), responseBody...)
	client := newHTTPExecutorClient(func(_ *http.Request) (*http.Response, error) {
		return newHTTPExecutorResponse(http.StatusPartialContent, http.Header{
			"Content-Type": []string{"application/octet-stream; charset=binary"},
			"X-Response":   []string{"bytes"},
		}, responseBody), nil
	})

	raw, err := NewHTTPExecutor(client).Execute(context.Background(), &ToolDefinition{
		Name: "download_http",
		Type: ToolTypeHTTP,
		HTTP: &HTTPToolSpec{
			Method:       http.MethodGet,
			URL:          "https://tools.example.test/v1/download",
			ResponseMode: "bytes",
		},
	}, ToolCallRequest{ToolCallID: "tc-http-bytes"})
	if err != nil {
		t.Fatalf("execute http tool: %v", err)
	}
	responseBody[0] = 'X'
	if !bytes.Equal(raw.Bytes, wantBody) {
		t.Fatalf("bytes = %v, want %v", raw.Bytes, wantBody)
	}
	if len(raw.Data) != 0 {
		t.Fatalf("data must be empty, got %q", raw.Data)
	}
	if raw.Text != "" {
		t.Fatalf("text must be empty, got %q", raw.Text)
	}
	if raw.StatusCode != http.StatusPartialContent {
		t.Fatalf("status code = %d", raw.StatusCode)
	}
	if raw.MimeType != "application/octet-stream" {
		t.Fatalf("mime type = %q", raw.MimeType)
	}
	if raw.Headers["X-Response"] != "bytes" {
		t.Fatalf("response headers = %#v", raw.Headers)
	}
}

func TestHTTPExecutorMapsPermissionRateAndServerErrors(t *testing.T) {
	const rawResponseSentinel = "raw-response-must-not-reach-error"
	tests := []struct {
		name      string
		status    int
		body      string
		mode      string
		errorType ErrorType
		retryable bool
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: rawResponseSentinel, mode: "json", errorType: ErrorTypePermissionDenied},
		{name: "forbidden", status: http.StatusForbidden, body: rawResponseSentinel, mode: "json", errorType: ErrorTypePermissionDenied},
		{name: "rate limited", status: http.StatusTooManyRequests, body: rawResponseSentinel, mode: "json", errorType: ErrorTypeRateLimited, retryable: true},
		{name: "server error", status: http.StatusBadGateway, body: rawResponseSentinel, mode: "json", errorType: ErrorTypeUpstreamError, retryable: true},
		{name: "other client rejection", status: http.StatusTeapot, body: rawResponseSentinel, mode: "json", errorType: ErrorTypeUpstreamError},
		{name: "invalid json", status: http.StatusOK, body: "not-json-" + rawResponseSentinel, mode: "json", errorType: ErrorTypeUpstreamError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newHTTPExecutorClient(func(_ *http.Request) (*http.Response, error) {
				return newHTTPExecutorResponse(tc.status, http.Header{
					"Content-Type": []string{"application/json"},
				}, []byte(tc.body)), nil
			})

			_, err := NewHTTPExecutor(client).Execute(context.Background(), &ToolDefinition{
				Name: "search_http",
				Type: ToolTypeHTTP,
				HTTP: &HTTPToolSpec{
					Method:       http.MethodPost,
					URL:          "https://tools.example.test/v1/search",
					ResponseMode: tc.mode,
				},
			}, ToolCallRequest{ToolCallID: "tc-http-error", Arguments: []byte(`{"query":"hotel"}`)})
			if !IsErrorType(err, tc.errorType) {
				t.Fatalf("expected %s, got %v", tc.errorType, err)
			}
			var toolErr *ToolError
			if !AsToolError(err, &toolErr) {
				t.Fatalf("expected ToolError, got %T", err)
			}
			if toolErr.Retryable != tc.retryable {
				t.Fatalf("retryable = %v, want %v", toolErr.Retryable, tc.retryable)
			}
			if strings.Contains(err.Error(), rawResponseSentinel) {
				t.Fatalf("error exposed raw response: %v", err)
			}
		})
	}
}

func TestHTTPExecutorClassifiesOwnDeadline(t *testing.T) {
	const timeout = 20 * time.Millisecond
	tests := []struct {
		name      string
		roundTrip httpExecutorRoundTripFunc
	}{
		{
			name: "RoundTrip",
			roundTrip: func(req *http.Request) (*http.Response, error) {
				<-req.Context().Done()
				return nil, errors.New("round trip interrupted")
			},
		},
		{
			name: "body Read",
			roundTrip: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type": []string{"application/json"},
					},
					Body: &httpExecutorDeadlineBody{ctx: req.Context()},
				}, nil
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parentCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := NewHTTPExecutor(newHTTPExecutorClient(tc.roundTrip)).Execute(parentCtx, &ToolDefinition{
				Name: "deadline_http",
				Type: ToolTypeHTTP,
				HTTP: &HTTPToolSpec{
					Method:       http.MethodPost,
					URL:          "https://tools.example.test/v1/deadline",
					ResponseMode: "json",
					Timeout:      timeout,
				},
			}, ToolCallRequest{ToolCallID: "tc-http-deadline", Arguments: []byte(`{"query":"hotel"}`)})
			if parentCtx.Err() != nil {
				t.Fatalf("parent context must remain live: %v", parentCtx.Err())
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error must remain deadline-related, got %v", err)
			}
			errorType, retryable := classifyExecutionError(err)
			if errorType != ErrorTypeTimeout {
				t.Fatalf("error type = %s, want %s (err=%v)", errorType, ErrorTypeTimeout, err)
			}
			if !retryable {
				t.Fatal("executor-owned deadline must remain retryable")
			}
		})
	}
}

func TestHTTPExecutorConfigResolverOverlaysEndpointAndHeaders(t *testing.T) {
	var gotURL, gotAuth string
	client := newHTTPExecutorClient(func(req *http.Request) (*http.Response, error) {
		gotURL = req.URL.String()
		gotAuth = req.Header.Get("Authorization")
		return newHTTPExecutorResponse(http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}, []byte(`{"ok":true}`)), nil
	})
	base := &ToolDefinition{
		Name: "svc_http", Type: ToolTypeHTTP,
		ConfigSchema: json.RawMessage(`{"type":"object"}`),
		HTTP:         &HTTPToolSpec{Method: http.MethodPost, URL: "https://base.example.test/v1", ResponseMode: "json", Timeout: time.Second},
	}
	resolver := func(_ context.Context, def *ToolDefinition) (*HTTPToolSpec, error) {
		spec := *def.HTTP
		spec.URL = "https://tenant.example.test/v2"
		spec.Headers = map[string]string{"Authorization": "Bearer tenant-token"}
		return &spec, nil
	}
	executor := NewHTTPExecutor(client).WithConfigResolver(resolver)
	if _, err := executor.Execute(context.Background(), base, ToolCallRequest{ToolCallID: "tc-overlay", Arguments: []byte(`{"q":"x"}`)}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotURL != "https://tenant.example.test/v2" {
		t.Fatalf("request URL = %s, want tenant endpoint", gotURL)
	}
	if gotAuth != "Bearer tenant-token" {
		t.Fatalf("Authorization = %q, want injected tenant token", gotAuth)
	}
}

func TestHTTPExecutorConfigResolverNilFallsBackToBaseSpec(t *testing.T) {
	var gotURL string
	client := newHTTPExecutorClient(func(req *http.Request) (*http.Response, error) {
		gotURL = req.URL.String()
		return newHTTPExecutorResponse(http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}, []byte(`{}`)), nil
	})
	base := &ToolDefinition{
		Name: "svc_http", Type: ToolTypeHTTP,
		HTTP: &HTTPToolSpec{Method: http.MethodPost, URL: "https://base.example.test/v1", ResponseMode: "json", Timeout: time.Second},
	}
	resolver := func(_ context.Context, _ *ToolDefinition) (*HTTPToolSpec, error) { return nil, nil }
	if _, err := NewHTTPExecutor(client).WithConfigResolver(resolver).Execute(context.Background(), base, ToolCallRequest{ToolCallID: "tc-base", Arguments: []byte(`{}`)}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotURL != "https://base.example.test/v1" {
		t.Fatalf("request URL = %s, want base endpoint", gotURL)
	}
}

func TestHTTPExecutorConfigResolverErrorAbortsCall(t *testing.T) {
	called := false
	client := newHTTPExecutorClient(func(req *http.Request) (*http.Response, error) {
		called = true
		return newHTTPExecutorResponse(http.StatusOK, http.Header{}, []byte(`{}`)), nil
	})
	base := &ToolDefinition{Name: "svc_http", Type: ToolTypeHTTP, HTTP: &HTTPToolSpec{Method: http.MethodPost, URL: "https://base.example.test/v1", ResponseMode: "json", Timeout: time.Second}}
	resolver := func(_ context.Context, _ *ToolDefinition) (*HTTPToolSpec, error) {
		return nil, NewToolError(ErrorTypeInternal, "tenant credential env not set", false, nil)
	}
	_, err := NewHTTPExecutor(client).WithConfigResolver(resolver).Execute(context.Background(), base, ToolCallRequest{ToolCallID: "tc-err", Arguments: []byte(`{}`)})
	if err == nil {
		t.Fatal("expected resolver error to abort the call")
	}
	if called {
		t.Fatal("HTTP request must not be sent when the resolver fails")
	}
}
