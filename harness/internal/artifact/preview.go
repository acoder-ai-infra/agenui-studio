package artifact

import (
	"encoding/json"
	"fmt"
	"mime"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const defaultPreviewBytes = 1024

var sensitivePreviewKeys = map[string]struct{}{
	"authorization": {},
	"cookie":        {},
	"token":         {},
	"api_key":       {},
	"password":      {},
	"secret":        {},
	"mobile":        {},
	"phone":         {},
	"id_card":       {},
	"location":      {},
}

func buildPreview(visibility Visibility, mimeType, name, hash string, data []byte, hint PreviewHint) Preview {
	if visibility == VisibilityDebug {
		return Preview{}
	}
	maxBytes := effectivePreviewLimit(hint)
	classification := mimeType
	if parsed, _, err := mime.ParseMediaType(mimeType); err == nil {
		classification = parsed
	}
	switch {
	case strings.HasPrefix(classification, "text/"):
		return textPreview(data, maxBytes)
	case classification == "application/json" || strings.HasSuffix(classification, "+json"):
		return jsonPreview(data, maxBytes)
	case strings.HasPrefix(classification, "image/"):
		return binaryPreview("image", mimeType, name, hash, data, maxBytes)
	default:
		return binaryPreview("file", mimeType, name, hash, data, maxBytes)
	}
}

func textPreview(data []byte, maxBytes int) Preview {
	text := strings.ToValidUTF8(string(data), "\uFFFD")
	text = redactSensitiveAssignments(text)
	text, truncated := truncateUTF8Bytes(text, maxBytes)
	return Preview{Text: text, Truncated: truncated}
}

func effectivePreviewLimit(hint PreviewHint) int {
	if hint.MaxBytes <= 0 || hint.MaxBytes > defaultPreviewBytes {
		return defaultPreviewBytes
	}
	return hint.MaxBytes
}

func redactSensitiveAssignments(text string) string {
	var redacted strings.Builder
	for len(text) > 0 {
		newline := strings.IndexByte(text, '\n')
		if newline < 0 {
			redacted.WriteString(redactSensitiveLine(text))
			break
		}
		line := text[:newline]
		if strings.HasSuffix(line, "\r") {
			redacted.WriteString(redactSensitiveLine(strings.TrimSuffix(line, "\r")))
			redacted.WriteByte('\r')
		} else {
			redacted.WriteString(redactSensitiveLine(line))
		}
		redacted.WriteByte('\n')
		text = text[newline+1:]
	}
	return redacted.String()
}

func redactSensitiveLine(line string) string {
	for start := 0; start < len(line); {
		if !isAssignmentKeyByte(line[start]) || (start > 0 && isAssignmentKeyByte(line[start-1])) {
			start++
			continue
		}
		end := start
		for end < len(line) && isAssignmentKeyByte(line[end]) {
			end++
		}
		if _, sensitive := sensitivePreviewKeys[strings.ToLower(line[start:end])]; sensitive {
			delimiter := end
			if delimiter < len(line) && start > 0 &&
				(line[start-1] == '\'' || line[start-1] == '"') && line[delimiter] == line[start-1] {
				delimiter++
			}
			for delimiter < len(line) && (line[delimiter] == ' ' || line[delimiter] == '\t') {
				delimiter++
			}
			if delimiter < len(line) && (line[delimiter] == '=' || line[delimiter] == ':') {
				return line[:delimiter+1] + "***"
			}
		}
		start = end
	}
	return line
}

func isAssignmentKeyByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '_'
}

func truncateUTF8Bytes(text string, maxBytes int) (string, bool) {
	if len(text) <= maxBytes {
		return text, false
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return text[:end], true
}

func jsonPreview(data []byte, maxBytes int) Preview {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return textPreview(data, maxBytes)
	}
	obj, objectValue := value.(map[string]any)
	if !objectValue {
		text, truncated := boundedJSONText("JSON value", maxBytes)
		return Preview{Text: text, Fields: map[string]any{}, Truncated: truncated}
	}

	text, truncated := boundedJSONText(fmt.Sprintf("JSON object with %d keys", len(obj)), maxBytes)
	fields := make(map[string]any)
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		if !isSensitivePreviewKey(key) {
			continue
		}
		if !trySetJSONPreviewField(text, fields, key, "***", maxBytes) {
			truncated = true
		}
	}
	for _, key := range []string{"count", "source", "schema", "type"} {
		value, ok := obj[key]
		if !ok {
			continue
		}
		if !trySetJSONPreviewField(text, fields, key, cleanJSONPreviewValue(value), maxBytes) {
			truncated = true
		}
	}

	summary := make([]string, 0, len(keys))
	if !trySetJSONPreviewField(text, fields, "keys", summary, maxBytes) {
		truncated = true
	} else {
		for _, key := range keys {
			candidate := append(append([]string(nil), summary...), redactSensitiveAssignments(key))
			if !trySetJSONPreviewField(text, fields, "keys", candidate, maxBytes) {
				truncated = true
				break
			}
			summary = candidate
		}
	}
	return Preview{Text: text, Fields: fields, Truncated: truncated}
}

func boundedJSONText(text string, maxBytes int) (string, bool) {
	available := maxBytes - len([]byte("{}"))
	if available < 0 {
		available = 0
	}
	return truncateUTF8Bytes(text, available)
}

func trySetJSONPreviewField(text string, fields map[string]any, key string, value any, maxBytes int) bool {
	previous, existed := fields[key]
	fields[key] = value
	encoded, err := json.Marshal(fields)
	if err == nil && len([]byte(text))+len(encoded) <= maxBytes {
		return true
	}
	if existed {
		fields[key] = previous
	} else {
		delete(fields, key)
	}
	return false
}

func cleanJSONPreviewValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		cleaned := make(map[string]any, len(typed))
		for _, key := range keys {
			cleanedKey := redactSensitiveAssignments(key)
			if isSensitivePreviewKey(key) {
				cleaned[cleanedKey] = "***"
				continue
			}
			if _, exists := cleaned[cleanedKey]; !exists {
				cleaned[cleanedKey] = cleanJSONPreviewValue(typed[key])
			}
		}
		return cleaned
	case []any:
		cleaned := make([]any, len(typed))
		for index, item := range typed {
			cleaned[index] = cleanJSONPreviewValue(item)
		}
		return cleaned
	case string:
		return redactSensitiveAssignments(typed)
	default:
		return value
	}
}

func isSensitivePreviewKey(key string) bool {
	_, sensitive := sensitivePreviewKeys[strings.ToLower(key)]
	return sensitive
}

func nameOrMime(name, mimeType string) string {
	if name != "" {
		return name
	}
	return mimeType
}

func sanitizeArtifactName(name string) string {
	if name == "" {
		return ""
	}
	normalized := strings.ReplaceAll(strings.ToValidUTF8(name, "\uFFFD"), `\`, "/")
	base := path.Base(normalized)
	if base == "." || base == "/" {
		return ""
	}
	base = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, base)
	return redactSensitiveAssignments(base)
}

func binaryPreview(kind, mimeType, name, hash string, data []byte, maxBytes int) Preview {
	sanitizedName := sanitizeArtifactName(name)
	boundedName, truncated := truncateUTF8Bytes(sanitizedName, maxBytes/2)
	size := int64(len(data))
	return Preview{
		Text: fmt.Sprintf("%s %s, %d bytes", kind, nameOrMime(boundedName, mimeType), size),
		Fields: map[string]any{
			"name":      boundedName,
			"mime_type": mimeType,
			"size":      size,
			"hash":      hash,
		},
		Truncated: truncated,
	}
}
