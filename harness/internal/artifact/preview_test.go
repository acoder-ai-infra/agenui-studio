package artifact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTextPreviewRedactsSensitiveAssignments(t *testing.T) {
	tests := []string{
		"token=secret-value end",
		"Authorization: Bearer private-token",
		`TOKEN="private value"`,
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			preview := buildPreview(VisibilityUserVisible, "text/plain", "report.txt", "sha256:x", []byte(raw), PreviewHint{MaxBytes: 64})
			if !strings.Contains(preview.Text, "***") {
				t.Fatalf("preview = %#v, want redaction marker", preview)
			}
			for _, forbidden := range []string{"secret-value", "Bearer", "private-token", "private value"} {
				if strings.Contains(preview.Text, forbidden) {
					t.Fatalf("raw = %q, preview leaked %q: %#v", raw, forbidden, preview)
				}
			}
		})
	}
}

func TestTextPreviewRedactsEveryDocumentedSensitiveKeyCaseFolded(t *testing.T) {
	for _, key := range []string{
		"authorization", "cookie", "token", "api_key", "password",
		"secret", "mobile", "phone", "id_card", "location",
	} {
		t.Run(key, func(t *testing.T) {
			raw := "prefix\n" + strings.ToUpper(key) + "=private value\nsuffix"
			preview := buildPreview(VisibilityInternal, "text/plain", "", "sha256:x", []byte(raw), PreviewHint{})
			if strings.Contains(preview.Text, "private") || !strings.Contains(preview.Text, "***") {
				t.Fatalf("key = %q, preview = %#v", key, preview)
			}
			if !strings.Contains(preview.Text, "prefix\n") || !strings.HasSuffix(preview.Text, "\nsuffix") {
				t.Fatalf("redaction crossed logical line for key %q: %#v", key, preview)
			}
		})
	}
}

func TestTextPreviewDoesNotInventSensitiveKeyAliases(t *testing.T) {
	raw := "credential=visible\npasswd=visible"
	preview := buildPreview(VisibilityInternal, "text/plain", "", "sha256:x", []byte(raw), PreviewHint{})
	if preview.Text != raw {
		t.Fatalf("preview = %q, want unchanged undocumented aliases %q", preview.Text, raw)
	}
}

func TestTextPreviewTruncatesAtValidUTF8Boundary(t *testing.T) {
	preview := buildPreview(VisibilityUserVisible, "text/plain", "report.txt", "sha256:x", []byte("手机🙂导航内容"), PreviewHint{MaxBytes: 9})
	if preview.Text != "手机" || !utf8.ValidString(preview.Text) || !preview.Truncated {
		t.Fatalf("preview = %#v, want valid rune-boundary truncation", preview)
	}
	if len([]byte(preview.Text)) > 9 {
		t.Fatalf("preview bytes = %d, want <= 9", len([]byte(preview.Text)))
	}
}

func TestTextPreviewRepairsInvalidUTF8WithinBudget(t *testing.T) {
	raw := []byte{'a', 0xff, 'b', 0xfe, 'c'}
	preview := buildPreview(VisibilityInternal, "text/plain", "", "sha256:x", raw, PreviewHint{MaxBytes: 8})
	if !utf8.ValidString(preview.Text) {
		t.Fatalf("preview contains invalid UTF-8: %q", preview.Text)
	}
	if len([]byte(preview.Text)) > 8 {
		t.Fatalf("preview bytes = %d, want <= 8", len([]byte(preview.Text)))
	}
	if bytes.Contains([]byte(preview.Text), []byte{0xff}) || bytes.Contains([]byte(preview.Text), []byte{0xfe}) {
		t.Fatalf("preview retained invalid bytes: %q", preview.Text)
	}
}

func TestTextPreviewUsesDefaultAndNarrowLimits(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 2048)
	tests := []struct {
		name string
		hint PreviewHint
		want int
	}{
		{name: "zero defaults", hint: PreviewHint{}, want: defaultPreviewBytes},
		{name: "negative defaults", hint: PreviewHint{MaxBytes: -1}, want: defaultPreviewBytes},
		{name: "positive narrows", hint: PreviewHint{MaxBytes: 17}, want: 17},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			preview := buildPreview(VisibilityInternal, "text/plain", "", "sha256:x", data, tc.hint)
			if len([]byte(preview.Text)) != tc.want || !preview.Truncated {
				t.Fatalf("preview = %#v, bytes = %d, want %d", preview, len([]byte(preview.Text)), tc.want)
			}
		})
	}
}

