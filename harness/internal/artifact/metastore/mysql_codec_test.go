package metastore

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

type mysqlCodecNamedString string
type mysqlCodecNamedMap map[string]any
type mysqlCodecNamedSlice []any

func TestMySQLPreviewCodecNilStringAndBool(t *testing.T) {
	tests := []artifact.Preview{
		{},
		{Text: "plain text", Fields: map[string]any{
			"nil":    nil,
			"string": "value",
			"false":  false,
			"true":   true,
		}},
	}
	for _, original := range tests {
		original := original
		t.Run(original.Text, func(t *testing.T) {
			got, _ := roundTripMySQLPreview(t, original)
			if !reflect.DeepEqual(got, original) {
				t.Fatalf("round trip = %#v, want %#v", got, original)
			}
		})
	}
}

func TestMySQLPreviewCodecSortsMapEntriesDeterministically(t *testing.T) {
	first := map[string]any{}
	second := map[string]any{}
	keys := []string{"z", "a", "\xff", "middle", "\x00", "aa", "A"}
	for index, key := range keys {
		first[key] = int64(index)
	}
	for index := len(keys) - 1; index >= 0; index-- {
		second[keys[index]] = int64(index)
	}

	firstPayload, err := encodeMySQLPreview(artifact.Preview{Fields: first})
	if err != nil {
		t.Fatalf("encode first: %v", err)
	}
	secondPayload, err := encodeMySQLPreview(artifact.Preview{Fields: second})
	if err != nil {
		t.Fatalf("encode second: %v", err)
	}
	if !bytes.Equal(firstPayload, secondPayload) {
		t.Fatalf("deterministic payload mismatch:\nfirst  %s\nsecond %s", firstPayload, secondPayload)
	}

	var envelope mysqlPreviewPayload
	if err := json.Unmarshal(firstPayload, &envelope); err != nil {
		t.Fatalf("json.Unmarshal(): %v", err)
	}
	gotKeys := make([]string, 0, len(*envelope.Fields.Entries))
	for _, entry := range *envelope.Fields.Entries {
		key, err := decodeMySQLString(entry.Key)
		if err != nil {
			t.Fatalf("decode key: %v", err)
		}
		gotKeys = append(gotKeys, key)
	}
	if !sort.StringsAreSorted(gotKeys) {
		t.Fatalf("encoded entry keys = %q, want raw-byte sorted", gotKeys)
	}
}

func TestMySQLPreviewCodecPreservesArbitraryStringAndKeyBytes(t *testing.T) {
	text := "text\x00\xff"
	key := "key\x00\xfe"
	value := "value\x00\xff"
	original := artifact.Preview{
		Text: text,
		Fields: map[string]any{
			key:       value,
			"strings": []string{"\xff", "\x00"},
		},
	}

	_, payload := roundTripMySQLPreview(t, original)
	if !utf8.Valid(payload) {
		t.Fatalf("payload is not valid UTF-8: %q", payload)
	}
	for _, raw := range []string{text, key, value} {
		encoded := base64.StdEncoding.EncodeToString([]byte(raw))
		if !bytes.Contains(payload, []byte(encoded)) {
			t.Fatalf("payload %q does not contain base64 %q", payload, encoded)
		}
	}
}

func TestMySQLPreviewCodecFloatBitsAndInt64(t *testing.T) {
	floats := []float64{
		0,
		math.Copysign(0, -1),
		math.SmallestNonzeroFloat64,
		-math.SmallestNonzeroFloat64,
		math.MaxFloat64,
		math.Float64frombits(0x3fd5555555555555),
	}
	fields := map[string]any{
		"min_int64": int64(math.MinInt64),
		"max_int64": int64(math.MaxInt64),
	}
	for index, value := range floats {
		fields[string(rune('a'+index))] = value
	}

	got, _ := roundTripMySQLPreview(t, artifact.Preview{Fields: fields})
	for index, want := range floats {
		key := string(rune('a' + index))
		value, ok := got.Fields[key].(float64)
		if !ok || math.Float64bits(value) != math.Float64bits(want) {
			t.Fatalf("float %q bits = %016x, want %016x", key, math.Float64bits(value), math.Float64bits(want))
		}
	}
}

func TestMySQLPreviewCodecPortableContainersPreserveConcreteTypesAndNil(t *testing.T) {
	var nilStrings []string
	var nilItems []any
	var nilMap map[string]any
	original := artifact.Preview{Fields: map[string]any{
		"nil_strings":   nilStrings,
		"empty_strings": []string{},
		"nil_items":     nilItems,
		"empty_items":   []any{},
		"nil_map":       nilMap,
		"empty_map":     map[string]any{},
		"nested": []any{
			[]string{"one", "two"},
			map[string]any{"inner": []any{int64(7), true, nil}},
		},
	}}

	got, _ := roundTripMySQLPreview(t, original)
	if _, ok := got.Fields["empty_strings"].([]string); !ok {
		t.Fatalf("empty_strings type = %T, want []string", got.Fields["empty_strings"])
	}
	if _, ok := got.Fields["empty_items"].([]any); !ok {
		t.Fatalf("empty_items type = %T, want []any", got.Fields["empty_items"])
	}
}

func TestMySQLPreviewCodecAllowsSharedAcyclicSlices(t *testing.T) {
	outer := []any{int64(1), nil}
	outer[1] = outer[:1]
	shared := []any{int64(2)}
	original := artifact.Preview{Fields: map[string]any{
		"subslice": outer,
		"dag":      []any{shared, shared},
	}}

	roundTripMySQLPreview(t, original)
}

