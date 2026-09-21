package diagnostics

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestErrorChainPreservesCauseAndRedactsSecrets(t *testing.T) {
	err := fmt.Errorf("dispatch child: %w", errors.New("gateway api_key=secret-value Authorization:Bearer-2 failed"))
	got := ErrorChain(err)
	for _, want := range []string{"dispatch child", "gateway", "<redacted>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("ErrorChain()=%q, missing %q", got, want)
		}
	}
	for _, secret := range []string{"secret-value", "Bearer-2"} {
		if strings.Contains(got, secret) {
			t.Fatalf("ErrorChain() leaked %q: %s", secret, got)
		}
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("ErrorChain() is not one line: %q", got)
	}
}

func TestSanitizeTextBoundsUntrustedDependencyErrors(t *testing.T) {
	got := SanitizeText(strings.Repeat("界", maxDiagnosticRunes+20))
	if count := len([]rune(got)); count != maxDiagnosticRunes {
		t.Fatalf("runes=%d want=%d", count, maxDiagnosticRunes)
	}
}
