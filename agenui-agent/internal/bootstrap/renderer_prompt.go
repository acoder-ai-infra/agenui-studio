package bootstrap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/agenui/renderercatalog"
)

// modelCatalogAlias stays fixed in the model prompt. Workspace commit stamps
// the selected catalog ID before delivery.
const modelCatalogAlias = "https://agenui.org/specification/v0_9/basic_catalog.json"

// materializeRendererPrompt renders one immutable catalog into the system prompt
// before Harness freezes it. The resulting config is process-local and is
// removed on App.Close; normal user messages are never modified.
func materializeRendererPrompt(harnessPath string, snapshot renderercatalog.Snapshot) (string, func(), error) {
	if harnessPath == "" {
		return "", nil, fmt.Errorf("renderer prompt: harness config path is required")
	}
	promptPath := filepath.Clean(filepath.Join(filepath.Dir(harnessPath), "../../harness/prompts.yaml"))
	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		return "", nil, err
	}
	catalog, err := rendererPromptCatalog(snapshot)
	if err != nil {
		return "", nil, err
	}
	rendered := strings.ReplaceAll(string(prompt), "{{renderer_catalog_id}}", modelCatalogAlias)
	rendered = strings.ReplaceAll(rendered, "{{renderer_catalog_components}}", catalog)
	dir, err := os.MkdirTemp("", "agenui-renderer-prompt-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	promptOut := filepath.Join(dir, "prompts.yaml")
	if err := os.WriteFile(promptOut, []byte(rendered), 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	harness, err := os.ReadFile(harnessPath)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	needle := "path: ../../harness/prompts.yaml"
	if !strings.Contains(string(harness), needle) {
		cleanup()
		return "", nil, fmt.Errorf("renderer prompt: prompts path not found in %s", harnessPath)
	}
	harness = []byte(strings.Replace(string(harness), needle, "path: "+promptOut, 1))
	// Keep the generated harness file beside its source. Other paths in the
	// harness configuration are relative to this directory and must retain
	// their original resolution base.
	harnessOut, err := os.CreateTemp(filepath.Dir(harnessPath), ".renderer-harness-*.yaml")
	if err != nil {
		cleanup()
		return "", nil, err
	}
	harnessOutPath := harnessOut.Name()
	cleanupPromptDir := cleanup
	cleanup = func() {
		_ = harnessOut.Close()
		_ = os.Remove(harnessOutPath)
		cleanupPromptDir()
	}
	if _, err := harnessOut.Write(harness); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := harnessOut.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return harnessOutPath, cleanup, nil
}

func rendererPromptCatalog(snapshot renderercatalog.Snapshot) (string, error) {
	content, err := snapshot.JSON()
	if err != nil {
		return "", err
	}
	// Reuse the frozen schema's protocol order without importing input package.
	var catalog struct {
		Definitions map[string]struct {
			OneOf []struct {
				Ref string `json:"$ref"`
			} `json:"oneOf"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(content, &catalog); err != nil {
		return "", err
	}
	var names []string
	for _, item := range catalog.Definitions["anyComponent"].OneOf {
		if strings.HasPrefix(item.Ref, "#/components/") {
			names = append(names, "`"+strings.TrimPrefix(item.Ref, "#/components/")+"`")
		}
	}
	compact, err := json.Marshal(json.RawMessage(content))
	if err != nil {
		return "", fmt.Errorf("encode renderer catalog: %w", err)
	}
	// This value replaces a placeholder inside a YAML block scalar. Keep it on
	// one physical line: unindented Markdown fences would terminate that scalar
	// and make the generated Harness configuration invalid.
	return strings.Join(names, "、") + "。完整协议如下，组件和属性必须以它为准：<renderer-catalog-json>" + string(compact) + "</renderer-catalog-json>", nil
}
