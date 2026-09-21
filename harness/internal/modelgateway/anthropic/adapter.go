// Package anthropic implements the Anthropic Messages API boundary for the
// Harness model gateway.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	mg "github.com/AGenUI/agenui-studio/harness/internal/modelgateway"
)

const anthropicVersion = "2023-06-01"

type Adapter struct {
	id                         string
	baseURL                    string
	apiKey                     string
	client                     *http.Client
	promptCacheSessionAffinity bool
}

var _ mg.ChatProvider = (*Adapter)(nil)

// Option configures provider-specific Anthropic transport behavior.
type Option func(*Adapter)

// WithPromptCacheSessionAffinity enables the company-gateway X-Session-Id
// routing contract. It is deliberately opt-in: public Anthropic and arbitrary
// Anthropic-compatible endpoints must not receive a stable Harness identity.
func WithPromptCacheSessionAffinity() Option {
	return func(adapter *Adapter) {
		adapter.promptCacheSessionAffinity = true
	}
}

func New(id, baseURL, apiKey string, options ...Option) *Adapter {
	if id == "" {
		id = "anthropic_provider"
	}
	adapter := &Adapter{
		id: id, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey,
		// Streaming lifetime is controlled by the request context.
		client: &http.Client{},
	}
	for _, option := range options {
		if option != nil {
			option(adapter)
		}
	}
	// Defense in depth: the Option is only an opt-in signal. The adapter still
	// enforces the controlled endpoint boundary so an internal caller cannot
	// accidentally send the company routing header to public Anthropic or an
	// arbitrary compatible endpoint by bypassing composition validation.
	adapter.promptCacheSessionAffinity = adapter.promptCacheSessionAffinity &&
		mg.PromptCacheSessionAffinityAllowed(mg.ProviderKindSessionAffinity, "anthropic", adapter.baseURL)
	return adapter
}

func (a *Adapter) ID() string { return a.id }

func (a *Adapter) InvokeChat(ctx context.Context, req mg.AdapterRequest) (mg.AdapterStream, error) {
	body, err := buildRequest(req)
	if err != nil {
		return nil, &mg.AdapterError{Message: err.Error(), Class: mg.ErrorSchema, Retryable: false}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, messagesEndpoint(a.baseURL), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	if a.apiKey != "" {
		httpReq.Header.Set("x-api-key", a.apiKey)
	}
	if a.promptCacheSessionAffinity && req.PromptCacheAffinityKey != "" {
		httpReq.Header.Set("X-Session-Id", req.PromptCacheAffinityKey)
	}
	if req.Streaming {
		httpReq.Header.Set("accept", "text/event-stream")
	} else {
		httpReq.Header.Set("accept", "application/json")
	}

	client := a.client
	if a.promptCacheSessionAffinity {
		// X-Session-Id is company-gateway-only. net/http otherwise forwards
		// unknown headers across redirects, so disable redirects for affinity
		// requests and classify the 3xx response instead of leaking the key to a
		// different endpoint.
		clientCopy := *client
		clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		client = &clientCopy
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return nil, &mg.AdapterError{Message: err.Error(), Class: mg.ErrorStreamInterrupted, Retryable: true}
		case errors.Is(err, context.DeadlineExceeded):
			return nil, &mg.AdapterError{Message: err.Error(), Class: mg.ErrorTimeout, Retryable: true}
		}
		return nil, &mg.AdapterError{Message: err.Error(), Class: mg.ErrorNetwork, Retryable: true}
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		rawError, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, classifyHTTPError(resp.StatusCode, rawError)
	}
	if req.Streaming {
		return newStream(resp.Body), nil
	}
	stream, err := newNonStream(resp.Body)
	if err != nil {
		_ = resp.Body.Close()
		var adapterErr *mg.AdapterError
		if errors.As(err, &adapterErr) {
			return nil, adapterErr
		}
		return nil, &mg.AdapterError{Message: fmt.Sprintf("decode anthropic response: %v", err), Class: mg.ErrorProvider5xx, Retryable: false}
	}
	return stream, nil
}

func messagesEndpoint(baseURL string) string {
	trimmed := strings.TrimRight(baseURL, "/")
	switch {
	case strings.HasSuffix(trimmed, "/v1/messages"):
		return trimmed
	case strings.HasSuffix(trimmed, "/v1"):
		return trimmed + "/messages"
	default:
		return trimmed + "/v1/messages"
	}
}
