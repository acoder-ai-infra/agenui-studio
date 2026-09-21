package search

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode/utf8"
)

var ErrInvalidRequest = errors.New("invalid search request")

const (
	MaxResultsPerSearch  = 100
	MaxEvidencePerResult = 32
	MaxEvidenceRefRunes  = 512
	MaxQueryRunes        = 4_096
	MaxRequestTermRunes  = 128
	maxIdentityBytes     = 256
	maxTermsPerRequest   = 100
)

const (
	EvidenceKindSourceRef         = "source_ref"
	EvidenceKindFieldSemantic     = "field_semantic"
	EvidenceKindResponseExample   = "response_example"
	EvidenceKindBindingCapability = "binding_capability"
)

type Request struct {
	TenantID             string
	UserID               string
	Query                string
	RequiredCapabilities []string
	AllowedDataSourceIDs []string
	Limit                int
}

type Evidence struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

type ScoreComponents struct {
	Lexical    float64 `json:"lexical"`
	Semantic   float64 `json:"semantic"`
	Capability float64 `json:"capability"`
}

type RevisionKey struct {
	TenantID     string `json:"tenant_id"`
	DataSourceID string `json:"data_source_id"`
	APIVersion   string `json:"api_version"`
	Revision     string `json:"revision"`
}

type Result struct {
	ResultID          string          `json:"result_id"`
	DataSourceID      string          `json:"data_source_id"`
	APIVersion        string          `json:"api_version"`
	KnowledgeRevision string          `json:"knowledge_revision"`
	ContentHash       string          `json:"content_hash"`
	Score             float64         `json:"score"`
	ScoreComponents   ScoreComponents `json:"score_components"`
	MatchedEvidence   []Evidence      `json:"matched_evidence,omitempty"`
	Revision          RevisionKey     `json:"-"`
}

type Response struct {
	Results           []Result `json:"results"`
	SelectedResultIDs []string `json:"selected_result_ids"`
}

func NormalizeAndValidateRequest(request Request) (Request, error) {
	if len(request.RequiredCapabilities) > maxTermsPerRequest ||
		len(request.AllowedDataSourceIDs) > maxTermsPerRequest {
		return Request{}, fmt.Errorf("%w: too many request terms", ErrInvalidRequest)
	}
	if !validRawBoundedBytes(request.TenantID, maxIdentityBytes) ||
		!validRawBoundedBytes(request.UserID, maxIdentityBytes) ||
		!validRawBoundedRunes(request.Query, MaxQueryRunes) {
		return Request{}, fmt.Errorf("%w: raw identity or query exceeds bounds", ErrInvalidRequest)
	}
	for _, values := range [][]string{request.RequiredCapabilities, request.AllowedDataSourceIDs} {
		seen := make(map[string]struct{}, len(values))
		for _, value := range values {
			if !validRawBoundedRunes(value, MaxRequestTermRunes) {
				return Request{}, fmt.Errorf("%w: raw request term exceeds bounds", ErrInvalidRequest)
			}
			normalized := strings.TrimSpace(value)
			if !validBoundedRunes(normalized, MaxRequestTermRunes) {
				return Request{}, fmt.Errorf("%w: normalized request term is invalid", ErrInvalidRequest)
			}
			if _, duplicate := seen[normalized]; duplicate {
				return Request{}, fmt.Errorf("%w: duplicate normalized request term", ErrInvalidRequest)
			}
			seen[normalized] = struct{}{}
		}
	}
	request.TenantID = strings.TrimSpace(request.TenantID)
	request.UserID = strings.TrimSpace(request.UserID)
	request.Query = strings.TrimSpace(request.Query)
	request.RequiredCapabilities = normalizedStrings(request.RequiredCapabilities)
	request.AllowedDataSourceIDs = normalizedStrings(request.AllowedDataSourceIDs)
	if request.Limit == 0 {
		request.Limit = 20
	}
	if request.TenantID == "" || request.UserID == "" || request.Query == "" ||
		!validBoundedBytes(request.TenantID, maxIdentityBytes) ||
		!validBoundedBytes(request.UserID, maxIdentityBytes) ||
		!validBoundedRunes(request.Query, MaxQueryRunes) ||
		request.Limit < 1 || request.Limit > MaxResultsPerSearch {
		return Request{}, fmt.Errorf("%w: invalid identity, query, or limit", ErrInvalidRequest)
	}
	return request, nil
}

func validRawBoundedBytes(value string, maximum int) bool {
	return len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func validRawBoundedRunes(value string, maximum int) bool {
	return len(value) <= maximum*utf8.UTFMax && utf8.ValidString(value) &&
		!strings.ContainsRune(value, '\x00') && utf8.RuneCountInString(value) <= maximum
}

func validBoundedBytes(value string, maximum int) bool {
	return value != "" && validRawBoundedBytes(value, maximum)
}

func validBoundedRunes(value string, maximum int) bool {
	return value != "" && validRawBoundedRunes(value, maximum)
}

func validUnitScore(score float64) bool {
	return !math.IsNaN(score) && !math.IsInf(score, 0) && score >= 0 && score <= 1
}

func normalizedStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	slices.Sort(result)
	return slices.Compact(result)
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}
