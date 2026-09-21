package edit

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

const (
	DefaultCandidatePageSize = 20
	MaxCandidatePageSize     = 50
)

var (
	ErrCandidateCursorInvalid = errors.New("design edit: candidate cursor is invalid")
	ErrCandidateCursorStale   = errors.New("design edit: candidate cursor is stale")
)

// CandidateFilter is deliberately structural. Natural-language target selection
// remains the model's responsibility; the Host only narrows on facts it owns.
type CandidateFilter struct {
	ComponentTypes []string `json:"component_types,omitempty"`
	SemanticRoles  []string `json:"semantic_roles,omitempty"`
	Relations      []string `json:"relations,omitempty"`
}

// CandidateSummary is the bounded discovery shape. Exact lookup returns the
// full Element, including current properties and styles.
type CandidateSummary struct {
	ElementID        string   `json:"element_id"`
	ComponentID      string   `json:"component_id"`
	ComponentType    string   `json:"component_type"`
	ParentID         string   `json:"parent_id,omitempty"`
	ChildCount       int      `json:"child_count,omitempty"`
	Relation         string   `json:"relation,omitempty"`
	SemanticRole     string   `json:"semantic_role,omitempty"`
	ContractItemID   string   `json:"contract_item_id,omitempty"`
	ContractActionID string   `json:"contract_action_id,omitempty"`
	Descriptions     []string `json:"descriptions,omitempty"`
	EditablePaths    []string `json:"editable_paths"`
}

type CandidatePage struct {
	Candidates   []CandidateSummary `json:"candidates"`
	MatchedCount int                `json:"matched_count"`
	HasMore      bool               `json:"has_more"`
	NextCursor   string             `json:"next_cursor,omitempty"`
}

type candidateCursor struct {
	Revision string `json:"revision"`
	Offset   int    `json:"offset"`
}

func PageCandidates(index Index, filter CandidateFilter, cursor string, pageSize int) (CandidatePage, error) {
	if pageSize <= 0 {
		pageSize = DefaultCandidatePageSize
	}
	if pageSize > MaxCandidatePageSize {
		pageSize = MaxCandidatePageSize
	}
	offset, err := decodeCandidateCursor(index.Revision, cursor)
	if err != nil {
		return CandidatePage{}, err
	}
	candidates := Candidates(index)
	filtered := make([]Element, 0, len(candidates))
	for _, candidate := range candidates {
		if candidateMatchesFilter(candidate, filter) {
			filtered = append(filtered, candidate)
		}
	}
	if offset > len(filtered) {
		return CandidatePage{}, ErrCandidateCursorInvalid
	}
	end := offset + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	page := CandidatePage{
		Candidates:   summarizeCandidates(filtered[offset:end]),
		MatchedCount: len(filtered),
		HasMore:      end < len(filtered),
	}
	if page.HasMore {
		page.NextCursor = encodeCandidateCursor(index.Revision, end)
	}
	return page, nil
}

func ExactMatches(index Index, targetID string) []Element {
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		return nil
	}
	result := make([]Element, 0, 1)
	for _, element := range index.Elements {
		if element.ElementID == targetID || element.ComponentID == targetID {
			result = append(result, element)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].ElementID != result[j].ElementID {
			return result[i].ElementID < result[j].ElementID
		}
		return result[i].ComponentID < result[j].ComponentID
	})
	return result
}

func SummarizeCandidates(candidates []Element) []CandidateSummary {
	return summarizeCandidates(candidates)
}

func summarizeCandidates(candidates []Element) []CandidateSummary {
	result := make([]CandidateSummary, 0, len(candidates))
	for _, candidate := range candidates {
		result = append(result, CandidateSummary{
			ElementID: candidate.ElementID, ComponentID: candidate.ComponentID,
			ComponentType: candidate.ComponentType, ParentID: candidate.ParentID,
			ChildCount: len(candidate.ChildIDs), Relation: candidate.Relation,
			SemanticRole: candidate.SemanticRole, ContractItemID: candidate.ContractItemID,
			ContractActionID: candidate.ContractActionID,
			Descriptions:     append([]string(nil), candidate.Descriptions...),
			EditablePaths:    append([]string(nil), candidate.EditablePaths...),
		})
	}
	return result
}

func candidateMatchesFilter(candidate Element, filter CandidateFilter) bool {
	return matchesStructuralValue(candidate.ComponentType, filter.ComponentTypes) &&
		matchesStructuralValue(candidate.SemanticRole, filter.SemanticRoles) &&
		matchesStructuralValue(candidate.Relation, filter.Relations)
}

func matchesStructuralValue(value string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if strings.EqualFold(strings.TrimSpace(candidate), value) {
			return true
		}
	}
	return false
}

func encodeCandidateCursor(revision string, offset int) string {
	raw, _ := json.Marshal(candidateCursor{Revision: revision, Offset: offset})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCandidateCursor(revision, encoded string) (int, error) {
	if strings.TrimSpace(encoded) == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return 0, ErrCandidateCursorInvalid
	}
	var cursor candidateCursor
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || cursor.Offset < 0 || strings.TrimSpace(cursor.Revision) == "" {
		return 0, ErrCandidateCursorInvalid
	}
	if cursor.Revision != revision {
		return 0, ErrCandidateCursorStale
	}
	return cursor.Offset, nil
}