func TestMySQLPreviewCodecRejectsUnsupportedBuiltinConcreteTypes(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "int", value: int(1)},
		{name: "int slice", value: []int{1}},
		{name: "byte slice", value: []byte{1}},
		{name: "string map", value: map[string]string{"key": "value"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			payload, err := encodeMySQLPreview(artifact.Preview{Fields: map[string]any{"value": test.value}})
			if payload != nil {
				t.Fatalf("payload = %q, want nil", payload)
			}
			requireMySQLCodecInvalidArgument(t, err)
		})
	}
}

func TestMySQLPreviewCodecRejectsEveryOtherConcreteType(t *testing.T) {
	integer := 1
	values := []any{
		uint(1),
		float32(1),
		struct{ Value string }{Value: "value"},
		&integer,
		map[string]int{"value": 1},
	}
	for _, value := range values {
		payload, err := encodeMySQLPreview(artifact.Preview{Fields: map[string]any{"value": value}})
		if payload != nil {
			t.Fatalf("%T payload = %q, want nil", value, payload)
		}
		requireMySQLCodecInvalidArgument(t, err)
	}
}

func TestMySQLPreviewCodecRejectsNamedTypes(t *testing.T) {
	values := []any{
		mysqlCodecNamedString("named"),
		mysqlCodecNamedMap{"key": "value"},
		mysqlCodecNamedSlice{"value"},
	}
	for _, value := range values {
		payload, err := encodeMySQLPreview(artifact.Preview{Fields: map[string]any{"value": value}})
		if payload != nil {
			t.Fatalf("%T payload = %q, want nil", value, payload)
		}
		requireMySQLCodecInvalidArgument(t, err)
	}
}

func TestMySQLPreviewCodecRejectsNonFiniteFloat64(t *testing.T) {
	values := []float64{math.NaN(), math.Inf(1), math.Inf(-1)}
	for _, value := range values {
		payload, err := encodeMySQLPreview(artifact.Preview{Fields: map[string]any{"value": value}})
		if payload != nil {
			t.Fatalf("%v payload = %q, want nil", value, payload)
		}
		requireMySQLCodecInvalidArgument(t, err)
	}
}

func TestMySQLPreviewCodecRejectsFunctionsAndChannels(t *testing.T) {
	values := []any{
		func() {},
		(func())(nil),
		make(chan int),
		(chan int)(nil),
	}
	for _, value := range values {
		payload, err := encodeMySQLPreview(artifact.Preview{Fields: map[string]any{"value": value}})
		if payload != nil {
			t.Fatalf("%T payload = %q, want nil", value, payload)
		}
		requireMySQLCodecInvalidArgument(t, err)
	}
}

func TestMySQLPreviewCodecRejectsCyclicMap(t *testing.T) {
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	payload, err := encodeMySQLPreview(artifact.Preview{Fields: map[string]any{"value": cyclic}})
	if payload != nil {
		t.Fatalf("payload = %q, want nil", payload)
	}
	requireMySQLCodecInvalidArgument(t, err)
}

func TestMySQLPreviewCodecRejectsCyclicSlice(t *testing.T) {
	cyclic := make([]any, 1)
	cyclic[0] = cyclic
	payload, err := encodeMySQLPreview(artifact.Preview{Fields: map[string]any{"value": cyclic}})
	if payload != nil {
		t.Fatalf("payload = %q, want nil", payload)
	}
	requireMySQLCodecInvalidArgument(t, err)
}

func TestMySQLPreviewCodecRejectsUnknownVersion(t *testing.T) {
	payload, err := encodeMySQLPreview(artifact.Preview{})
	if err != nil {
		t.Fatalf("encodeMySQLPreview(): %v", err)
	}
	var envelope mysqlPreviewPayload
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("json.Unmarshal(): %v", err)
	}
	envelope.Version++
	payload, err = json.Marshal(envelope)
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}
	_, err = decodeMySQLPreview(payload)
	requireMySQLCodecInvalidArgument(t, err)
}

func TestMySQLPreviewCodecRejectsUnknownKind(t *testing.T) {
	envelope := mysqlPreviewPayload{
		Version: mysqlCodecVersion,
		Text:    encodeMySQLString(""),
		Fields:  mysqlTypedNode{Kind: "future-kind"},
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}
	_, err = decodeMySQLPreview(payload)
	requireMySQLCodecInvalidArgument(t, err)
}

