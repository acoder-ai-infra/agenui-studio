package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const defaultHTTPTimeout = 500 * time.Millisecond

type HTTPDetailClientConfig struct {
	Endpoint string
	Client   *http.Client
	Timeout  time.Duration
}

type HTTPDetailClient struct {
	endpoint string
	client   *http.Client
}

func NewHTTPDetailClient(config HTTPDetailClientConfig) (*HTTPDetailClient, error) {
	endpoint := strings.TrimSpace(config.Endpoint)
	if endpoint == "" {
		return nil, errors.New("operator detail client: endpoint is required")
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	client := config.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &HTTPDetailClient{
		endpoint: endpoint,
		client:   client,
	}, nil
}

func (c *HTTPDetailClient) GetOperator(
	ctx context.Context,
	operatorID uint64,
) (OperatorDetail, error) {
	if c == nil || c.client == nil || strings.TrimSpace(c.endpoint) == "" {
		return OperatorDetail{}, errors.New("operator detail client unavailable")
	}
	if operatorID == 0 {
		return OperatorDetail{}, errors.New("operator version id must be uint64 greater than 0")
	}
	body, err := json.Marshal(map[string]uint64{"operatorVersionId": operatorID})
	if err != nil {
		return OperatorDetail{}, err
	}
	statusCode, raw, err := c.postJSON(ctx, body)
	if err != nil {
		return OperatorDetail{}, err
	}
	if statusCode < 200 || statusCode >= 300 {
		return OperatorDetail{}, fmt.Errorf(
			"operator detail http status %d: %s",
			statusCode,
			strings.TrimSpace(string(raw)),
		)
	}
	detail, err := decodePublishedOperatorDetail(raw)
	if err != nil {
		return OperatorDetail{}, err
	}
	if detail.OperatorVersionID != operatorID {
		return OperatorDetail{}, fmt.Errorf(
			"operator detail response id mismatch: requested=%d returned=%d",
			operatorID,
			detail.OperatorVersionID,
		)
	}
	if detail.ID == "" {
		detail.ID = strconv.FormatUint(operatorID, 10)
	}
	return detail, nil
}

func (c *HTTPDetailClient) postJSON(ctx context.Context, body []byte) (int, []byte, error) {
	headers := map[string][]string{
		"Accept":       {"application/json"},
		"Content-Type": {"application/json"},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := c.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return response.StatusCode, raw, nil
}

type publishedOperatorResponse struct {
	Code      int                   `json:"code"`
	Message   string                `json:"message"`
	Data      publishedOperatorData `json:"data"`
	Result    bool                  `json:"result"`
	Timestamp int64                 `json:"timestamp"`
	TraceID   string                `json:"traceId"`
}

type publishedOperatorData struct {
	OperatorVersionID uint64          `json:"operatorVersionId"`
	OperatorKey       string          `json:"operatorKey"`
	Version           int             `json:"version"`
	InputSchema       json.RawMessage `json:"inputSchema"`
	ParamsSchema      json.RawMessage `json:"paramsSchema"`
	OutputSchema      json.RawMessage `json:"outputSchema"`
	SourceHash        string          `json:"sourceHash"`
	Language          string          `json:"language"`
	LanguageVersion   string          `json:"languageVersion"`
	SourceCode        string          `json:"sourceCode"`
	Entry             string          `json:"entry"`
}

func decodePublishedOperatorDetail(body []byte) (OperatorDetail, error) {
	var envelope publishedOperatorResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return OperatorDetail{}, err
	}
	if !envelope.Result || envelope.Code != 1 {
		if envelope.Code == 40415 {
			return OperatorDetail{}, ErrOperatorNotFound
		}
		return OperatorDetail{}, fmt.Errorf(
			"operator detail business failed code=%d message=%q traceId=%q",
			envelope.Code,
			envelope.Message,
			envelope.TraceID,
		)
	}
	if envelope.Data.OperatorVersionID == 0 {
		return OperatorDetail{}, errors.New("operator detail response missing operatorVersionId")
	}
	return OperatorDetail{
		ID:                strconv.FormatUint(envelope.Data.OperatorVersionID, 10),
		OperatorVersionID: envelope.Data.OperatorVersionID,
		OperatorKey:       strings.TrimSpace(envelope.Data.OperatorKey),
		Version:           strconv.Itoa(envelope.Data.Version),
		InputSchema:       append(json.RawMessage(nil), envelope.Data.InputSchema...),
		ParamsSchema:      append(json.RawMessage(nil), envelope.Data.ParamsSchema...),
		OutputSchema:      append(json.RawMessage(nil), envelope.Data.OutputSchema...),
		SourceHash:        strings.TrimSpace(envelope.Data.SourceHash),
		Language:          strings.TrimSpace(envelope.Data.Language),
		LanguageVersion:   strings.TrimSpace(envelope.Data.LanguageVersion),
		Code:              envelope.Data.SourceCode,
		Entry:             strings.TrimSpace(envelope.Data.Entry),
	}, nil
}
