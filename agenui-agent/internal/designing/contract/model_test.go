package contract

import "testing"

func TestCanonicalizeAndHashAreOrderIndependent(t *testing.T) {
	left := Draft{Goal: " 比较商品 ", Type: "list", Contents: []ContentItem{
		{ID: "product.price", Description: "价格"},
		{ID: "product.name", Description: "名称", Required: true},
	}, Actions: []ActionItem{{ID: "product.detail", Description: "查看详情"}}, Constraints: []string{"不增加预订", "不增加预订"}}
	right := Draft{Goal: "比较商品", Type: "list", Contents: []ContentItem{
		{ID: "product.name", Description: "名称", Required: true},
		{ID: "product.price", Description: "价格"},
	}, Actions: []ActionItem{{ID: "product.detail", Description: "查看详情"}}, Constraints: []string{"不增加预订"}}
	leftHash, err := Hash(left)
	if err != nil {
		t.Fatal(err)
	}
	rightHash, err := Hash(right)
	if err != nil {
		t.Fatal(err)
	}
	if leftHash != rightHash {
		t.Fatalf("hashes differ: %s != %s", leftHash, rightHash)
	}
}

func TestCanonicalizeRejectsInvalidDraft(t *testing.T) {
	tests := []Draft{
		{Type: "list", Contents: []ContentItem{{ID: "x", Description: "x"}}},
		{Goal: "x", Type: "grid", Contents: []ContentItem{{ID: "x", Description: "x"}}},
		{Goal: "x", Type: "list"},
		{Goal: "x", Type: "list", Contents: []ContentItem{{ID: "x", Description: "x"}, {ID: "x", Description: "y"}}},
	}
	for index, test := range tests {
		if _, err := Canonicalize(test); err == nil {
			t.Fatalf("case %d unexpectedly succeeded", index)
		}
	}
}

func TestCanonicalizePreservesExplicitDeliveryModeAndLegacyDefault(t *testing.T) {
	preview, err := Canonicalize(Draft{
		Goal: "静态预览", Type: "single", DeliveryMode: DeliveryModeDesignPreview,
		Contents: []ContentItem{{ID: "title", Description: "标题"}},
	})
	if err != nil || !preview.IsDesignPreview() {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	legacy, err := Canonicalize(Draft{
		Goal: "历史默认", Type: "single", Contents: []ContentItem{{ID: "title", Description: "标题"}},
	})
	if err != nil || legacy.IsDesignPreview() || legacy.DeliveryMode != "" {
		t.Fatalf("legacy=%+v err=%v", legacy, err)
	}
	if _, err := Canonicalize(Draft{
		Goal: "错误模式", Type: "single", DeliveryMode: "static",
		Contents: []ContentItem{{ID: "title", Description: "标题"}},
	}); err == nil {
		t.Fatal("invalid delivery mode was accepted")
	}
}