func TestMySQLPreviewCodecRejectsBadBase64(t *testing.T) {
	bad := "!"
	emptyEntries := []mysqlTypedEntry{}
	stringEntries := []mysqlTypedEntry{{
		Key:   encodeMySQLString("key"),
		Value: mysqlTypedNode{Kind: "string", String: &bad},
	}}
	badKeyEntries := []mysqlTypedEntry{{
		Key:   bad,
		Value: mysqlTypedNode{Kind: "nil", Nil: true},
	}}
	badStrings := []string{bad}
	stringSliceEntries := []mysqlTypedEntry{{
		Key:   encodeMySQLString("key"),
		Value: mysqlTypedNode{Kind: "strings", Strings: &badStrings},
	}}
	tests := []mysqlPreviewPayload{
		{Version: mysqlCodecVersion, Text: bad, Fields: mysqlTypedNode{Kind: "map", Entries: &emptyEntries}},
		{Version: mysqlCodecVersion, Text: encodeMySQLString(""), Fields: mysqlTypedNode{Kind: "map", Entries: &stringEntries}},
		{Version: mysqlCodecVersion, Text: encodeMySQLString(""), Fields: mysqlTypedNode{Kind: "map", Entries: &badKeyEntries}},
		{Version: mysqlCodecVersion, Text: encodeMySQLString(""), Fields: mysqlTypedNode{Kind: "map", Entries: &stringSliceEntries}},
	}
	for _, envelope := range tests {
		payload, err := json.Marshal(envelope)
		if err != nil {
			t.Fatalf("json.Marshal(): %v", err)
		}
		_, err = decodeMySQLPreview(payload)
		requireMySQLCodecInvalidArgument(t, err)
	}
}

func TestMySQLPreviewCodecRejectsNonCanonicalBase64(t *testing.T) {
	for index, encoded := range []string{"YQ==\n", "YQ==\r", "YQ==\r\n", "/x=="} {
		decoded, err := decodeMySQLString(encoded)
		if decoded != "" {
			t.Fatalf("decodeMySQLString(%q) = %q, want empty result", encoded, decoded)
		}
		requireMySQLCodecInvalidArgument(t, err)
		if index < 3 && !errors.Is(err, errMySQLNonCanonicalBase64) {
			t.Fatalf("decodeMySQLString(%q) error chain = %v, want non-canonical cause", encoded, err)
		}
		if index == 3 {
			var corrupt base64.CorruptInputError
			if !errors.As(err, &corrupt) {
				t.Fatalf("decodeMySQLString(%q) error chain = %v, want base64.CorruptInputError", encoded, err)
			}
		}
	}
	decoded, err := decodeMySQLString("")
	if err != nil || decoded != "" {
		t.Fatalf("decodeMySQLString(empty) = %q, error %v; want empty, nil", decoded, err)
	}
}

func TestMySQLPreviewCodecEncodingWireFormatUnchanged(t *testing.T) {
	preview, err := encodeMySQLPreview(artifact.Preview{
		Text:      "t",
		Fields:    map[string]any{"k": "v"},
		Truncated: true,
	})
	if err != nil {
		t.Fatalf("encodeMySQLPreview(): %v", err)
	}
	wantPreview := `{"version":1,"text":"dA==","fields":{"kind":"map","nil":false,"entries":[{"key":"aw==","value":{"kind":"string","nil":false,"string":"dg=="}}]},"truncated":true}`
	if string(preview) != wantPreview {
		t.Fatalf("preview wire = %s, want %s", preview, wantPreview)
	}

	lineage, err := encodeMySQLLineage([]artifact.ArtifactLineage{{ArtifactRef: "r", Relation: artifact.LineageRelation("x")}})
	if err != nil {
		t.Fatalf("encodeMySQLLineage(): %v", err)
	}
	wantLineage := `{"version":1,"items":[{"artifact_ref":"cg==","relation":"eA=="}]}`
	if string(lineage) != wantLineage {
		t.Fatalf("lineage wire = %s, want %s", lineage, wantLineage)
	}

	metadata, err := encodeMySQLMetadata(map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("encodeMySQLMetadata(): %v", err)
	}
	wantMetadata := `{"version":1,"entries":[{"key":"aw==","value":"dg=="}]}`
	if string(metadata) != wantMetadata {
		t.Fatalf("metadata wire = %s, want %s", metadata, wantMetadata)
	}
}

func TestMySQLPreviewCodecRejectsBadOrNonFiniteFloatBits(t *testing.T) {
	values := []string{
		"000000000000000g",
		"0",
		"00000000000000000",
		"7ff0000000000000",
		"7ff8000000000001",
	}
	for _, bits := range values {
		bits := bits
		entries := []mysqlTypedEntry{{
			Key:   encodeMySQLString("float"),
			Value: mysqlTypedNode{Kind: "float64", Float: &bits},
		}}
		envelope := mysqlPreviewPayload{
			Version: mysqlCodecVersion,
			Text:    encodeMySQLString(""),
			Fields:  mysqlTypedNode{Kind: "map", Entries: &entries},
		}
		payload, err := json.Marshal(envelope)
		if err != nil {
			t.Fatalf("json.Marshal(): %v", err)
		}
		_, err = decodeMySQLPreview(payload)
		requireMySQLCodecInvalidArgument(t, err)
	}
}

func TestMySQLPreviewCodecRejectsNonHexFloatBitsWithParseCause(t *testing.T) {
	bits := "000000000000000g"
	entries := []mysqlTypedEntry{{
		Key:   encodeMySQLString("float"),
		Value: mysqlTypedNode{Kind: "float64", Float: &bits},
	}}
	payload, err := json.Marshal(mysqlPreviewPayload{
		Version: mysqlCodecVersion,
		Text:    encodeMySQLString(""),
		Fields:  mysqlTypedNode{Kind: "map", Entries: &entries},
	})
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}
	_, err = decodeMySQLPreview(payload)
	requireMySQLCodecInvalidArgument(t, err)
	var parseErr *strconv.NumError
	if !errors.As(err, &parseErr) {
		t.Fatalf("error chain = %v, want *strconv.NumError proving ParseUint path", err)
	}
}

