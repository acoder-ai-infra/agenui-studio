package runtimeadapter

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/AGenUI/agenui-studio/harness/internal/identifiercontract"
)

func TestComposeToolStepIDIsBoundedStableAndOpaque(t *testing.T) {
	tests := []struct {
		name       string
		toolCallID string
	}{
		{name: "legacy boundary", toolCallID: strings.Repeat("a", 12)},
		{name: "previous overflow boundary", toolCallID: strings.Repeat("b", 13)},
		{name: "identifier boundary", toolCallID: strings.Repeat("c", 64)},
		{name: "provider opaque", toolCallID: "call_" + strings.Repeat("d", 251)},
		{name: "multi byte", toolCallID: strings.Repeat("工具", 128)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := composeToolStepID("run_1", tt.toolCallID)
			second := composeToolStepID("run_1", tt.toolCallID)
			if first != second {
				t.Fatalf("tool step id is not stable: %q != %q", first, second)
			}
			if !strings.HasPrefix(first, "tool_") || utf8.RuneCountInString(first) > identifiercontract.MaxStepIDCharacters {
				t.Fatalf("tool step id is not bounded: id=%q length=%d", first, utf8.RuneCountInString(first))
			}
			if strings.Contains(first, tt.toolCallID) || strings.Contains(first, ":tool:") {
				t.Fatalf("tool step id exposed compositional input: %q", first)
			}
		})
	}

	base := composeToolStepID("run_1", "call_1")
	if base == composeToolStepID("run_2", "call_1") {
		t.Fatal("different runs produced the same tool step id")
	}
	if base == composeToolStepID("run_1", "call_2") {
		t.Fatal("different tool calls produced the same tool step id")
	}
}

func TestValidateResolvedStepIdentityHonorsLedgerBoundary(t *testing.T) {
	if err := validateResolvedStepIdentity(strings.Repeat("s", 64), strings.Repeat("p", 64)); err != nil {
		t.Fatalf("64-character step identity rejected: %v", err)
	}
	if err := validateResolvedStepIdentity(strings.Repeat("s", 65), "parent"); err == nil {
		t.Fatal("65-character step id was accepted")
	}
	if err := validateResolvedStepIdentity("step", strings.Repeat("p", 65)); err == nil {
		t.Fatal("65-character parent step id was accepted")
	}
}
