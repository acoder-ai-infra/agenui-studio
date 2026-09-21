package runtime

import (
	"context"
	"os"
	"testing"
)

func TestNegativePackagesReturnExpectedCodes(t *testing.T) {
	t.Run("wildcard scalar shape", func(t *testing.T) {
		pkg := loadTestPackage(t, "testdata/negative/01-wildcard-scalar-shape.json")
		response := mustJSON(t, `{"items":[{"price_cents":6800},{"price_cents":12800}]}`)
		_, err := NewEngine(&fakeFetcher{responses: map[string]any{"ds-1": response}}, nil).Execute(context.Background(), pkg, nil)
		requireErrorCode(t, err, CodeOperatorInputValidationFailed)
	})
	t.Run("three wildcards", func(t *testing.T) {
		raw, err := os.ReadFile("testdata/negative/02-three-wildcards.json")
		if err != nil {
			t.Fatal(err)
		}
		_, err = Load(raw)
		requireErrorCode(t, err, CodeBindingWildcardMismatch)
	})
	t.Run("fixed index", func(t *testing.T) {
		pkg := loadTestPackage(t, "testdata/negative/03-fixed-index-out-of-range.json")
		response := mustJSON(t, `{"items":[{"price_cents":6800}]}`)
		_, err := NewEngine(&fakeFetcher{responses: map[string]any{"ds-1": response}}, nil).Execute(context.Background(), pkg, nil)
		requireErrorCode(t, err, CodeFixedIndexOutOfRange)
	})
}

func loadTestPackage(t *testing.T, path string) *Package {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}
