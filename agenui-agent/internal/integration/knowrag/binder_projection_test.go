package knowrag

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// foodToolSearchResult mirrors the real DestinationGoodFoodTool receipt shape:
// price lives in bottomDataCoupon positional text (description says 价格), the
// ranking label lives in bottomDataRank, and dozens of style/track noise
// fields compete for projection budget.
func foodToolSearchResult(t *testing.T) string {
	t.Helper()
	noise := map[string]any{}
	for index := 0; index < 60; index++ {
		noise[fmt.Sprintf("styleAttr%02d", index)] = map[string]any{
			"type": "string", "description": "样式属性",
		}
	}
	poiItem := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"poiName": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{"type": "string", "description": "POI名称文本"},
				},
			},
			"mainImg": map[string]any{"type": "string", "description": "POI主图URL"},
			"bottomDataCoupon": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"text": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"text": map[string]any{
									"type":        "string",
									"description": "文本内容: 位置[1]为价格描述, 位置[2]为价格修饰词",
								},
							},
						},
					},
				},
			},
			"bottomDataRank": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"text": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"text": map[string]any{
									"type":        "string",
									"description": "主文本内容，如榜单名称",
								},
							},
						},
					},
				},
			},
			"extra": map[string]any{"type": "object", "properties": noise},
		},
	}
	schema, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"data": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"voCard": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"poiList": map[string]any{"type": "array", "items": poiItem},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	example, err := json.Marshal(map[string]any{
		"data": map[string]any{"voCard": map[string]any{"poiList": []any{map[string]any{
			"poiName": map[string]any{"text": "柴火鸡"}, "mainImg": "https://img",
		}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"query": "美食榜单列表接口，返回美食名称、描述、主图、价格、排名字段",
		"total": 1,
		"results": []any{map[string]any{
			"path": "/ws/tools/general/DestinationGoodFoodTool", "method": "POST",
			"description": "获取终点附近优选美食餐厅推荐列表", "project_name": "agenui",
			"api_name": "终点附近优选餐厅推荐工具", "score": 0.62,
			"response_model": string(schema), "response_example": string(example),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestProcessToolResultForBinderKeepsPriceAndRankEvidence locks the P0 fix:
// the Binder-facing projection must carry enough field evidence (price and
// ranking paths included) instead of the 3KiB Search-model projection that
// silently dropped them and capped fields at 24.
func TestProcessToolResultForBinderKeepsPriceAndRankEvidence(t *testing.T) {
	t.Parallel()

	raw := foodToolSearchResult(t)
	query := "美食榜单列表接口，返回美食名称、描述、主图、价格、排名字段"
	_, binderCompact, err := ProcessToolResultForBinder(raw, query, "tenant-a", "user-a", 1)
	if err != nil {
		t.Fatalf("ProcessToolResultForBinder() error = %v", err)
	}
	for _, must := range []string{
		"bottomDataCoupon", "bottomDataRank", "poiName.text", "mainImg",
	} {
		if !strings.Contains(binderCompact, must) {
			t.Fatalf("binder projection lost %q:\n%s", must, binderCompact)
		}
	}

	// The Search-model projection contract must stay frozen at 3KiB.
	_, searchCompact, err := ProcessToolResult(raw, query, "tenant-a", "user-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(searchCompact) > maxBindingProjectionBytes {
		t.Fatalf("search projection budget drifted: %d bytes", len(searchCompact))
	}
}
