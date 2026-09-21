package edit

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestPageCandidatesBoundsLargeCatalogAndPreservesEveryID(t *testing.T) {
	index := Index{Revision: "design@large"}
	for i := 0; i < 137; i++ {
		index.Elements = append(index.Elements, Element{
			ElementID: fmt.Sprintf("slot.%03d", i), ComponentID: fmt.Sprintf("component-%03d", i),
			ComponentType: "Text", SemanticRole: "title", ParentID: "root",
			EditablePaths: []string{"styles.font-size"},
			Properties:    map[string]any{"large_unbounded_value": string(make([]byte, 2048))},
		})
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := PageCandidates(index, CandidateFilter{SemanticRoles: []string{"title"}}, cursor, 17)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Candidates) > 17 {
			t.Fatalf("page size = %d", len(page.Candidates))
		}
		encoded, err := json.Marshal(page)
		if err != nil || len(encoded) >= 16*1024 {
			t.Fatalf("bounded page bytes=%d err=%v", len(encoded), err)
		}
		for _, candidate := range page.Candidates {
			if candidate.ElementID == "" || candidate.ComponentID == "" {
				t.Fatalf("candidate lost identity: %#v", candidate)
			}
			seen[candidate.ComponentID] = true
		}
		if !page.HasMore {
			break
		}
		if page.NextCursor == "" {
			t.Fatal("has_more page has no cursor")
		}
		cursor = page.NextCursor
	}
	if len(seen) != len(index.Elements) {
		t.Fatalf("seen %d candidates, want %d", len(seen), len(index.Elements))
	}
}

func TestPageCandidatesRejectsCursorFromAnotherDesign(t *testing.T) {
	first := Index{Revision: "design@1", Elements: []Element{{ElementID: "a", ComponentID: "a"}, {ElementID: "b", ComponentID: "b"}}}
	page, err := PageCandidates(first, CandidateFilter{}, "", 1)
	if err != nil || page.NextCursor == "" {
		t.Fatalf("first page=%#v err=%v", page, err)
	}
	second := first
	second.Revision = "design@2"
	if _, err := PageCandidates(second, CandidateFilter{}, page.NextCursor, 1); !errors.Is(err, ErrCandidateCursorStale) {
		t.Fatalf("stale cursor error = %v", err)
	}
}

func TestCandidateSummaryOmitsUnboundedProperties(t *testing.T) {
	page, err := PageCandidates(Index{Revision: "r1", Elements: []Element{{
		ElementID: "title", ComponentID: "title", ComponentType: "Text",
		Properties: map[string]any{"text": string(make([]byte, 65536))},
	}}}, CandidateFilter{}, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Candidates) != 1 || page.Candidates[0].ComponentID != "title" {
		t.Fatalf("page=%#v", page)
	}
}