func TestMySQLPreviewCodecRejectsDuplicateMapKeys(t *testing.T) {
	entries := []mysqlTypedEntry{
		{Key: encodeMySQLString("duplicate"), Value: mysqlTypedNode{Kind: "nil", Nil: true}},
		{Key: encodeMySQLString("duplicate"), Value: mysqlTypedNode{Kind: "bool", Bool: mysqlBoolPointer(true)}},
	}
	envelope := mysqlPreviewPayload{
		Version: mysqlCodecVersion,
		Text:    encodeMySQLString(""),
		Fields:  mysqlTypedNode{Kind: "map", Entries: &entries},
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}
	_, err = decodeMySQLPreview(payload)
	requireMySQLCodecInvalidArgument(t, err)
}

func TestMySQLPreviewCodecRejectsTrailingJSON(t *testing.T) {
	payload, err := encodeMySQLPreview(artifact.Preview{})
	if err != nil {
		t.Fatalf("encodeMySQLPreview(): %v", err)
	}
	payload = append(payload, []byte(` {"version":1}`)...)
	_, err = decodeMySQLPreview(payload)
	requireMySQLCodecInvalidArgument(t, err)
}

func TestMySQLPreviewCodecRejectsMalformedEncoding(t *testing.T) {
	valid, err := encodeMySQLPreview(artifact.Preview{})
	if err != nil {
		t.Fatalf("encodeMySQLPreview(): %v", err)
	}
	withUnknownField := append([]byte(nil), valid[:len(valid)-1]...)
	withUnknownField = append(withUnknownField, []byte(`,"unknown":true}`)...)
	emptyEntries := []mysqlTypedEntry{}
	text := encodeMySQLString("unexpected")
	boolean := true
	badInt := "not-an-int64"
	tests := [][]byte{
		[]byte(`{"version":`),
		withUnknownField,
		marshalMySQLPreviewEnvelope(t, mysqlTypedNode{Kind: "map"}),
		marshalMySQLPreviewEnvelope(t, mysqlTypedNode{Kind: "map", Nil: true, Entries: &emptyEntries}),
		marshalMySQLPreviewEnvelope(t, mysqlTypedNode{Kind: "string", String: &text, Bool: &boolean}),
		marshalMySQLPreviewEnvelope(t, mysqlTypedNode{Kind: "nil", Nil: true, String: &text}),
		marshalMySQLPreviewEnvelope(t, mysqlTypedNode{Kind: "int64", Int: &badInt}),
		marshalMySQLPreviewEnvelope(t, mysqlTypedNode{Kind: "string", String: &text}),
	}
	for index, payload := range tests {
		_, err := decodeMySQLPreview(payload)
		if !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
			t.Fatalf("case %d payload %s: error = %v, want %s", index, payload, err, artifact.ErrInvalidArgument)
		}
	}
}

func TestMySQLPreviewCodecRejectsMissingExplicitNilFalse(t *testing.T) {
	payloads := []string{
		`{"version":1,"text":"","fields":{"kind":"map","entries":[]},"truncated":false}`,
		`{"version":1,"text":"","fields":{"kind":"map","nil":false,"entries":[{"key":"aw==","value":{"kind":"string","string":"dg=="}}]},"truncated":false}`,
	}
	for _, payload := range payloads {
		_, err := decodeMySQLPreview([]byte(payload))
		requireMySQLCodecInvalidArgument(t, err)
	}
}

func TestMySQLPreviewCodecRejectsNullBusinessStrings(t *testing.T) {
	payloads := []string{
		`{"version":1,"text":null,"fields":{"kind":"map","nil":true},"truncated":false}`,
		`{"version":1,"text":"","fields":{"kind":"map","nil":false,"entries":[{"key":null,"value":{"kind":"nil","nil":true}}]},"truncated":false}`,
		`{"version":1,"text":"","fields":{"kind":"map","nil":false,"entries":[{"key":"aw==","value":{"kind":"string","nil":false,"string":null}}]},"truncated":false}`,
		`{"version":1,"text":"","fields":{"kind":"map","nil":false,"entries":[{"key":"aw==","value":{"kind":"strings","nil":false,"strings":[null]}}]},"truncated":false}`,
	}
	for _, payload := range payloads {
		_, err := decodeMySQLPreview([]byte(payload))
		requireMySQLCodecInvalidArgument(t, err)
	}
}

func TestMySQLPreviewCodecRejectsDuplicateOrCaseVariantObjectMembers(t *testing.T) {
	payloads := []string{
		`{"version":1,"text":"","text":"","fields":{"kind":"map","nil":true},"truncated":false}`,
		`{"version":1,"text":"","fields":{"kind":"map","nil":true,"nil":true},"truncated":false}`,
		`{"version":1,"text":"","fields":{"kind":"map","nil":false,"entries":[{"key":"","key":"","value":{"kind":"nil","nil":true}}]},"truncated":false}`,
		`{"Version":1,"text":"","fields":{"kind":"map","nil":true},"truncated":false}`,
		`{"version":1,"text":"","fields":{"Kind":"map","nil":true},"truncated":false}`,
		`{"version":1,"text":"","fields":{"kind":"map","nil":false,"entries":[{"Key":"","value":{"kind":"nil","nil":true}}]},"truncated":false}`,
	}
	for _, payload := range payloads {
		_, err := decodeMySQLPreview([]byte(payload))
		requireMySQLCodecInvalidArgument(t, err)
	}
}

