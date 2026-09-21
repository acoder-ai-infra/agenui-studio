package runtime

import (
	"reflect"
	"testing"
)

func TestPathParsing(t *testing.T) {
	if _, err := parsePathBody("", '.'); err == nil {
		t.Fatal("empty path body succeeded")
	}
	validFields := []struct {
		path string
		want int
	}{
		{"$", 0}, {"$.a", 1}, {"a.b", 2}, {"$[0].a", 2},
		{"$.a[*].b", 3}, {`$.price_map['vip-member']`, 2}, {`$.price_map["vip-member"]`, 2},
	}
	for _, test := range validFields {
		tokens, err := parseFieldPath(test.path)
		if err != nil || len(tokens) != test.want {
			t.Fatalf("parseFieldPath(%q)=(%#v,%v), want %d tokens", test.path, tokens, err, test.want)
		}
	}
	for _, path := range []string{"", "$bad", "a..b", "a.", "a[]", "a[-1]", "a[bad]", `a['']`, `a["bad]`} {
		if _, err := parseFieldPath(path); err == nil {
			t.Fatalf("parseFieldPath(%q) succeeded", path)
		}
	}
	if key, err := unquoteMapKey(`'a\'b'`); err != nil || key != "a'b" {
		t.Fatalf("single quoted key=%q error=%v", key, err)
	}
	if key, err := unquoteMapKey(`"a\\b"`); err != nil || key != `a\b` {
		t.Fatalf("double quoted key=%q error=%v", key, err)
	}
	if _, err := unquoteMapKey(`""`); err == nil {
		t.Fatal("empty double quoted key succeeded")
	}
	validRefs := []string{"/a", "/items[*]/price", "/items[0]/price", "/a~1b/~0value"}
	for _, path := range validRefs {
		if _, err := parseRefKey(path); err != nil {
			t.Fatalf("parseRefKey(%q): %v", path, err)
		}
	}
	for _, path := range []string{"", "a", "/", "/a//b", "/a[]"} {
		if _, err := parseRefKey(path); err == nil {
			t.Fatalf("parseRefKey(%q) succeeded", path)
		}
	}
}

func TestExtractPathFormsAndFailures(t *testing.T) {
	root := mustJSON(t, `{"items":[{"name":"a","tags":["x","y"]},{"name":"b","tags":[]}],"map":{"vip-member":9}}`)
	for _, test := range []struct {
		path string
		want any
	}{
		{"$", root}, {"$.items[0].name", "a"}, {"$.items.1.name", "b"},
		{"$.items[*].name", []any{"a", "b"}}, {"$.items[0].tags[*]", []any{"x", "y"}},
		{`$.map['vip-member']`, float64(9)},
	} {
		got, ok := Extract(root, test.path)
		if !ok || !jsonEqual(got, test.want) {
			t.Fatalf("Extract(%q)=(%#v,%v), want %#v", test.path, got, ok, test.want)
		}
	}
	var numeric []pathMatch
	selectPathInto([]any{"ok"}, []pathToken{{kind: pathTokenField, field: "0"}}, nil, &numeric)
	if len(numeric) != 1 || numeric[0].value != "ok" {
		t.Fatalf("numeric field=%#v", numeric)
	}
	for _, path := range []string{"", "$.missing", "$.items.name", "$.items[9]", "$.items[0].tags[9]", "$.items[*].missing", "$.items[0].name[*]", "$bad"} {
		if value, ok := Extract(root, path); ok {
			t.Fatalf("Extract(%q)=%#v, want missing", path, value)
		}
	}
	if value, found, code := extractProjected(nil, nil); !found || value != nil || code != "" {
		t.Fatalf("root nil=(%#v,%v,%q)", value, found, code)
	}
	if _, found, code := extractProjected([]any{"a"}, []pathToken{{kind: pathTokenField, field: "bad"}}); found || code != CodeBindingSourcePathInvalid {
		t.Fatalf("bad dot index found=%v code=%s", found, code)
	}
	if _, found, code := extractProjected([]any{"a"}, []pathToken{{kind: pathTokenField, field: "9"}}); found || code != CodeFixedIndexOutOfRange {
		t.Fatalf("out-of-range dot index found=%v code=%s", found, code)
	}
	if _, found, code := extractProjected("bad", []pathToken{{kind: pathTokenField, field: "x"}}); found || code != CodeBindingSourcePathInvalid {
		t.Fatalf("scalar field found=%v code=%s", found, code)
	}
}

