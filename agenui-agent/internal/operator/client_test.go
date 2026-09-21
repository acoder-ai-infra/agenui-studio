package operator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestHTTPDetailClientGetOperator(t *testing.T) {
	client, err := NewHTTPDetailClient(HTTPDetailClientConfig{
		Endpoint: "http://operators.example.test/api/v1/agenui/platform/operator/published/detail",
		Client: &http.Client{Transport: roundTripFunc(func(
			request *http.Request,
		) (*http.Response, error) {
			if request.Method != http.MethodPost {
				t.Fatalf("unexpected method %s", request.Method)
			}
			if request.URL.Host != "operators.example.test" {
				t.Fatalf("unexpected host %s", request.URL.Host)
			}
			if request.URL.Path != "/api/v1/agenui/platform/operator/published/detail" {
				t.Fatalf("unexpected path %s", request.URL.Path)
			}
			var payload map[string]uint64
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["operatorVersionId"] != 205 {
				t.Fatalf("unexpected payload %+v", payload)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{
					"code": 1,
					"message": "success",
					"result": true,
					"timestamp": 1786600000,
					"traceId": "trace-1",
					"data": {
						"operatorVersionId": 205,
						"operatorKey": "agenui.scalar.identity",
						"version": 2,
						"inputSchema": true,
						"paramsSchema": {"type":"object"},
						"outputSchema": true,
						"sourceHash": "sha256",
						"language": "javascript",
						"languageVersion": "ES2022",
						"sourceCode": "function transform(value) { return value; }"
					}
				}`)),
			}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := client.GetOperator(context.Background(), 205)
	if err != nil {
		t.Fatal(err)
	}
	if detail.ID != "205" ||
		detail.OperatorVersionID != 205 ||
		detail.OperatorKey != "agenui.scalar.identity" ||
		detail.Version != "2" ||
		detail.Language != "javascript" ||
		detail.LanguageVersion != "ES2022" ||
		detail.SourceHash != "sha256" ||
		!strings.Contains(detail.Code, "transform") ||
		len(detail.InputSchema) == 0 || len(detail.ParamsSchema) == 0 || len(detail.OutputSchema) == 0 {
		t.Fatalf("unexpected detail %+v", detail)
	}
}

func TestHTTPDetailClientGetOperatorNotFound(t *testing.T) {
	client, err := NewHTTPDetailClient(HTTPDetailClientConfig{
		Endpoint: "http://operator.test/api/v1/agenui/platform/operator/published/detail",
		Client: &http.Client{Transport: roundTripFunc(func(
			request *http.Request,
		) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(strings.NewReader(`{
					"code": 40415,
					"message": "not found",
					"result": false,
					"traceId": "trace-404"
				}`)),
			}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetOperator(context.Background(), 404)
	if !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("expected ErrOperatorNotFound, got %v", err)
	}
}

func TestHTTPDetailClientRejectsMismatchedOperatorID(t *testing.T) {
	client, err := NewHTTPDetailClient(HTTPDetailClientConfig{
		Endpoint: "http://operator.test/api/v1/agenui/platform/operator/published/detail",
		Client: &http.Client{Transport: roundTripFunc(func(
			request *http.Request,
		) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(strings.NewReader(`{
					"code": 1,
					"message": "success",
					"result": true,
					"data": {
						"operatorVersionId": 206,
						"version": 2,
						"sourceHash": "sha256:test",
						"language": "javascript",
						"languageVersion": "ES2022",
						"sourceCode": "function transform(value) { return value; }"
					}
				}`)),
			}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetOperator(context.Background(), 205)
	if err == nil || !strings.Contains(err.Error(), "id mismatch") {
		t.Fatalf("expected id mismatch, got %v", err)
	}
}

func TestHTTPDetailClientGetOperatorBusinessError(t *testing.T) {
	client, err := NewHTTPDetailClient(HTTPDetailClientConfig{
		Endpoint: "http://operator.test/api/v1/agenui/platform/operator/published/detail",
		Client: &http.Client{Transport: roundTripFunc(func(
			request *http.Request,
		) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(strings.NewReader(`{
					"code": 50001,
					"message": "platform error",
					"result": false,
					"traceId": "trace-500"
				}`)),
			}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetOperator(context.Background(), 500)
	if err == nil ||
		!strings.Contains(err.Error(), "50001") ||
		!strings.Contains(err.Error(), "trace-500") {
		t.Fatalf("expected business error, got %v", err)
	}
}

func TestHTTPDetailClientGetOperatorRejectsZeroID(t *testing.T) {
	client, err := NewHTTPDetailClient(HTTPDetailClientConfig{
		Endpoint: "http://operator.test/api/v1/agenui/platform/operator/published/detail",
		Client: &http.Client{Transport: roundTripFunc(func(
			request *http.Request,
		) (*http.Response, error) {
			t.Fatal("request should not be sent for invalid id")
			return nil, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetOperator(context.Background(), 0)
	if err == nil || !strings.Contains(err.Error(), "uint64") {
		t.Fatalf("expected invalid id error, got %v", err)
	}
}