func marshalMySQLPreviewEnvelope(t *testing.T, fields mysqlTypedNode) []byte {
	t.Helper()
	payload, err := json.Marshal(mysqlPreviewPayload{
		Version: mysqlCodecVersion,
		Text:    encodeMySQLString(""),
		Fields:  fields,
	})
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}
	return payload
}

func TestMySQLPreviewCodecReturnsIndependentDecodedContainers(t *testing.T) {
	original := artifact.Preview{Fields: map[string]any{
		"map":     map[string]any{"value": "original"},
		"items":   []any{map[string]any{"value": int64(1)}},
		"strings": []string{"original"},
	}}
	payload, err := encodeMySQLPreview(original)
	if err != nil {
		t.Fatalf("encodeMySQLPreview(): %v", err)
	}
	original.Fields["map"].(map[string]any)["value"] = "input mutation"
	original.Fields["items"].([]any)[0].(map[string]any)["value"] = int64(2)
	original.Fields["strings"].([]string)[0] = "input mutation"

	first, err := decodeMySQLPreview(payload)
	if err != nil {
		t.Fatalf("first decode: %v", err)
	}
	second, err := decodeMySQLPreview(payload)
	if err != nil {
		t.Fatalf("second decode: %v", err)
	}
	first.Fields["map"].(map[string]any)["value"] = "first mutation"
	first.Fields["items"].([]any)[0].(map[string]any)["value"] = int64(3)
	first.Fields["strings"].([]string)[0] = "first mutation"

	if second.Fields["map"].(map[string]any)["value"] != "original" ||
		second.Fields["items"].([]any)[0].(map[string]any)["value"] != int64(1) ||
		second.Fields["strings"].([]string)[0] != "original" {
		t.Fatalf("second decode was aliased: %#v", second)
	}
}

func TestMySQLLineageCodecNilEmptyBinaryDeterministicAndIsolation(t *testing.T) {
	nilPayload, err := encodeMySQLLineage(nil)
	if err != nil || nilPayload != nil {
		t.Fatalf("encode nil = %q, error %v; want nil, nil", nilPayload, err)
	}
	nilValue, err := decodeMySQLLineage(nil)
	if err != nil || nilValue != nil {
		t.Fatalf("decode nil = %#v, error %v; want nil, nil", nilValue, err)
	}

	empty := []artifact.ArtifactLineage{}
	emptyPayload, err := encodeMySQLLineage(empty)
	if err != nil || emptyPayload == nil {
		t.Fatalf("encode empty = %q, error %v; want non-nil payload", emptyPayload, err)
	}
	emptyValue, err := decodeMySQLLineage(emptyPayload)
	if err != nil || emptyValue == nil || len(emptyValue) != 0 {
		t.Fatalf("decode empty = %#v, error %v; want non-nil empty", emptyValue, err)
	}

	original := []artifact.ArtifactLineage{
		{ArtifactRef: "ref-\x00\xff", Relation: artifact.LineageRelation("relation-\xfe")},
		{ArtifactRef: "second", Relation: artifact.LineageReferenced},
	}
	payload, err := encodeMySQLLineage(original)
	if err != nil {
		t.Fatalf("encodeMySQLLineage(): %v", err)
	}
	repeated, err := encodeMySQLLineage(append([]artifact.ArtifactLineage(nil), original...))
	if err != nil || !bytes.Equal(payload, repeated) {
		t.Fatalf("deterministic lineage encoding = %q, error %v; want %q", repeated, err, payload)
	}
	original[0].ArtifactRef = "input mutation"
	first, err := decodeMySQLLineage(payload)
	if err != nil {
		t.Fatalf("first decode: %v", err)
	}
	second, err := decodeMySQLLineage(payload)
	if err != nil {
		t.Fatalf("second decode: %v", err)
	}
	first[0].ArtifactRef = "first mutation"
	if second[0].ArtifactRef != "ref-\x00\xff" || second[0].Relation != artifact.LineageRelation("relation-\xfe") {
		t.Fatalf("second decode = %#v, want independent binary-safe copy", second)
	}
}

func TestMySQLLineageCodecRejectsMalformedPayload(t *testing.T) {
	valid, err := encodeMySQLLineage([]artifact.ArtifactLineage{{ArtifactRef: "ref", Relation: artifact.LineageReferenced}})
	if err != nil {
		t.Fatalf("encodeMySQLLineage(): %v", err)
	}
	trailing := append(append([]byte(nil), valid...), []byte(` {}`)...)
	unknown := append([]byte(nil), valid[:len(valid)-1]...)
	unknown = append(unknown, []byte(`,"unknown":true}`)...)
	badBase64, err := json.Marshal(mysqlLineagePayload{
		Version: mysqlCodecVersion,
		Items:   []mysqlLineageItem{{ArtifactRef: "!", Relation: encodeMySQLString("relation")}},
	})
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}
	for _, payload := range [][]byte{trailing, unknown, badBase64, {}} {
		_, err := decodeMySQLLineage(payload)
		requireMySQLCodecInvalidArgument(t, err)
	}
}