func TestSelectPathCoordinatesAndMissing(t *testing.T) {
	root := mustJSON(t, `{"groups":[{"items":[{"v":"a"},{"v":"b"}]},{"items":[{"v":"c"}]}]}`)
	tokens, _ := parseFieldPath("$.groups[*].items[*].v")
	matches := selectPath(root, tokens)
	if len(matches) != 3 || !reflect.DeepEqual(matches[0].coordinates, []int{0, 0}) || matches[2].value != "c" {
		t.Fatalf("matches=%#v", matches)
	}
	for _, test := range []struct {
		root any
		path string
		code string
	}{
		{map[string]any{"a": "x"}, "$.a.b", CodeBindingSourcePathInvalid},
		{map[string]any{}, "$.a", CodeMapKeyNotFound},
		{map[string]any{"a": "x"}, "$.a[0]", CodeBindingSourcePathInvalid},
		{map[string]any{"a": []any{}}, "$.a[0]", CodeFixedIndexOutOfRange},
		{map[string]any{"a": "x"}, "$.a[*]", CodeBindingSourcePathInvalid},
		{[]any{"a"}, "bad", CodeBindingSourcePathInvalid},
		{[]any{"a"}, "9", CodeFixedIndexOutOfRange},
		{"bad", "value", CodeBindingSourcePathInvalid},
	} {
		tokens, _ := parseFieldPath(test.path)
		got := selectPath(test.root, tokens)
		if len(got) != 1 || got[0].found || got[0].code != test.code {
			t.Fatalf("selectPath(%q)=%#v", test.path, got)
		}
	}
	tokens, _ = parseFieldPath("$.a[*]")
	if got := selectPath(map[string]any{"a": []any{}}, tokens); len(got) != 1 || !got[0].empty {
		t.Fatalf("empty wildcard=%#v", got)
	}
	tokens, _ = parseFieldPath("$[0]")
	if got := selectPath([]any{"ok"}, tokens); len(got) != 1 || got[0].value != "ok" {
		t.Fatalf("fixed index=%#v", got)
	}
	for _, test := range []struct {
		node  any
		token pathToken
		code  string
	}{
		{[]any{"a"}, pathToken{kind: pathTokenField, field: "bad"}, CodeBindingSourcePathInvalid},
		{[]any{"a"}, pathToken{kind: pathTokenField, field: "9"}, CodeFixedIndexOutOfRange},
		{"bad", pathToken{kind: pathTokenField, field: "x"}, CodeBindingSourcePathInvalid},
		{"bad", pathToken{kind: pathTokenIndex, index: 0}, CodeBindingSourcePathInvalid},
		{[]any{}, pathToken{kind: pathTokenIndex, index: 0}, CodeFixedIndexOutOfRange},
	} {
		var direct []pathMatch
		selectPathInto(test.node, []pathToken{test.token}, nil, &direct)
		if len(direct) != 1 || direct[0].code != test.code {
			t.Fatalf("direct=%#v", direct)
		}
	}
}

