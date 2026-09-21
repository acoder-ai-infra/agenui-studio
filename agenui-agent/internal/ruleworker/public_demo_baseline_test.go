package ruleworker

import "testing"

func TestPublicDemoBaselineSatisfiesSourceWorkerInputContract(t *testing.T) {
	baseline, err := LoadBaseRevision("../../configs/design-public/revisions/demo-v1")
	if err != nil {
		t.Fatalf("public baseline must be accepted by source worker: %v", err)
	}
	if got, want := baseline.LayoutIDs, []string{"layout.information-action", "layout.repeated-item-list", "layout.summary-card"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("public baseline layouts = %#v", baseline.LayoutIDs)
	}
}