func TestMySQLLineageCodecRejectsMissingNullDuplicateOrCaseVariantMembers(t *testing.T) {
	payloads := []string{
		`{"version":1,"items":[{}]}`,
		`{"version":1,"items":[null]}`,
		`{"version":1,"items":[{"artifact_ref":null,"relation":""}]}`,
		`{"version":1,"items":[{"artifact_ref":"","relation":null}]}`,
		`{"version":1,"version":1,"items":[]}`,
		`{"version":1,"items":[{"artifact_ref":"","artifact_ref":"","relation":""}]}`,
		`{"Version":1,"items":[]}`,
		`{"version":1,"Items":[]}`,
		`{"version":1,"items":[{"Artifact_Ref":"","relation":""}]}`,
	}
	for _, payload := range payloads {
		_, err := decodeMySQLLineage([]byte(payload))
		requireMySQLCodecInvalidArgument(t, err)
	}
}

func TestMySQLMetadataCodecNilEmptyBinaryDeterministicAndIsolation(t *testing.T) {
	nilPayload, err := encodeMySQLMetadata(nil)
	if err != nil || nilPayload != nil {
		t.Fatalf("encode nil = %q, error %v; want nil, nil", nilPayload, err)
	}
	nilValue, err := decodeMySQLMetadata(nil)
	if err != nil || nilValue != nil {
		t.Fatalf("decode nil = %#v, error %v; want nil, nil", nilValue, err)
	}

	empty := map[string]string{}
	emptyPayload, err := encodeMySQLMetadata(empty)
	if err != nil || emptyPayload == nil {
		t.Fatalf("encode empty = %q, error %v; want non-nil payload", emptyPayload, err)
	}
	emptyValue, err := decodeMySQLMetadata(emptyPayload)
	if err != nil || emptyValue == nil || len(emptyValue) != 0 {
		t.Fatalf("decode empty = %#v, error %v; want non-nil empty", emptyValue, err)
	}

	firstInput := map[string]string{}
	firstInput["z\xff"] = "last\x00"
	firstInput["a\x00"] = "first\xff"
	secondInput := map[string]string{}
	secondInput["a\x00"] = "first\xff"
	secondInput["z\xff"] = "last\x00"
	payload, err := encodeMySQLMetadata(firstInput)
	if err != nil {
		t.Fatalf("encodeMySQLMetadata(): %v", err)
	}
	repeated, err := encodeMySQLMetadata(secondInput)
	if err != nil || !bytes.Equal(payload, repeated) {
		t.Fatalf("deterministic metadata encoding = %q, error %v; want %q", repeated, err, payload)
	}
	firstInput["a\x00"] = "input mutation"
	first, err := decodeMySQLMetadata(payload)
	if err != nil {
		t.Fatalf("first decode: %v", err)
	}
	second, err := decodeMySQLMetadata(payload)
	if err != nil {
		t.Fatalf("second decode: %v", err)
	}
	first["a\x00"] = "first mutation"
	if second["a\x00"] != "first\xff" || second["z\xff"] != "last\x00" {
		t.Fatalf("second decode = %#v, want independent binary-safe copy", second)
	}
}

func TestMySQLMetadataCodecRejectsDuplicateKeysAndMalformedPayload(t *testing.T) {
	duplicate, err := json.Marshal(mysqlMetadataPayload{
		Version: mysqlCodecVersion,
		Entries: []mysqlMetadataEntry{
			{Key: encodeMySQLString("key"), Value: encodeMySQLString("first")},
			{Key: encodeMySQLString("key"), Value: encodeMySQLString("second")},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}
	badBase64, err := json.Marshal(mysqlMetadataPayload{
		Version: mysqlCodecVersion,
		Entries: []mysqlMetadataEntry{{Key: "!", Value: encodeMySQLString("value")}},
	})
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}
	unknownVersion, err := json.Marshal(mysqlMetadataPayload{Version: mysqlCodecVersion + 1, Entries: []mysqlMetadataEntry{}})
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}
	for _, payload := range [][]byte{duplicate, badBase64, unknownVersion, {}} {
		_, err := decodeMySQLMetadata(payload)
		requireMySQLCodecInvalidArgument(t, err)
	}
}

func TestMySQLMetadataCodecRejectsMissingNullDuplicateOrCaseVariantMembers(t *testing.T) {
	payloads := []string{
		`{"version":1,"entries":[{}]}`,
		`{"version":1,"entries":[null]}`,
		`{"version":1,"entries":[{"key":null,"value":""}]}`,
		`{"version":1,"entries":[{"key":"","value":null}]}`,
		`{"version":1,"version":1,"entries":[]}`,
		`{"version":1,"entries":[{"key":"","key":"","value":""}]}`,
		`{"Version":1,"entries":[]}`,
		`{"version":1,"Entries":[]}`,
		`{"version":1,"entries":[{"Key":"","value":""}]}`,
	}
	for _, payload := range payloads {
		_, err := decodeMySQLMetadata([]byte(payload))
		requireMySQLCodecInvalidArgument(t, err)
	}
}

func TestMySQLTimePreservesSecondsNanosAndInstant(t *testing.T) {
	location := time.FixedZone("non-UTC", 9*60*60+37)
	times := []time.Time{
		time.Unix(0, 1).UTC(),
		time.Unix(0, 999999999).UTC(),
		time.Date(1960, time.January, 2, 3, 4, 5, 1, location),
		time.Date(2040, time.December, 31, 23, 59, 59, 999999999, location),
	}
	for _, original := range times {
		columns := encodeNullableMySQLTime(original)
		if !columns.Seconds.Valid || columns.Seconds.Int64 != original.Unix() {
			t.Fatalf("seconds for %v = %#v, want %d", original, columns.Seconds, original.Unix())
		}
		if !columns.Nanoseconds.Valid || columns.Nanoseconds.Int64 != int64(original.Nanosecond()) {
			t.Fatalf("nanos for %v = %#v, want %d", original, columns.Nanoseconds, original.Nanosecond())
		}
		decoded, err := decodeNullableMySQLTime(columns)
		if err != nil {
			t.Fatalf("decode %v: %v", original, err)
		}
		if !decoded.Equal(original) {
			t.Fatalf("decoded = %v, want same instant as %v", decoded, original)
		}
		if decoded.Location() != time.UTC {
			t.Fatalf("decoded location = %v, want UTC", decoded.Location())
		}
	}
}

