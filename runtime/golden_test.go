package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestGoldenPackagesExecute(t *testing.T) {
	cases := map[string]struct {
		response any
		want     any
	}{
		"01-fixed-map-key.json":           {mustJSON(t, `{"price_map":{"vip-member":6800}}`), mustJSON(t, `{"product_price":"¥68.00"}`)},
		"02-fixed-array-index.json":       {mustJSON(t, `{"items":[{"price_cents":6800},{"price_cents":12800}]}`), mustJSON(t, `{"product_price":"¥68.00"}`)},
		"03-wildcard-coordinate-map.json": {mustJSON(t, `{"items":[{"title":"Museum","price_cents":6800},{"title":"Lake","price_cents":12800}]}`), mustJSON(t, `{"items":[{"title":"Museum","price":"¥68.00"},{"title":"Lake","price":"¥128.00"}]}`)},
		"04-list-join.json":               {mustJSON(t, `{"tags":["亲子","室内","免费"]}`), mustJSON(t, `{"tag_summary":"亲子 · 室内 · 免费"}`)},
		"05-list-sum.json":                {mustJSON(t, `{"values":[1,2,3]}`), mustJSON(t, `{"total":6}`)},
		"06-list-max.json":                {mustJSON(t, `{"values":[1,9,3]}`), mustJSON(t, `{"maximum":9}`)},
		"07-pick-format-money-chain.json": {mustJSON(t, `{"items":[{"id":"p1","price_cents":6800},{"id":"p2","price_cents":12800}]}`), mustJSON(t, `{"product_price":"¥68.00"}`)},
		"08-filter-map-join-chain.json":   {mustJSON(t, `{"items":[{"status":"open","name":"A"},{"status":"closed","name":"B"},{"status":"open","name":"C"}]}`), mustJSON(t, `{"open_names":"A,C"}`)},
		"09-nested-wildcards.json":        {mustJSON(t, `{"groups":[{"items":[{"price_cents":6800},{"price_cents":12800}]},{"items":[{"price_cents":9900}]},{"items":[]}]}`), mustJSON(t, `{"groups":[{"items":[{"price":"¥68.00"},{"price":"¥128.00"}]},{"items":[{"price":"¥99.00"}]},{"items":[]}]}`)},
		"10-empty-list.json":              {mustJSON(t, `{"items":[]}`), mustJSON(t, `{"items":[]}`)},
	}
	files, err := filepath.Glob("testdata/golden/*.json")
	if err != nil || len(files) != len(cases) {
		t.Fatalf("files=%v error=%v", files, err)
	}
	for _, path := range files {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			pkg, err := Load(raw)
			if err != nil {
				t.Fatal(err)
			}
			fixture := cases[name]
			responses := map[string]any{"ds-1": fixture.response}
			result, err := NewEngine(&fakeFetcher{responses: responses}, nil).Execute(context.Background(), pkg, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !jsonEqual(result.DataModel, fixture.want) {
				t.Fatalf("dataModel=%#v want=%#v", result.DataModel, fixture.want)
			}
		})
	}
}

func FuzzEngineDoesNotPanicOnDownstreamJSON(f *testing.F) {
	rawPackage, err := os.ReadFile("testdata/sample-package.json")
	if err != nil {
		f.Fatal(err)
	}
	pkg, err := Load(rawPackage)
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range []string{
		`{"items":[],"catalog_url":"https://example.com"}`,
		`{"items":null,"catalog_url":"https://example.com"}`,
		`{"items":[null,1,"x",{}],"catalog_url":"https://example.com"}`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		var response any
		if json.Unmarshal([]byte(raw), &response) != nil {
			return
		}
		_, _ = NewEngine(&fakeFetcher{responses: map[string]any{"ds-1": response}}, nil).
			Execute(context.Background(), pkg, nil)
	})
}
