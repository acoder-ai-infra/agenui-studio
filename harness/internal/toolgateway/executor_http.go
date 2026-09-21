package toolgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// HTTPConfigResolver, when set on an HTTPExecutor, resolves the effective
// HTTPToolSpec for a call by overlaying tenant-scoped configuration (endpoint,
// credential headers resolved from environment variables) onto the tool's base
// catalog spec. Returning (nil, nil) means "no tenant override; use the base
// spec". A non-nil error aborts the call. The resolver reads the tenant from
// ctx; the toolgateway itself stays decoupled from the config store.
type HTTPConfigResolver func(ctx context.Context, def *ToolDefinition) (*HTTPToolSpec, error)

type HTTPExecutor struct {
	client           *http.Client
	maxResponseBytes int64
	configResolver   HTTPConfigResolver
}

const defaultHTTPToolMaxResponseBytes int64 = 8 << 20

func NewHTTPExecutor(client *http.Client) *HTTPExecutor {
	return NewHTTPExecutorWithLimit(client, defaultHTTPToolMaxResponseBytes)
}

func NewHTTPExecutorWithLimit(client *http.Client, maxResponseBytes int64) *HTTPExecutor {
	if client == nil {
		client = http.DefaultClient
	}
	if maxResponseBytes <= 0 {
		maxResponseBytes = defaultHTTPToolMaxResponseBytes
	}
	return &HTTPExecutor{client: client, maxResponseBytes: maxResponseBytes}
}

// WithConfigResolver attaches a tenant tool-config overlay resolver and returns
// the executor for chaining. A nil resolver leaves the executor using base
// catalog specs only.
func (e *HTTPExecutor) WithConfigResolver(resolver HTTPConfigResolver) *HTTPExecutor {
	e.configResolver = resolver
	return e
}

func (e *HTTPExecutor) Type() ToolType {
	return ToolTypeHTTP
}

func (e *HTTPExecutor) Execute(ctx context.Context, def *ToolDefinition, req ToolCallRequest) (*ToolRawResult, error) {
	if def.HTTP == nil {
		return nil, NewToolError(ErrorTypeInternal, "http tool spec is required", false, nil)
	}
	spec := def.HTTP
	if e.configResolver != nil {
		resolved, err := e.configResolver(ctx, def)
		if err != nil {
			return nil, err
		}
		if resolved != nil {
			spec = resolved
		}
	}
	if spec.URL == "" {
		return nil, NewToolError(ErrorTypeInternal, "http tool spec is required", false, nil)
	}
	method := strings.ToUpper(strings.TrimSpace(spec.Method))
	if method == "" {
		method = http.MethodPost
	}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = def.Timeout
	}
	execCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	endpoint := spec.URL
	var requestBody io.Reader
	var err error
	if method == http.MethodGet {
		endpoint, err = httpURLWithArguments(endpoint, req.Arguments)
		if err != nil {
			return nil, NewToolError(ErrorTypeSchemaValidationFailed, "map validated http tool query arguments", false, err)
		}
	} else {
		requestBody = bytes.NewReader(req.Arguments)
	}
	httpReq, err := http.NewRequestWithContext(execCtx, method, endpoint, requestBody)
	if err != nil {
		return nil, NewToolError(ErrorTypeInternal, "build http tool request", false, err)
	}
	if len(req.Arguments) > 0 && method != http.MethodGet {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	for key, value := range spec.Headers {
		httpReq.Header.Set(key, value)
	}

	resp, err := e.client.Do(httpReq)
	if err != nil {
		if execCtx.Err() != nil {
			return nil, execCtx.Err()
		}
		return nil, NewToolError(ErrorTypeUpstreamError, "http tool request failed", true, err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, e.maxResponseBytes+1))
	if err != nil {
		if execCtx.Err() != nil {
			return nil, execCtx.Err()
		}
		return nil, NewToolError(ErrorTypeUpstreamError, "read http tool response", true, err)
	}
	if int64(len(responseBody)) > e.maxResponseBytes {
		return nil, NewToolError(ErrorTypeResultTooLarge, "http tool response exceeds configured limit", false, nil)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, NewToolError(ErrorTypePermissionDenied, "http tool permission denied", false, nil)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, NewToolError(ErrorTypeRateLimited, "http tool rate limited", true, nil)
	}
	if resp.StatusCode >= 500 {
		return nil, NewToolError(ErrorTypeUpstreamError, "http tool upstream error", true, nil)
	}
	if resp.StatusCode >= 400 {
		return nil, NewToolError(ErrorTypeUpstreamError, "http tool request rejected", false, nil)
	}
	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "application/json"
	}
	if strings.Contains(mimeType, ";") {
		mimeType = strings.TrimSpace(strings.Split(mimeType, ";")[0])
	}
	if spec.ResponseMode == "bytes" {
		return &ToolRawResult{StatusCode: resp.StatusCode, MimeType: mimeType, Bytes: append([]byte(nil), responseBody...), Headers: cloneHTTPHeaders(resp.Header)}, nil
	}
	if spec.ResponseMode == "text" {
		return &ToolRawResult{StatusCode: resp.StatusCode, MimeType: mimeType, Text: string(responseBody), Headers: cloneHTTPHeaders(resp.Header)}, nil
	}
	if !json.Valid(responseBody) && spec.ResponseMode == "json" {
		return nil, NewToolError(ErrorTypeUpstreamError, "http tool returned invalid json", false, nil)
	}
	return &ToolRawResult{
		StatusCode: resp.StatusCode,
		MimeType:   mimeType,
		Data:       append(json.RawMessage(nil), responseBody...),
		Headers:    cloneHTTPHeaders(resp.Header),
	}, nil
}

func httpURLWithArguments(endpoint string, arguments json.RawMessage) (string, error) {
	if len(arguments) == 0 || string(arguments) == "{}" || string(arguments) == "null" {
		return endpoint, nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.UseNumber()
	var values map[string]any
	if err := decoder.Decode(&values); err != nil {
		return "", err
	}
	query := parsed.Query()
	for key, value := range values {
		if err := appendHTTPQueryValue(query, key, value); err != nil {
			return "", err
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func appendHTTPQueryValue(query url.Values, key string, value any) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("query argument name is empty")
	}
	switch value := value.(type) {
	case nil:
		return nil
	case string, bool, json.Number:
		query.Add(key, fmt.Sprint(value))
		return nil
	case []any:
		for _, item := range value {
			if err := appendHTTPQueryValue(query, key, item); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("query argument %q must be a scalar or scalar array", key)
	}
}

func cloneHTTPHeaders(headers http.Header) map[string]string {
	out := make(map[string]string, len(headers))
	for key, values := range headers {
		if len(values) > 0 {
			out[key] = values[0]
		}
	}
	return out
}
