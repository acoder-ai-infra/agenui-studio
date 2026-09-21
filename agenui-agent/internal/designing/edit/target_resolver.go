package edit

import (
	"sort"
	"strings"
)

// Candidates returns facts extracted from the current validated component
// tree. It deliberately performs no natural-language matching: semantic target
// selection belongs to the model, not to a Host-side keyword engine.
func Candidates(index Index) []Element {
	result := append([]Element(nil), index.Elements...)
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].ElementID < result[j].ElementID
	})
	return result
}

// Select accepts only an exact ID from the current tree. The model may choose
// either the semantic element ID or physical component ID returned by
// Candidates; invented and ambiguous IDs are rejected.
func Select(index Index, targetID string) (Element, error) {
	if strings.TrimSpace(targetID) == "" {
		return Element{}, ErrTargetNotFound
	}
	matches := ExactMatches(index, targetID)
	if len(matches) == 0 {
		return Element{}, ErrTargetNotFound
	}
	if len(matches) > 1 {
		return Element{}, ErrTargetAmbiguous
	}
	return matches[0], nil
}
