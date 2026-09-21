package agentregistry

import (
	"bytes"
	"testing"
)

func TestIdentityTupleEncodingDoesNotDependOnInjectableSeparators(t *testing.T) {
	left := []string{"a\x1fb", "c"}
	right := []string{"a", "b\x1fc"}
	if bytes.Equal([]byte(identityTupleKey(left...)), []byte(identityTupleKey(right...))) {
		t.Fatal("different identity tuples produced the same canonical key")
	}
	if bytes.Equal(identityTupleDigest(left...), identityTupleDigest(right...)) {
		t.Fatal("different identity tuples produced the same digest")
	}
	if got := len(identityTupleDigest("agent", "v1")); got != 32 {
		t.Fatalf("identity digest length=%d, want 32", got)
	}
}
