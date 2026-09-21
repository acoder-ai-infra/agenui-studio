package runtime

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLoadAndPackageLookups(t *testing.T) {
	pkg := validPackage(t)
	raw, err := json.Marshal(pkg)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(raw)
	if err != nil || loaded.CardID != pkg.CardID {
		t.Fatalf("loaded=%#v error=%v", loaded, err)
	}
	if _, err := Load([]byte(`{`)); err == nil {
		t.Fatal("invalid JSON loaded")
	}
	pkg.Version = "bad"
	invalid, _ := json.Marshal(pkg)
	if _, err := Load(invalid); err == nil {
		t.Fatal("invalid package loaded")
	}
	if op, ok := pkg.FindOperator(101); !ok || op.OperatorKey == "" {
		t.Fatalf("operator=%#v found=%v", op, ok)
	}
	if _, ok := pkg.FindOperator(999); ok {
		t.Fatal("unknown operator found")
	}
	if source, ok := pkg.FindDataSource("ds-products"); !ok || source.Role != RolePrimary {
		t.Fatalf("source=%#v found=%v", source, ok)
	}
	if _, ok := pkg.FindDataSource("missing"); ok {
		t.Fatal("unknown source found")
	}
	if source, ok := pkg.PrimaryDataSource(); !ok || source.ID != "ds-products" {
		t.Fatalf("primary=%#v found=%v", source, ok)
	}
	pkg.DataSources[0].Role = RoleSupplement
	if _, ok := pkg.PrimaryDataSource(); ok {
		t.Fatal("unexpected primary")
	}
	if normalizedParams(nil) == nil || normalizedParams(map[string]any{"x": 1})["x"] != 1 {
		t.Fatal("normalizedParams")
	}
}

func TestPackageValidateTopLevelAndSources(t *testing.T) {
	var nilPackage *Package
	if err := nilPackage.Validate(); err == nil {
		t.Fatal("nil package validated")
	}
	for _, test := range []struct {
		name   string
		mutate func(*Package)
	}{
		{"version", func(p *Package) { p.Version = "1.0" }},
		{"card id", func(p *Package) { p.CardID = "" }},
		{"protocol", func(p *Package) { p.Protocol = nil }},
		{"source id", func(p *Package) { p.DataSources[0].ID = "" }},
		{"source endpoint", func(p *Package) { p.DataSources[0].Endpoint = "" }},
		{"duplicate source", func(p *Package) { p.DataSources[1].ID = p.DataSources[0].ID }},
		{"invalid role", func(p *Package) { p.DataSources[1].Role = "other" }},
		{"items path syntax", func(p *Package) { p.DataSources[0].ItemsPath = "$bad" }},
		{"items path wildcard", func(p *Package) { p.DataSources[0].ItemsPath = "$.items[*]" }},
		{"no primary", func(p *Package) { p.DataSources[0].Role = RoleSupplement }},
		{"two primaries", func(p *Package) { p.DataSources[1].Role = RolePrimary }},
		{"meta contract", func(p *Package) { p.Meta.ContractHash = "bad" }},
		{"meta design", func(p *Package) { p.Meta.DesignHash = "bad" }},
		{"meta requirements", func(p *Package) { p.Meta.RequirementsHash = "bad" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := validPackage(t)
			test.mutate(pkg)
			if err := pkg.Validate(); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestPackageValidateOperators(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Operator)
		code   string
	}{
		{"id", func(op *Operator) { op.OperatorVersionID = 0 }, CodeOperatorNotPublished},
		{"key", func(op *Operator) { op.OperatorKey = "" }, CodeOperatorNotPublished},
		{"version", func(op *Operator) { op.Version = 0 }, CodeOperatorNotPublished},
		{"source", func(op *Operator) { op.SourceCode = "" }, CodeOperatorNotPublished},
		{"entry", func(op *Operator) { op.Entry = "" }, CodeOperatorNotPublished},
		{"hash form", func(op *Operator) { op.SourceHash = "bad" }, CodeOperatorNotPublished},
		{"hash mismatch", func(op *Operator) { op.SourceHash = "sha256:" + strings.Repeat("0", 64) }, CodeOperatorSourceHashMismatch},
		{"language", func(op *Operator) { op.Language = "python" }, CodeOperatorNotPublished},
		{"input schema", func(op *Operator) { op.InputSchema = schema(`{"type":"bad"}`) }, ""},
		{"params schema", func(op *Operator) { op.ParamsSchema = schema(`{`) }, ""},
		{"output schema", func(op *Operator) { op.OutputSchema = nil }, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := validPackage(t)
			test.mutate(&pkg.Operators[0])
			err := pkg.Validate()
			if err == nil {
				t.Fatal("expected error")
			}
			if test.code != "" {
				requireErrorCode(t, err, test.code)
			}
		})
	}
	t.Run("duplicate", func(t *testing.T) {
		pkg := validPackage(t)
		pkg.Operators = append(pkg.Operators, pkg.Operators[0])
		if err := pkg.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestPackageValidateBindings(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Package)
		code   string
	}{
		{"slot", func(p *Package) { p.Bindings[0].SlotID = "" }, ""},
		{"field", func(p *Package) { p.Bindings[0].FieldPath = "" }, ""},
		{"ref", func(p *Package) { p.Bindings[0].RefKey = "" }, ""},
		{"source", func(p *Package) { p.Bindings[0].DataSourceID = "missing" }, ""},
		{"field syntax", func(p *Package) { p.Bindings[0].FieldPath = "$bad" }, CodeBindingSourcePathInvalid},
		{"ref syntax", func(p *Package) { p.Bindings[0].RefKey = "bad" }, CodeBindingTargetPathInvalid},
		{"field depth", func(p *Package) {
			p.Bindings[0].FieldPath = "$.a[*].b[*].c[*]"
			p.Bindings[0].RefKey = "/a[*]/b[*]/c[*]"
		}, CodeBindingWildcardMismatch},
		{"ref depth", func(p *Package) { p.Bindings[0].RefKey = "/a[*]/b[*]/c[*]" }, CodeBindingWildcardMismatch},
		{"coordinate mismatch", func(p *Package) { p.Bindings[0].RefKey = "/items[*]/nested[*]/name" }, CodeBindingWildcardMismatch},
		{"policy", func(p *Package) { p.Bindings[0].MissingPolicy = "other" }, ""},
		{"operator", func(p *Package) { p.Bindings[0].Transforms = []Invocation{{OperatorVersionID: 999}} }, CodeOperatorNotPublished},
		{"params", func(p *Package) { p.Bindings[1].Transforms[0].Params["extra"] = true }, CodeOperatorParamsValidationFailed},
		{"supplement without coordinate target", func(p *Package) { p.Bindings[3].RefKey = "/rating" }, CodeSourceJoinNotProven},
		{"supplement key mismatch", func(p *Package) { p.DataSources[1].EntityKey = "other" }, CodeSourceJoinNotProven},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := validPackage(t)
			test.mutate(pkg)
			err := pkg.Validate()
			if err == nil {
				t.Fatal("expected error")
			}
			if test.code != "" {
				requireErrorCode(t, err, test.code)
			}
		})
	}
	// A source wildcard may legally collapse to one target through a list operator.
	pkg := validPackage(t)
	pkg.Bindings[0].RefKey = "/names"
	if err := pkg.Validate(); err != nil {
		t.Fatalf("aggregate binding rejected: %v", err)
	}
}

