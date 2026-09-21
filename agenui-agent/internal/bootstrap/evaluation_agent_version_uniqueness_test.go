package bootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

// Agent registries treat (agent_id, version) as immutable globally. Evaluation
// lanes may repeat an existing version only when the Agent definition is
// semantically identical; a candidate model must always use a new version.
func TestAgentVersionsAreGloballyImmutableAcrossServingAndEvaluation(t *testing.T) {
	t.Parallel()
	root := filepath.Clean(filepath.Join("..", ".."))
	patterns := []string{
		filepath.Join(root, "configs", "harness", "agents", "*.yaml"),
		filepath.Join(root, "configs", "evaluation", "*", "agents", "*.yaml"),
	}
	var paths []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, matches...)
	}
	sort.Strings(paths)
	type registered struct {
		definition string
		path       string
	}
	seen := make(map[string]registered)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var definition map[string]any
		if err := yaml.Unmarshal(raw, &definition); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		agentID, _ := definition["agent_id"].(string)
		version, _ := definition["version"].(string)
		if agentID == "" || version == "" {
			continue
		}
		canonical, err := json.Marshal(definition)
		if err != nil {
			t.Fatalf("canonicalize %s: %v", path, err)
		}
		key := agentID + "@" + version
		if previous, ok := seen[key]; ok && previous.definition != string(canonical) {
			t.Errorf(
				"immutable Agent version %s differs between %s and %s",
				key,
				previous.path,
				path,
			)
			continue
		}
		seen[key] = registered{definition: string(canonical), path: path}
	}
}