func TestMySQLTimeNullableZeroAndPairValidation(t *testing.T) {
	zeroColumns := encodeNullableMySQLTime(time.Time{})
	if zeroColumns.Seconds.Valid || zeroColumns.Nanoseconds.Valid {
		t.Fatalf("zero columns = %#v, want paired NULL", zeroColumns)
	}
	zero, err := decodeNullableMySQLTime(zeroColumns)
	if err != nil || !zero.IsZero() {
		t.Fatalf("decode zero = %v, error %v; want Go zero time", zero, err)
	}

	invalid := []mysqlTimeColumns{
		{Seconds: sql.NullInt64{Valid: true}, Nanoseconds: sql.NullInt64{}},
		{Seconds: sql.NullInt64{}, Nanoseconds: sql.NullInt64{Valid: true}},
		{Seconds: sql.NullInt64{Valid: true}, Nanoseconds: sql.NullInt64{Int64: -1, Valid: true}},
		{Seconds: sql.NullInt64{Valid: true}, Nanoseconds: sql.NullInt64{Int64: 1_000_000_000, Valid: true}},
	}
	for _, columns := range invalid {
		_, err := decodeNullableMySQLTime(columns)
		requireMySQLCodecInvalidArgument(t, err)
	}
}

func TestMySQLTimeRequiredZeroAndValidation(t *testing.T) {
	columns := encodeRequiredMySQLTime(time.Time{})
	if !columns.Seconds.Valid || !columns.Nanoseconds.Valid {
		t.Fatalf("required zero columns = %#v, want non-NULL pair", columns)
	}
	decoded, err := decodeRequiredMySQLTime(columns)
	if err != nil || !decoded.Equal(time.Time{}) {
		t.Fatalf("decode required zero = %v, error %v; want zero instant", decoded, err)
	}

	for _, invalid := range []mysqlTimeColumns{
		{},
		{Seconds: sql.NullInt64{Int64: 1, Valid: true}},
		{Seconds: sql.NullInt64{Int64: 1, Valid: true}, Nanoseconds: sql.NullInt64{Int64: 1_000_000_000, Valid: true}},
	} {
		_, err := decodeRequiredMySQLTime(invalid)
		requireMySQLCodecInvalidArgument(t, err)
	}
}

func TestMySQLArtifactRowPurgeFieldsRoundTrip(t *testing.T) {
	purgedAt := time.Date(2026, time.July, 14, 8, 9, 10, 987_654_321, time.FixedZone("purged-non-UTC", 5*60*60+43))
	tests := []struct {
		status   artifact.PurgeStatus
		purgedAt time.Time
	}{
		{status: artifact.PurgeStatusPending},
		{status: artifact.PurgeStatusLeased},
		{status: artifact.PurgeStatusRetrying},
		{status: artifact.PurgeStatusPurged, purgedAt: purgedAt},
	}
	for _, test := range tests {
		t.Run(string(test.status), func(t *testing.T) {
			meta := portableMySQLMetaFixture()
			meta.PurgeStatus = test.status
			meta.PurgedAt = test.purgedAt
			row, immediate, err := prepareMySQLArtifactRow(meta)
			if err != nil {
				t.Fatalf("prepareMySQLArtifactRow() error = %v", err)
			}
			if immediate == nil {
				t.Fatal("prepareMySQLArtifactRow() immediate result = nil")
			}
			if immediate.PurgeStatus != test.status || !reflect.DeepEqual(immediate.PurgedAt, test.purgedAt) {
				t.Fatalf("immediate purge fields = (%q, %#v), want (%q, %#v)", immediate.PurgeStatus, immediate.PurgedAt, test.status, test.purgedAt)
			}

			decoded, err := decodeMySQLArtifactRow(row)
			if err != nil {
				t.Fatalf("decodeMySQLArtifactRow() error = %v", err)
			}
			if decoded.PurgeStatus != test.status {
				t.Fatalf("decoded PurgeStatus = %q, want %q", decoded.PurgeStatus, test.status)
			}
			if test.purgedAt.IsZero() {
				if !decoded.PurgedAt.IsZero() {
					t.Fatalf("decoded PurgedAt = %v, want zero", decoded.PurgedAt)
				}
			} else if !decoded.PurgedAt.Equal(test.purgedAt) || decoded.PurgedAt.Location() != time.UTC {
				t.Fatalf("decoded PurgedAt = %#v, want same instant as %#v in UTC", decoded.PurgedAt, test.purgedAt)
			}
		})
	}
}