func TestPackageValidateActions(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Action)
	}{
		{"id", func(a *Action) { a.ID = "" }},
		{"type", func(a *Action) { a.Type = "" }},
		{"payload empty", func(a *Action) { a.Payload = nil }},
		{"payload invalid", func(a *Action) { a.Payload = json.RawMessage(`{`) }},
		{"payload source", func(a *Action) { a.Payload = schema(`{"dataSourceId":"missing","path":"$.x","componentId":"c"}`) }},
		{"payload path", func(a *Action) { a.Payload = schema(`{"dataSourceId":"ds-products","path":"","componentId":"c"}`) }},
		{"payload component", func(a *Action) { a.Payload = schema(`{"dataSourceId":"ds-products","path":"$.x","componentId":""}`) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := validPackage(t)
			test.mutate(&pkg.Actions[0])
			if err := pkg.Validate(); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestActionJSONContractIsUnchanged(t *testing.T) {
	action := Action{ID: "open", Name: "Open", Type: "url", Payload: schema(`{"dataSourceId":"ds-1","path":"$.url","componentId":"button"}`)}
	raw, err := json.Marshal(action)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if json.Unmarshal(raw, &decoded) != nil || decoded["id"] != "open" || decoded["name"] != "Open" || decoded["type"] != "url" {
		t.Fatalf("action=%s", raw)
	}
	payload := decoded["payload"].(map[string]any)
	if payload["dataSourceId"] != "ds-1" || payload["path"] != "$.url" || payload["componentId"] != "button" {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestContentHashValidation(t *testing.T) {
	valid := "sha256:" + strings.Repeat("a", 64)
	if !validContentHash(valid) || validContentHash("sha256:"+strings.Repeat("A", 64)) ||
		validContentHash("sha256:short") || validContentHash("md5:"+strings.Repeat("a", 64)) {
		t.Fatal("content hash validation mismatch")
	}
}
