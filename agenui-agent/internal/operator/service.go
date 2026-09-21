package operator

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type DetailClient interface {
	GetOperator(ctx context.Context, operatorID uint64) (OperatorDetail, error)
}

type Service struct {
	client   DetailClient
	registry Registry
}

func NewService(client DetailClient, registry Registry) (*Service, error) {
	if client == nil {
		return nil, errors.New("operator service: detail client is required")
	}
	if registry.executors == nil {
		registry = DefaultRegistry()
	}
	return &Service{client: client, registry: registry}, nil
}

func MustNewService(client DetailClient, registry Registry) *Service {
	service, err := NewService(client, registry)
	if err != nil {
		panic(err)
	}
	return service
}

func (s *Service) Execute(ctx context.Context, req ExecuteRequest) (result ExecuteResult) {
	if ctx == nil {
		ctx = context.Background()
	}
	operatorID := req.OperatorID
	operatorIDText := strconv.FormatUint(operatorID, 10)
	original := req.Value
	defer func() {
		if recovered := recover(); recovered != nil {
			result = fallbackResult(
				operatorID,
				result.Language,
				original,
				CodePanicRecovered,
				"operator execution recovered from panic",
				fmt.Sprint(recovered),
			)
		}
	}()

	if operatorID == 0 {
		return fallbackResult(
			0,
			"",
			original,
			CodeOperatorIDEmpty,
			"operator id is empty",
			"",
		)
	}
	if s == nil || s.client == nil {
		return fallbackResult(
			operatorID,
			"",
			original,
			CodeOperatorDetailFetchFail,
			"operator service is unavailable",
			"",
		)
	}
	if err := ctx.Err(); err != nil {
		return fallbackResult(
			operatorID,
			"",
			original,
			CodeOperatorDetailFetchFail,
			"operator detail fetch canceled",
			err.Error(),
		)
	}

	detail, err := s.client.GetOperator(ctx, operatorID)
	if err != nil {
		code := CodeOperatorDetailFetchFail
		message := "operator detail fetch failed"
		if errors.Is(err, ErrOperatorNotFound) {
			code = CodeOperatorNotFound
			message = "operator detail not found"
		}
		return fallbackResult(operatorID, "", original, code, message, err.Error())
	}
	if detail.ID == "" {
		detail.ID = operatorIDText
	}
	if detail.OperatorVersionID == 0 {
		detail.OperatorVersionID = operatorID
	}
	detail.Language = normalizeLanguage(detail.Language)
	if !operatorStatusExecutable(detail.Status) {
		return fallbackResult(
			operatorID,
			detail.Language,
			original,
			CodeOperatorNotActive,
			"operator is not executable",
			"status="+detail.Status,
		)
	}
	executor, ok := s.registry.Get(detail.Language)
	if !ok {
		return fallbackResult(
			operatorID,
			detail.Language,
			original,
			CodeUnsupportedLanguage,
			"operator language is not supported",
			detail.Language,
		)
	}
	return executor.Execute(ctx, detail, original, req.Context)
}

func (s *Service) ExecuteValue(
	ctx context.Context,
	operatorID uint64,
	value any,
) ExecuteResult {
	return s.Execute(ctx, ExecuteRequest{OperatorID: operatorID, Value: value})
}

func operatorStatusExecutable(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "active", "published", "enabled", "online":
		return true
	default:
		return false
	}
}
