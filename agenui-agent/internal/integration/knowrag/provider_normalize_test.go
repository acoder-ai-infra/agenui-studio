package knowrag

import "testing"

// TestNormalizedJSONTextUnwrapsMCPContentEnvelope locks the tool-result
// normalization contract: SDK paths sometimes hand interceptors the raw MCP
// content envelope instead of the extracted text payload, and both envelope
// forms must unwrap to the inner search JSON while ordinary payloads and
// non-text envelopes stay untouched.
func TestNormalizedJSONTextUnwrapsMCPContentEnvelope(t *testing.T) {
	t.Parallel()

	inner := `{"query":"补齐价格","results":[],"total":0}`
	cases := map[string]struct {
		input string
		want  string
	}{
		"plain payload": {input: inner, want: inner},
		"object envelope": {
			input: `{"type":"text","text":"{\"query\":\"补齐价格\",\"results\":[],\"total\":0}"}`,
			want:  inner,
		},
		"array envelope": {
			input: `[{"type":"text","text":"{\"query\":\"补齐价格\",\"results\":[],\"total\":0}"}]`,
			want:  inner,
		},
		"quoted then envelope": {
			input: `"{\"type\":\"text\",\"text\":\"{\\\"query\\\":\\\"补齐价格\\\",\\\"results\\\":[],\\\"total\\\":0}\"}"`,
			want:  inner,
		},
		"non-text envelope stays": {
			input: `{"type":"image","text":"ignored"}`,
			want:  `{"type":"image","text":"ignored"}`,
		},
		"multi content stays": {
			input: `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`,
			want:  `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`,
		},
	}
	for name, testCase := range cases {
		if got := normalizedJSONText(testCase.input); got != testCase.want {
			t.Fatalf("%s: normalizedJSONText = %s, want %s", name, got, testCase.want)
		}
	}
}
