package objectstore

import (
	"path/filepath"
	"testing"

	"github.com/AGenUI/agenui-studio/harness/internal/artifact"
)

func TestValidateObjectKeyAcceptsCanonicalRelativeKeys(t *testing.T) {
	keys := []string{
		"object",
		"tenants/tenant-a/sessions/session-a/artifact.bin",
		"unicode/对象.bin",
	}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			if err := validateObjectKey(key); err != nil {
				t.Fatalf("validateObjectKey(%q) error = %v, want nil", key, err)
			}
		})
	}
}

func TestValidateObjectKeyRejectsNonCanonicalKeys(t *testing.T) {
	platformAbsolute := filepath.Join(t.TempDir(), "object")

	tests := []struct {
		name string
		key  string
	}{
		{name: "empty", key: ""},
		{name: "absolute POSIX", key: "/absolute/object"},
		{name: "absolute platform", key: platformAbsolute},
		{name: "dot segment", key: "a/./object"},
		{name: "dot-dot segment", key: "a/../object"},
		{name: "cleaning changes duplicate slash", key: "a//object"},
		{name: "trailing slash", key: "a/"},
		{name: "backslash", key: `a\object`},
		{name: "NUL", key: "a/\x00/object"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateObjectKey(tc.key)
			if !artifact.IsErrorCode(err, artifact.ErrInvalidArgument) {
				t.Fatalf("validateObjectKey(%q) error = %v, want invalid_argument", tc.key, err)
			}
		})
	}
}