func TestMySQLRowCodecPreparesPersistedRowAndImmediateClone(t *testing.T) {
	meta := portableMySQLMetaFixture()
	if reflect.DeepEqual(meta.CreatedAt, meta.CreatedAt.Round(0)) {
		t.Fatal("fixture CreatedAt does not carry a monotonic reading")
	}
	row, immediate, err := prepareMySQLArtifactRow(meta)
	if err != nil {
		t.Fatalf("prepareMySQLArtifactRow(): %v", err)
	}
	if immediate == nil || !reflect.DeepEqual(immediate, &meta) {
		t.Fatalf("immediate clone = %#v, want reflect.DeepEqual to input %#v", immediate, meta)
	}
	if !reflect.DeepEqual(immediate.CreatedAt, meta.CreatedAt) {
		t.Fatalf("immediate CreatedAt lost monotonic/location representation: got %#v want %#v", immediate.CreatedAt, meta.CreatedAt)
	}

	meta.Preview.Fields["nested"].([]any)[0] = int64(99)
	meta.DerivedFrom[0].ArtifactRef = "input mutation"
	meta.Metadata["key-\xff"] = "input mutation"
	if immediate.Preview.Fields["nested"].([]any)[0] != int64(math.MinInt64) ||
		immediate.DerivedFrom[0].ArtifactRef != "source-\x00\xff" ||
		immediate.Metadata["key-\xff"] != "value-\x00" {
		t.Fatalf("immediate clone aliases input: %#v", immediate)
	}

	reread, err := decodeMySQLArtifactRow(row)
	if err != nil {
		t.Fatalf("decodeMySQLArtifactRow(): %v", err)
	}
	for _, pair := range []struct {
		name string
		got  time.Time
		want time.Time
	}{
		{name: "created", got: reread.CreatedAt, want: immediate.CreatedAt},
		{name: "expires", got: reread.ExpiresAt, want: immediate.ExpiresAt},
		{name: "deleted", got: reread.DeletedAt, want: immediate.DeletedAt},
	} {
		if !pair.got.Equal(pair.want) {
			t.Fatalf("reread %s time = %v, want same instant as %v", pair.name, pair.got, pair.want)
		}
		if pair.got.Location() != time.UTC {
			t.Fatalf("reread %s location = %v, want UTC", pair.name, pair.got.Location())
		}
	}
	want := *immediate
	want.CreatedAt = reread.CreatedAt
	want.ExpiresAt = reread.ExpiresAt
	want.DeletedAt = reread.DeletedAt
	if !reflect.DeepEqual(reread, &want) {
		t.Fatalf("reread row = %#v, want %#v after time representation normalization", reread, want)
	}

	immediate.Preview.Fields["nested"].([]any)[0] = int64(100)
	immediate.DerivedFrom[0].ArtifactRef = "clone mutation"
	immediate.Metadata["key-\xff"] = "clone mutation"
	if reread.Preview.Fields["nested"].([]any)[0] != int64(math.MinInt64) ||
		reread.DerivedFrom[0].ArtifactRef != "source-\x00\xff" ||
		reread.Metadata["key-\xff"] != "value-\x00" {
		t.Fatalf("reread row aliases immediate clone: %#v", reread)
	}
}

func TestMySQLRowCodecDoesNotAddArtifactDomainValidation(t *testing.T) {
	meta := portableMySQLMetaFixture()
	meta.ArtifactID = ""
	meta.ArtifactRef = ""
	meta.SizeBytes = -99
	meta.OwnerModule = artifact.OwnerModule("unknown-owner")
	meta.ArtifactType = artifact.ArtifactType("unknown-type")
	meta.Visibility = artifact.Visibility("unknown-visibility")
	meta.RetentionPolicy = artifact.RetentionPolicy("unknown-retention")
	meta.Status = artifact.ArtifactStatus("unknown-status")
	meta.DeleteReason = artifact.DeleteReason("unknown-reason")
	if _, clone, err := prepareMySQLArtifactRow(meta); err != nil || clone == nil || !reflect.DeepEqual(clone, &meta) {
		t.Fatalf("prepare domain aliases = %#v, error %v; want unchanged clone", clone, err)
	}
}

func TestMySQLRowCodecRejectsUnsupportedPreviewBeforeProducingOutputs(t *testing.T) {
	meta := portableMySQLMetaFixture()
	meta.Preview.Fields["unsupported"] = []byte{1}
	row, clone, err := prepareMySQLArtifactRow(meta)
	if !reflect.DeepEqual(row, mysqlArtifactRow{}) {
		t.Fatalf("row = %#v, want zero row", row)
	}
	if clone != nil {
		t.Fatalf("clone = %#v, want nil", clone)
	}
	requireMySQLCodecInvalidArgument(t, err)
}

func TestMySQLRowCodecRejectsCorruptPayloadOrTimeColumns(t *testing.T) {
	row, _, err := prepareMySQLArtifactRow(portableMySQLMetaFixture())
	if err != nil {
		t.Fatalf("prepareMySQLArtifactRow(): %v", err)
	}
	tests := []mysqlArtifactRow{row, row, row}
	tests[0].previewPayload = []byte(`{"version":`)
	tests[1].expiresAt.Nanoseconds.Valid = false
	tests[2].createdAt.Nanoseconds.Int64 = 1_000_000_000
	for _, corrupt := range tests {
		got, err := decodeMySQLArtifactRow(corrupt)
		if got != nil {
			t.Fatalf("decoded corrupt row = %#v, want nil", got)
		}
		requireMySQLCodecInvalidArgument(t, err)
	}
}

func mysqlBoolPointer(value bool) *bool {
	return &value
}
