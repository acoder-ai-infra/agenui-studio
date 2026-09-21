package bootstrap

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/AGenUI/agenui-studio/agenui-agent/pkg/agenui/renderercatalog"
)

func TestRendererPromptCatalogEmbedsVerifiedProtocolJSON(t *testing.T) {
	snapshot, err := renderercatalog.LoadCurrent(filepath.Join("..", "..", "configs", "renderer-catalogs"))
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := rendererPromptCatalog(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "`Button`") || !strings.Contains(prompt, `"catalogId":"`+snapshot.CatalogID+`"`) {
		t.Fatalf("prompt does not contain the selected catalog: %q", prompt)
	}
	if !strings.Contains(prompt, `"components"`) || !strings.Contains(prompt, `"$defs"`) {
		t.Fatal("prompt must contain the full protocol catalog, not only component names")
	}
	if strings.Contains(prompt, "\n```json") || !strings.Contains(prompt, "<renderer-catalog-json>") {
		t.Fatal("catalog prompt must remain a single YAML-safe line")
	}
}