func TestPreviewHintCannotExpandPastP0SafetyLimit(t *testing.T) {
	preview := buildPreview(VisibilityUserVisible, "text/plain", "", "sha256:x", bytes.Repeat([]byte("x"), 2048), PreviewHint{MaxBytes: 4096})
	if len([]byte(preview.Text)) != defaultPreviewBytes || !preview.Truncated {
		t.Fatalf("preview = %#v, bytes = %d, want %d", preview, len([]byte(preview.Text)), defaultPreviewBytes)
	}
}

func TestJSONPreviewIsDeterministicBoundedAndRedactsSensitiveKeys(t *testing.T) {
	fields := make([]string, 0, 80)
	for i := 0; i < 80; i++ {
		fields = append(fields, fmt.Sprintf(`"key_%02d":"value"`, i))
	}
	forward := []byte(`{"count":2,"token":"secret","source":"kb",` + strings.Join(fields, ",") + `}`)
	reversedFields := append([]string(nil), fields...)
	for left, right := 0, len(reversedFields)-1; left < right; left, right = left+1, right-1 {
		reversedFields[left], reversedFields[right] = reversedFields[right], reversedFields[left]
	}
	reversed := []byte(`{"source":"kb",` + strings.Join(reversedFields, ",") + `,"token":"secret","count":2}`)

	first := buildPreview(VisibilityInternal, "application/json", "", "sha256:x", forward, PreviewHint{MaxBytes: 64})
	second := buildPreview(VisibilityInternal, "application/json", "", "sha256:x", reversed, PreviewHint{MaxBytes: 64})
	firstFields, err := json.Marshal(first.Fields)
	if err != nil {
		t.Fatalf("marshal first fields: %v", err)
	}
	secondFields, err := json.Marshal(second.Fields)
	if err != nil {
		t.Fatalf("marshal second fields: %v", err)
	}
	derivedBytes := len([]byte(first.Text)) + len(firstFields)
	if derivedBytes > 64 {
		t.Fatalf("derived preview bytes = %d, want <= 64: %#v", derivedBytes, first)
	}
	if first.Fields["token"] != "***" || first.Fields["count"] != float64(2) || !first.Truncated {
		t.Fatalf("first preview = %#v", first)
	}
	if bytes.Contains(firstFields, []byte("secret")) {
		t.Fatalf("preview leaked secret: %s", firstFields)
	}
	if first.Text != second.Text || !bytes.Equal(firstFields, secondFields) || first.Truncated != second.Truncated {
		t.Fatalf("preview is not deterministic:\nfirst=%#v\nsecond=%#v", first, second)
	}
}

func TestJSONPreviewSortsTopLevelKeySummary(t *testing.T) {
	preview := buildPreview(VisibilityInternal, "application/json", "", "sha256:x", []byte(`{"z":1,"a":2,"m":3}`), PreviewHint{})
	keys, ok := preview.Fields["keys"].([]string)
	if !ok || !reflect.DeepEqual(keys, []string{"a", "m", "z"}) {
		t.Fatalf("keys = %#v, want sorted key summary", preview.Fields["keys"])
	}
}

