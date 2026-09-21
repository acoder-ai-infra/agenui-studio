package renderercatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCurrentVerifiesPublishedCatalog(t *testing.T) {
	root := t.TempDir()
	content := []byte(`{"catalogId":"test://catalog"}`)
	path := filepath.Join(root, "1.0.0")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "catalog.json"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(content)
	encoded := hex.EncodeToString(hash[:])
	if err := os.WriteFile(filepath.Join(root, "current.json"), []byte(`{"catalog_version":"1.0.0","catalog_id":"test://catalog","catalog_sha256":"`+encoded+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.json"), []byte(`{"schema_version":"v1","catalogs":[{"catalog_version":"1.0.0","catalog_id":"test://catalog","catalog_sha256":"`+encoded+`","status":"published"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadCurrent(root)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != "1.0.0" || snapshot.CatalogID != "test://catalog" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
}

func TestLoadCurrentRejectsTamperedCatalog(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "1.0.0", "catalog.json"), []byte(`{"catalogId":"test://catalog"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "current.json"), []byte(`{"catalog_version":"1.0.0","catalog_id":"test://catalog","catalog_sha256":"bad"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.json"), []byte(`{"schema_version":"v1","catalogs":[{"catalog_version":"1.0.0","catalog_id":"test://catalog","catalog_sha256":"bad","status":"published"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCurrent(root); err == nil {
		t.Fatal("LoadCurrent() error = nil")
	}
}

func TestSnapshotJSONRejectsCatalogChangedAfterLoad(t *testing.T) {
	root := t.TempDir()
	content := []byte(`{"catalogId":"test://catalog"}`)
	path := filepath.Join(root, "1.0.0")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "catalog.json"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(content)
	encoded := hex.EncodeToString(hash[:])
	if err := os.WriteFile(filepath.Join(root, "current.json"), []byte(`{"catalog_version":"1.0.0","catalog_id":"test://catalog","catalog_sha256":"`+encoded+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.json"), []byte(`{"schema_version":"v1","catalogs":[{"catalog_version":"1.0.0","catalog_id":"test://catalog","catalog_sha256":"`+encoded+`","status":"published"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadCurrent(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "catalog.json"), []byte(`{"catalogId":"test://catalog","changed":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.JSON(); err == nil {
		t.Fatal("Snapshot.JSON() error = nil")
	}
}

func TestLoadCurrentFollowsPublishedPointerUpdate(t *testing.T) {
	root := t.TempDir()
	first := []byte(`{"catalogId":"test://catalog/one"}`)
	second := []byte(`{"catalogId":"test://catalog/two"}`)
	firstHash := sha256.Sum256(first)
	secondHash := sha256.Sum256(second)
	firstEncoded := hex.EncodeToString(firstHash[:])
	secondEncoded := hex.EncodeToString(secondHash[:])
	for _, item := range []struct {
		gate    string
		content []byte
	}{
		{gate: "1.0.0", content: first},
		{gate: "1.1.0", content: second},
	} {
		if err := os.Mkdir(filepath.Join(root, item.gate), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, item.gate, "catalog.json"), item.content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	index := `{"schema_version":"v1","catalogs":[` +
		`{"catalog_version":"1.0.0","catalog_id":"test://catalog/one","catalog_sha256":"` + firstEncoded + `","status":"published"},` +
		`{"catalog_version":"1.1.0","catalog_id":"test://catalog/two","catalog_sha256":"` + secondEncoded + `","status":"published"}]}`
	if err := os.WriteFile(filepath.Join(root, "index.json"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCurrent := func(gate, catalogID, hash string) {
		t.Helper()
		current := `{"catalog_version":"` + gate + `","catalog_id":"` + catalogID + `","catalog_sha256":"` + hash + `"}`
		if err := os.WriteFile(filepath.Join(root, "current.json"), []byte(current), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeCurrent("1.0.0", "test://catalog/one", firstEncoded)
	if snapshot, err := LoadCurrent(root); err != nil || snapshot.Version != "1.0.0" {
		t.Fatalf("first snapshot = %+v, %v", snapshot, err)
	}
	writeCurrent("1.1.0", "test://catalog/two", secondEncoded)
	if snapshot, err := LoadCurrent(root); err != nil || snapshot.Version != "1.1.0" || snapshot.CatalogID != "test://catalog/two" {
		t.Fatalf("updated snapshot = %+v, %v", snapshot, err)
	}
}

func TestAuthoringJSONContainsComponentSchemasButOmitsRuntimeFunctions(t *testing.T) {
	snapshot, err := LoadCurrent(filepath.Join("..", "..", "..", "configs", "renderer-catalogs"))
	if err != nil {
		t.Fatal(err)
	}
	view, err := snapshot.AuthoringJSON()
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(view, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["components"]; !ok {
		t.Fatal("authoring view omits components")
	}
	if _, ok := decoded["$defs"]; !ok {
		t.Fatal("authoring view omits shared definitions")
	}
	if _, ok := decoded["functions"]; ok {
		t.Fatal("authoring view must omit runtime functions")
	}
	var definitions map[string]json.RawMessage
	if err := json.Unmarshal(decoded["$defs"], &definitions); err != nil {
		t.Fatal(err)
	}
	if _, ok := definitions["anyFunction"]; ok {
		t.Fatal("authoring view must omit unreferenced definitions")
	}
	full, err := snapshot.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if len(view) >= len(full) {
		t.Fatalf("authoring view was not reduced: view=%d full=%d", len(view), len(full))
	}
}
