package documentcontract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestContractSchemasCompile(t *testing.T) {
	entries, err := os.ReadDir("schemas")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("schemas", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var document any
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource(entry.Name(), document); err != nil {
			t.Fatalf("add %s: %v", entry.Name(), err)
		}
		if _, err := compiler.Compile(entry.Name()); err != nil {
			t.Fatalf("compile %s: %v", entry.Name(), err)
		}
	}
}
