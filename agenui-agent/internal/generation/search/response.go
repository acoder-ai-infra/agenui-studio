package search

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

var ErrInvalidResponse = errors.New("invalid API search response")

const (
	maxOutputIdentityRunes = 256
	scoreTolerance         = 1e-12
)

// NormalizeAndValidateResponse freezes the model-visible Search response
// contract at the Search boundary. It does not re-prove repository admission,
// tenant ACL, or evidence ownership; callers must inject the real Service for
// those guarantees.
func NormalizeAndValidateResponse(
	request Request,
	response Response,
) (Response, error) {
	normalizedRequest, err := NormalizeAndValidateRequest(request)
	if err != nil {
		return Response{}, fmt.Errorf("%w: request: %v", ErrInvalidResponse, err)
	}
	request = normalizedRequest
	maximumResults := request.Limit
	if maximumResults < 1 || maximumResults > MaxResultsPerSearch ||
		len(response.Results) > maximumResults {
		return Response{}, fmt.Errorf(
			"%w: result count %d exceeds effective limit %d",
			ErrInvalidResponse,
			len(response.Results),
			maximumResults,
		)
	}
	if len(response.SelectedResultIDs) != len(response.Results) {
		return Response{}, fmt.Errorf(
			"%w: selected result IDs do not match result count",
			ErrInvalidResponse,
		)
	}
	for index := range response.Results {
		if len(response.Results[index].MatchedEvidence) > MaxEvidencePerResult {
			return Response{}, fmt.Errorf(
				"%w: result %d evidence count exceeds %d",
				ErrInvalidResponse,
				index,
				MaxEvidencePerResult,
			)
		}
	}

	normalized := cloneResponse(response)
	if normalized.Results == nil {
		normalized.Results = make([]Result, 0)
	}
	if normalized.SelectedResultIDs == nil {
		normalized.SelectedResultIDs = make([]string, 0)
	}
	resultIDs := make(map[string]struct{}, len(normalized.Results))
	revisionIDs := make(map[string]struct{}, len(normalized.Results))
	allowedDataSources := stringSet(request.AllowedDataSourceIDs)
	for index, result := range normalized.Results {
		for name, value := range map[string]string{
			"result_id":          result.ResultID,
			"data_source_id":     result.DataSourceID,
			"api_version":        result.APIVersion,
			"knowledge_revision": result.KnowledgeRevision,
		} {
			if !validBoundedRunes(value, maxOutputIdentityRunes) {
				return Response{}, fmt.Errorf(
					"%w: result %d has invalid %s",
					ErrInvalidResponse,
					index,
					name,
				)
			}
		}
		if !validContentHash(result.ContentHash) {
			return Response{}, fmt.Errorf(
				"%w: result %d has invalid content hash",
				ErrInvalidResponse,
				index,
			)
		}
		if len(allowedDataSources) > 0 {
			if _, allowed := allowedDataSources[result.DataSourceID]; !allowed {
				return Response{}, fmt.Errorf(
					"%w: result %d is outside allowed data sources",
					ErrInvalidResponse,
					index,
				)
			}
		}
		if !validUnitScore(result.Score) ||
			!validUnitScore(result.ScoreComponents.Lexical) ||
			!validUnitScore(result.ScoreComponents.Semantic) ||
			!validUnitScore(result.ScoreComponents.Capability) {
			return Response{}, fmt.Errorf(
				"%w: result %d has an invalid score",
				ErrInvalidResponse,
				index,
			)
		}
		if err := validateScoreSemantics(request, result); err != nil {
			return Response{}, fmt.Errorf(
				"%w: result %d: %v",
				ErrInvalidResponse,
				index,
				err,
			)
		}
		if _, duplicate := resultIDs[result.ResultID]; duplicate {
			return Response{}, fmt.Errorf(
				"%w: duplicate result ID %q",
				ErrInvalidResponse,
				result.ResultID,
			)
		}
		resultIDs[result.ResultID] = struct{}{}
		revisionID := strings.Join([]string{
			result.DataSourceID,
			result.APIVersion,
			result.KnowledgeRevision,
		}, "\x00")
		if _, duplicate := revisionIDs[revisionID]; duplicate {
			return Response{}, fmt.Errorf(
				"%w: duplicate knowledge revision",
				ErrInvalidResponse,
			)
		}
		revisionIDs[revisionID] = struct{}{}
		if normalized.SelectedResultIDs[index] != result.ResultID {
			return Response{}, fmt.Errorf(
				"%w: selected result IDs must match results in order",
				ErrInvalidResponse,
			)
		}
		if err := validateResponseEvidence(result.MatchedEvidence); err != nil {
			return Response{}, fmt.Errorf(
				"%w: result %d: %v",
				ErrInvalidResponse,
				index,
				err,
			)
		}
	}
	return normalized, nil
}

func validContentHash(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	for _, char := range value[len("sha256:"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validateScoreSemantics(request Request, result Result) error {
	if len(request.RequiredCapabilities) == 0 &&
		result.ScoreComponents.Capability != 0 {
		return errors.New("capability score must be zero without requirements")
	}
	if len(request.RequiredCapabilities) > 0 &&
		result.ScoreComponents.Capability != 1 {
		return errors.New("result lacks complete capability coverage")
	}
	expected := 0.45*result.ScoreComponents.Lexical +
		0.35*result.ScoreComponents.Semantic +
		0.20*result.ScoreComponents.Capability
	if math.Abs(result.Score-expected) > scoreTolerance {
		return errors.New("aggregate score does not match score components")
	}
	return nil
}

func validateResponseEvidence(evidence []Evidence) error {
	if len(evidence) > MaxEvidencePerResult {
		return fmt.Errorf(
			"evidence count exceeds %d",
			MaxEvidencePerResult,
		)
	}
	seen := make(map[string]struct{}, len(evidence))
	for index, item := range evidence {
		if !validEvidenceKind(item.Kind) {
			return fmt.Errorf("evidence %d has unknown kind", index)
		}
		if !validBoundedRunes(item.Ref, MaxEvidenceRefRunes) {
			return fmt.Errorf("evidence %d has invalid ref", index)
		}
		key := item.Kind + "\x00" + item.Ref
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("evidence %d is duplicated", index)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validEvidenceKind(kind string) bool {
	switch kind {
	case EvidenceKindSourceRef,
		EvidenceKindFieldSemantic,
		EvidenceKindResponseExample,
		EvidenceKindBindingCapability:
		return true
	default:
		return false
	}
}

func cloneResponse(response Response) Response {
	response.Results = append([]Result(nil), response.Results...)
	for index := range response.Results {
		response.Results[index].MatchedEvidence = append(
			[]Evidence(nil),
			response.Results[index].MatchedEvidence...,
		)
	}
	response.SelectedResultIDs = append(
		[]string(nil),
		response.SelectedResultIDs...,
	)
	return response
}