func TestAssignRefAndAssignFailures(t *testing.T) {
	if err := assignRef(nil, []pathToken{{kind: pathTokenField, field: "x"}}, nil, "x"); err == nil {
		t.Fatal("nil target root succeeded")
	}
	emptyRoot := map[string]any{}
	emptyTokens, _ := parseRefKey("/groups[*]/items[*]/value")
	if err := assignEmptyRef(emptyRoot, emptyTokens, []int{0}); err != nil || !jsonEqual(emptyRoot, map[string]any{"groups": []any{map[string]any{"items": []any{}}}}) {
		t.Fatalf("empty root=%#v error=%v", emptyRoot, err)
	}
	if err := assignEmptyRef(map[string]any{}, []pathToken{{kind: pathTokenWildcard}}, nil); err == nil {
		t.Fatal("root wildcard empty assignment succeeded")
	}
	if err := assignEmptyRef(map[string]any{}, []pathToken{{kind: pathTokenField, field: "x"}}, nil); err == nil {
		t.Fatal("path without wildcard empty assignment succeeded")
	}
	root := map[string]any{}
	tokens, _ := parseRefKey("/groups[*]/items[*]/value")
	if err := assignRef(root, tokens, []int{1, 2}, "x"); err != nil {
		t.Fatal(err)
	}
	groups := root["groups"].([]any)
	if groups[1].(map[string]any)["items"].([]any)[2].(map[string]any)["value"] != "x" {
		t.Fatalf("root=%#v", root)
	}
	for _, test := range []struct {
		tokens []pathToken
		coords []int
	}{
		{nil, nil},
		{[]pathToken{{kind: pathTokenField, field: "a"}}, []int{0}},
		{[]pathToken{{kind: pathTokenIndex, index: 0}}, nil},
		{[]pathToken{{kind: pathTokenWildcard}}, nil},
	} {
		if err := assignRef(map[string]any{}, test.tokens, test.coords, "x"); err == nil {
			t.Fatalf("assignRef(%#v,%#v) succeeded", test.tokens, test.coords)
		}
	}
	if _, _, err := assignPathNode(map[string]any{"a": "blocked"}, []pathToken{{kind: pathTokenField, field: "a"}, {kind: pathTokenField, field: "b"}}, nil, 0, "x"); err == nil {
		t.Fatal("expected field collision")
	}
	if _, _, err := assignPathNode(map[string]any{"a": "blocked"}, []pathToken{{kind: pathTokenField, field: "a"}, {kind: pathTokenIndex, index: 0}}, nil, 0, "x"); err == nil {
		t.Fatal("expected array collision")
	}
	if _, _, err := assignPathNode(nil, []pathToken{{kind: 99}}, nil, 0, "x"); err == nil {
		t.Fatal("expected unknown token")
	}
	if _, _, err := assignPathNode([]any{"blocked"}, []pathToken{{kind: pathTokenIndex, index: 0}, {kind: pathTokenField, field: "x"}}, nil, 0, "x"); err == nil {
		t.Fatal("expected nested array child collision")
	}
	if err := assignRef(map[string]any{}, []pathToken{{kind: pathTokenField, field: "x"}}, nil, "ok"); err != nil {
		t.Fatal(err)
	}
	plain := map[string]any{}
	if !Assign(plain, "a.b", 1) || !Assign(plain, "/c/d", 2) || Assign(plain, "/a[*]", 3) || Assign(plain, "", 3) {
		t.Fatalf("plain=%#v", plain)
	}
	if renderParam("{{a}}/{{b}}", map[string]any{"a": 1, "b": "x"}) != "1/x" {
		t.Fatal("renderParam mismatch")
	}
}

func TestPathTokenHelpers(t *testing.T) {
	if wildcardCount([]pathToken{{kind: pathTokenWildcard}, {kind: pathTokenField}}) != 1 {
		t.Fatal("wildcard count")
	}
	left := pathToken{kind: pathTokenField, field: "a"}
	if !samePathToken(left, left) || samePathToken(left, pathToken{kind: pathTokenField, field: "b"}) {
		t.Fatal("samePathToken")
	}
}

func FuzzExtractDoesNotPanic(f *testing.F) {
	root := map[string]any{"items": []any{}}
	for _, seed := range []string{"$.items[*].name", "$[0]", `$.map['key']`, "$.a..b", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, path string) {
		Extract(root, path)
	})
}