func TestEveryDocumentedSensitiveKeyIsRedactedInJSON(t *testing.T) {
	for _, key := range []string{
		"authorization", "cookie", "token", "api_key", "password",
		"secret", "mobile", "phone", "id_card", "location",
	} {
		t.Run(key, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{"%s":"private"}`, key))
			preview := buildPreview(VisibilityInternal, "application/json", "", "sha256:x", raw, PreviewHint{})
			if preview.Fields[key] != "***" {
				t.Fatalf("key = %q, preview = %#v", key, preview)
			}
			encoded, err := json.Marshal(preview)
			if err != nil {
				t.Fatalf("marshal preview: %v", err)
			}
			if bytes.Contains(encoded, []byte("private")) {
				t.Fatalf("key = %q leaked value: %s", key, encoded)
			}
		})
	}
}

func TestJSONPreviewRedactsMixedCaseAndNestedSensitiveKeys(t *testing.T) {
	raw := []byte(`{"Authorization":"Bearer private","source":{"Token":"nested-private","kind":"kb","items":[{"PASSWORD":"deep-private","safe":true}]}}`)
	preview := buildPreview(VisibilityInternal, "application/json", "", "sha256:x", raw, PreviewHint{})
	encoded, err := json.Marshal(preview)
	if err != nil {
		t.Fatalf("marshal preview: %v", err)
	}
	for _, forbidden := range []string{"Bearer private", "nested-private", "deep-private"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("preview leaked %q: %s", forbidden, encoded)
		}
	}
	source, ok := preview.Fields["source"].(map[string]any)
	if !ok || source["Token"] != "***" || source["kind"] != "kb" {
		t.Fatalf("preview = %#v", preview)
	}
	items, ok := source["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("source items = %#v", source["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok || item["PASSWORD"] != "***" || item["safe"] != true {
		t.Fatalf("nested item = %#v", items[0])
	}
}

func TestJSONPreviewRedactsAssignmentsInKeySummaryAndRetainedStrings(t *testing.T) {
	raw := []byte(`{"token=key-name-secret":1,"source":{"kind":"token=value-secret","safe":"visible"}}`)
	preview := buildPreview(VisibilityInternal, "application/json", "", "sha256:x", raw, PreviewHint{})
	encoded, err := json.Marshal(preview)
	if err != nil {
		t.Fatalf("marshal preview: %v", err)
	}
	for _, forbidden := range []string{"key-name-secret", "value-secret"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("preview leaked %q: %s", forbidden, encoded)
		}
	}
	if !bytes.Contains(encoded, []byte("***")) || !bytes.Contains(encoded, []byte("visible")) {
		t.Fatalf("preview lost marker or safe value: %s", encoded)
	}
}

func TestJSONPreviewOmitsOversizedRetainedValueWithinBudget(t *testing.T) {
	raw := []byte(`{"source":{"safe":"` + strings.Repeat("x", 256) + `"},"count":2}`)
	preview := buildPreview(VisibilityInternal, "application/json", "", "sha256:x", raw, PreviewHint{MaxBytes: 40})
	encodedFields, err := json.Marshal(preview.Fields)
	if err != nil {
		t.Fatalf("marshal fields: %v", err)
	}
	if derived := len([]byte(preview.Text)) + len(encodedFields); derived > 40 {
		t.Fatalf("derived preview bytes = %d, want <= 40: %#v", derived, preview)
	}
	if !preview.Truncated || bytes.Contains(encodedFields, bytes.Repeat([]byte("x"), 32)) {
		t.Fatalf("preview = %#v, fields = %s", preview, encodedFields)
	}
}

func TestMalformedJSONFallsBackToBoundedRedactedText(t *testing.T) {
	preview := buildPreview(VisibilityInternal, "application/json", "", "sha256:x", []byte(`{"token":"secret"`), PreviewHint{MaxBytes: 16})
	if strings.Contains(preview.Text, "secret") || !strings.Contains(preview.Text, "***") || len([]byte(preview.Text)) > 16 {
		t.Fatalf("preview = %#v", preview)
	}
}

func TestParameterizedAndSuffixJSONMIMEUseJSONPreview(t *testing.T) {
	for _, mimeType := range []string{"application/json; charset=utf-8", "application/problem+json"} {
		t.Run(mimeType, func(t *testing.T) {
			preview := buildPreview(VisibilityInternal, mimeType, "", "sha256:x", []byte(`{"token":"private"}`), PreviewHint{})
			if preview.Fields["token"] != "***" {
				t.Fatalf("mime = %q, preview = %#v", mimeType, preview)
			}
		})
	}
}

func TestDebugPreviewIsEmpty(t *testing.T) {
	for _, mimeType := range []string{"text/plain", "application/json", "application/octet-stream", "image/png"} {
		t.Run(mimeType, func(t *testing.T) {
			preview := buildPreview(VisibilityDebug, mimeType, `C:\\private\\token=secret.bin`, "sha256:secret", []byte(`token=secret`), PreviewHint{})
			if !reflect.DeepEqual(preview, Preview{}) {
				t.Fatalf("debug preview = %#v, want zero Preview", preview)
			}
		})
	}
}

func TestSanitizeArtifactNameKeepsOnlyCrossPlatformBasename(t *testing.T) {
	for _, raw := range []string{
		`C:\private\report.txt`,
		`/var/private/report.txt`,
		`mixed\private/path/report.txt`,
	} {
		t.Run(raw, func(t *testing.T) {
			if got := sanitizeArtifactName(raw); got != "report.txt" {
				t.Fatalf("sanitizeArtifactName(%q) = %q, want report.txt", raw, got)
			}
		})
	}
}

func TestSanitizeArtifactNameRemovesUnicodeControlsAndInvalidUTF8(t *testing.T) {
	raw := "private/path/re\x00po\nrt\t\u0085" + string([]byte{0xff}) + ".txt"
	got := sanitizeArtifactName(raw)
	if !utf8.ValidString(got) || strings.ContainsAny(got, "\x00\n\t\u0085") || strings.Contains(got, "private") {
		t.Fatalf("sanitizeArtifactName(%q) = %q", raw, got)
	}
	if got != "report�.txt" {
		t.Fatalf("sanitized name = %q, want %q", got, "report�.txt")
	}
}

func TestSanitizeArtifactNameRedactsEverySensitiveAssignment(t *testing.T) {
	for _, key := range []string{
		"authorization", "cookie", "token", "api_key", "password",
		"secret", "mobile", "phone", "id_card", "location",
	} {
		t.Run(key, func(t *testing.T) {
			raw := `C:\private\` + strings.ToUpper(key) + `=private value.txt`
			got := sanitizeArtifactName(raw)
			if got != strings.ToUpper(key)+"=***" || strings.Contains(got, "private") || strings.ContainsAny(got, `/\`) {
				t.Fatalf("sanitizeArtifactName(%q) = %q", raw, got)
			}
		})
	}
}

func TestFileAndImagePreviewUseSanitizedBasenameAndRequiredFields(t *testing.T) {
	tests := []struct {
		name     string
		mimeType string
	}{
		{name: "file", mimeType: "application/octet-stream; profile=test"},
		{name: "image", mimeType: "image/png; variant=test"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			preview := buildPreview(VisibilityUserVisible, tc.mimeType, `C:\\private\\TOKEN=abc.txt`, "sha256:abc", []byte("x"), PreviewHint{})
			name, ok := preview.Fields["name"].(string)
			if !ok || name != "TOKEN=***" {
				t.Fatalf("preview name = %#v, want TOKEN=***", preview.Fields["name"])
			}
			if !strings.Contains(preview.Text, name) || strings.Contains(preview.Text, "private") || strings.Contains(preview.Text, "abc.txt") {
				t.Fatalf("preview text = %q", preview.Text)
			}
			if preview.Fields["mime_type"] != tc.mimeType || preview.Fields["hash"] != "sha256:abc" {
				t.Fatalf("preview required fields = %#v", preview.Fields)
			}
			size, ok := preview.Fields["size"].(int64)
			if !ok || size != 1 {
				t.Fatalf("preview size = %#v, want int64(1)", preview.Fields["size"])
			}
			if _, exists := preview.Fields["thumbnail_ref"]; exists {
				t.Fatalf("P0 preview advertised thumbnail_ref: %#v", preview.Fields)
			}
		})
	}
}

func TestLongFilePreviewNameSharesContentBudgetAcrossTextAndFields(t *testing.T) {
	preview := buildPreview(VisibilityUserVisible, "application/octet-stream", strings.Repeat("名", 100), "sha256:abc", []byte("x"), PreviewHint{MaxBytes: 16})
	name, ok := preview.Fields["name"].(string)
	if !ok || name == "" || !utf8.ValidString(name) || !strings.Contains(preview.Text, name) {
		t.Fatalf("preview = %#v", preview)
	}
	derivedNameBytes := len([]byte(name)) * 2
	if derivedNameBytes > 16 || !preview.Truncated {
		t.Fatalf("preview = %#v, derived name bytes = %d, want <= 16 and truncated", preview, derivedNameBytes)
	}
	if preview.Fields["mime_type"] != "application/octet-stream" || preview.Fields["size"] != int64(1) || preview.Fields["hash"] != "sha256:abc" {
		t.Fatalf("budget removed required fields: %#v", preview.Fields)
	}
}

func TestTextPreviewDoesNotGainFileNameField(t *testing.T) {
	preview := buildPreview(VisibilityUserVisible, "text/plain", "report.txt", "sha256:abc", []byte("safe"), PreviewHint{})
	if _, exists := preview.Fields["name"]; exists {
		t.Fatalf("text preview gained file name contract: %#v", preview)
	}
}
