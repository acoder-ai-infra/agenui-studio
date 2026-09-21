package catalogadmission

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAnalyzeFindsCatalogVersionAndStampsCatalogID(t *testing.T) {
	payload := `[
  {"version":"v0.9","createSurface":{"surfaceId":"default","catalogId":"https://agenui.org/specification/v0_9/basic_catalog.json"}},
  {"version":"v0.9","updateComponents":{"surfaceId":"default","components":[{"id":"root","component":"Column","children":["title"]},{"id":"title","component":"Text","text":{"path":"/title"}}]}},
  {"version":"v0.9","updateDataModel":{"surfaceId":"default","path":"/","value":{"title":"test"}}}
]`
	result := Analyze(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"), payload)
	if !result.Compatible || result.CatalogVersion != "1.1.0" {
		t.Fatalf("Analyze() = %+v", result)
	}
	if result.Status != StatusSupported {
		t.Fatalf("status = %q", result.Status)
	}
	if result.Message != "该卡片可在当前 Renderer Catalog 1.1.0 中生效。" {
		t.Fatalf("success message = %q", result.Message)
	}
	stamped, err := StampCatalogID(payload, result.CatalogID)
	if err != nil || !strings.Contains(stamped, result.CatalogID) {
		t.Fatalf("StampCatalogID() = %q, %v", stamped, err)
	}
}

func TestAnalyzeReturnsChineseMessageForUnsupportedCard(t *testing.T) {
	payload := `[
  {"version":"v0.9","createSurface":{"surfaceId":"default","catalogId":"https://agenui.org/specification/v0_9/basic_catalog.json"}},
  {"version":"v0.9","updateComponents":{"surfaceId":"default","components":[{"id":"root","component":"UnsupportedComponent"}]}},
  {"version":"v0.9","updateDataModel":{"surfaceId":"default","path":"/","value":{}}}
]`
	result := Analyze(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"), payload)
	if result.Compatible || result.Message == "" || !strings.Contains(result.Message, "不支持") {
		t.Fatalf("Analyze() = %+v", result)
	}
	if result.Status != StatusUnsupported {
		t.Fatalf("status = %q", result.Status)
	}
}

func TestAnalyzeDoesNotTreatIncompleteButtonActionAsRendererMismatch(t *testing.T) {
	payload := `[
  {"version":"v0.9","createSurface":{"surfaceId":"default","catalogId":"https://agenui.org/specification/v0_9/basic_catalog.json"}},
  {"version":"v0.9","updateComponents":{"surfaceId":"default","components":[
    {"id":"root","component":"Column","children":["cta"]},
    {"id":"cta","component":"Button","child":"cta-text"},
    {"id":"cta-text","component":"Text","text":{"path":"/ctaText"}}
  ]}},
  {"version":"v0.9","updateDataModel":{"surfaceId":"default","path":"/","value":{"ctaText":"领券"}}}
]`
	result := Analyze(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"), payload)
	if !result.Compatible || result.Status != StatusSupported || result.CatalogVersion != "1.1.0" {
		t.Fatalf("Analyze() = %+v", result)
	}
	if strings.Contains(result.Message, "不支持") {
		t.Fatalf("missing action was reported as renderer mismatch: %+v", result)
	}
	if result.Message != "该卡片可在当前 Renderer Catalog 1.1.0 中生效。" {
		t.Fatalf("message = %q", result.Message)
	}
}

func TestAnalyzeDoesNotInspectActionBindingContents(t *testing.T) {
	payload := `[
  {"version":"v0.9","createSurface":{"surfaceId":"default","catalogId":"https://agenui.org/specification/v0_9/basic_catalog.json"}},
  {"version":"v0.9","updateComponents":{"surfaceId":"default","components":[
    {"id":"root","component":"Column","children":["cta"]},
    {"id":"cta","component":"Button","child":"cta-text","action":{"unbound":"not-a-valid-action"}},
    {"id":"cta-text","component":"Text","text":{"path":"/ctaText"}}
  ]}},
  {"version":"v0.9","updateDataModel":{"surfaceId":"default","path":"/","value":{"ctaText":"领券"}}}
]`
	result := Analyze(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"), payload)
	if !result.Compatible || result.Status != StatusSupported || result.CatalogVersion != "1.1.0" {
		t.Fatalf("Gate inspected action binding contents: %+v", result)
	}
}

func TestAnalyzeIgnoresComponentProperties(t *testing.T) {
	payload := `[
  {"version":"v0.9","createSurface":{"surfaceId":"default","catalogId":"https://agenui.org/specification/v0_9/basic_catalog.json"}},
  {"version":"v0.9","updateComponents":{"surfaceId":"default","components":[
    {"id":"root","component":"Column","children":["cta"]},
    {"id":"cta","component":"Button","child":"cta-text","futureRendererProperty":true},
    {"id":"cta-text","component":"Text","text":{"path":"/ctaText"}}
  ]}},
  {"version":"v0.9","updateDataModel":{"surfaceId":"default","path":"/","value":{"ctaText":"领券"}}}
]`
	result := Analyze(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"), payload)
	if !result.Compatible || result.Status != StatusSupported || result.CatalogVersion != "1.1.0" {
		t.Fatalf("Analyze() = %+v", result)
	}
}

func TestEditableStylePathsComeFromPublishedCatalog(t *testing.T) {
	paths, err := EditableStylePaths(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"styles.color", "styles.font-weight"} {
		found := false
		for _, path := range paths {
			if path == required {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("published catalog paths %v do not include %s", paths, required)
		}
	}
}
