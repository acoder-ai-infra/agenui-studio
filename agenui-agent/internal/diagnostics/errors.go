package diagnostics

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	maxErrorChainDepth = 12
	maxDiagnosticRunes = 4096
)

var (
	bearerSecret = regexp.MustCompile(`(?i)\bbearer\s+[^\s,;]+`)
	namedSecret  = regexp.MustCompile(`(?i)\b(api[-_]?key|authorization|token|secret|password)\b(\s*[=:]\s*)([^\s,;}&]+)`)
)

// ErrorChain preserves the in-process causal chain before SDK event
// serialization intentionally drops private causes. It bounds and redacts the
// result so diagnostics never become a second payload or secret store.
func ErrorChain(err error) string {
	if err == nil {
		return ""
	}
	values := make([]string, 0, 4)
	collect(err, 0, &values)
	return SanitizeText(strings.Join(values, " <- "))
}

func collect(err error, depth int, values *[]string) {
	if err == nil || depth >= maxErrorChainDepth {
		return
	}
	*values = append(*values, fmt.Sprintf("%T: %s", err, err.Error()))
	var many interface{ Unwrap() []error }
	if errors.As(err, &many) {
		for _, child := range many.Unwrap() {
			collect(child, depth+1, values)
		}
		return
	}
	collect(errors.Unwrap(err), depth+1, values)
}

// SanitizeText makes arbitrary dependency errors safe for one-line logs.
func SanitizeText(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	value = bearerSecret.ReplaceAllString(value, "Bearer <redacted>")
	value = namedSecret.ReplaceAllString(value, `$1$2<redacted>`)
	if utf8.RuneCountInString(value) <= maxDiagnosticRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxDiagnosticRunes-1]) + "…"
}
