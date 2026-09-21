package mysql

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestBackendDoesNotConstructDatabaseConnections(t *testing.T) {
	violations, err := connectionBoundaryViolations(".")
	if err != nil {
		t.Fatalf("scan production mysql package: %v", err)
	}
	for _, violation := range violations {
		t.Error(violation)
	}
}

func connectionBoundaryViolations(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read package directory: %w", err)
	}

	fileSet := token.NewFileSet()
	var productionFiles []*ast.File
	var violations []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fileSet, filepath.Join(directory, name), nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		productionFiles = append(productionFiles, file)

		for _, imported := range file.Imports {
			importPath, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return nil, fmt.Errorf("parse import in %s: %w", name, err)
			}
			if importPath != "database/sql" {
				continue
			}
			if imported.Name != nil && imported.Name.Name == "." {
				violations = append(violations, connectionBoundaryViolation(fileSet, imported.Pos(), "dot import database/sql"))
			}
		}

		for _, declaration := range file.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				if declaration.Recv == nil && declaration.Name.Name == "Open" {
					violations = append(violations, connectionBoundaryViolation(fileSet, declaration.Pos(), "package-level Open function"))
				}
			case *ast.GenDecl:
				for _, specification := range declaration.Specs {
					switch specification := specification.(type) {
					case *ast.ValueSpec:
						for _, name := range specification.Names {
							if name.Name == "Open" {
								violations = append(violations, connectionBoundaryViolation(fileSet, name.Pos(), "package-level Open "+declaration.Tok.String()))
							}
						}
					case *ast.TypeSpec:
						if specification.Name.Name == "Open" {
							violations = append(violations, connectionBoundaryViolation(fileSet, specification.Pos(), "package-level Open type"))
						}
					}
				}
			}
		}
	}

	typeInformation := &types.Info{Uses: make(map[*ast.Ident]types.Object)}
	typeChecker := types.Config{
		Importer: importer.Default(),
		Error:    func(error) {},
	}
	if len(productionFiles) != 0 {
		_, _ = typeChecker.Check(directory, fileSet, productionFiles, typeInformation)
	}

	for _, file := range productionFiles {
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			qualifier, ok := selector.X.(*ast.Ident)
			if !ok || (selector.Sel.Name != "Open" && selector.Sel.Name != "OpenDB") {
				return true
			}
			object, ok := typeInformation.Uses[qualifier]
			if !ok {
				return true
			}
			packageName, ok := object.(*types.PkgName)
			if !ok || packageName.Imported().Path() != "database/sql" {
				return true
			}
			detail := qualifier.Name + "." + selector.Sel.Name
			violations = append(violations, connectionBoundaryViolation(fileSet, selector.Pos(), detail))
			return true
		})
	}
	return violations, nil
}

func connectionBoundaryViolation(fileSet *token.FileSet, position token.Pos, detail string) string {
	location := fileSet.Position(position)
	return fmt.Sprintf("%s:%d: production mysql package must not use %s", filepath.Base(location.Filename), location.Line, detail)
}

func TestConnectionBoundaryScannerCatchesProductionMutations(t *testing.T) {
	tests := []struct {
		name   string
		source string
		wants  []string
	}{
		{
			name: "database sql alias Open call in a second file",
			source: `package mysql
import dbsql "database/sql"
func connect() { _, _ = dbsql.Open("", "") }
`,
			wants: []string{"dbsql.Open"},
		},
		{
			name: "local Open function value in a second file",
			source: `package mysql
import dbsql "database/sql"
func connect() { opener := dbsql.Open; _, _ = opener("", "") }
`,
			wants: []string{"dbsql.Open"},
		},
		{
			name: "package Open variable references database sql Open",
			source: `package mysql
import dbsql "database/sql"
var Open = dbsql.Open
`,
			wants: []string{"package-level Open", "dbsql.Open"},
		},
		{
			name: "package variable references database sql OpenDB",
			source: `package mysql
import dbsql "database/sql"
var opener = dbsql.OpenDB
`,
			wants: []string{"dbsql.OpenDB"},
		},
		{
			name: "database sql dot import is forbidden",
			source: `package mysql
import . "database/sql"
`,
			wants: []string{"dot import database/sql"},
		},
		{
			name: "package function named Open",
			source: `package mysql
func Open() {}
`,
			wants: []string{"package-level Open"},
		},
		{
			name: "package constant named Open",
			source: `package mysql
const Open = 1
`,
			wants: []string{"package-level Open"},
		},
		{
			name: "package type named Open",
			source: `package mysql
type Open struct{}
`,
			wants: []string{"package-level Open"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			if err := os.WriteFile(filepath.Join(directory, "backend.go"), []byte("package mysql\n"), 0o600); err != nil {
				t.Fatalf("write clean backend fixture: %v", err)
			}
			if err := os.WriteFile(filepath.Join(directory, "connection.go"), []byte(test.source), 0o600); err != nil {
				t.Fatalf("write connection mutation fixture: %v", err)
			}

			violations, err := connectionBoundaryViolations(directory)
			if err != nil {
				t.Fatalf("scan fixture: %v", err)
			}
			got := strings.Join(violations, "\n")
			if !strings.Contains(got, "connection.go") {
				t.Fatalf("violations did not identify the second production file")
			}
			for _, want := range test.wants {
				if !strings.Contains(got, want) {
					t.Fatalf("violations did not contain required marker %q", want)
				}
			}
		})
	}
}

func TestConnectionBoundaryScannerIgnoresTestsAndUnrelatedOpeners(t *testing.T) {
	directory := t.TempDir()
	productionSource := `package mysql
import sqlite "example.com/sqlite"
var _ = sqlite.Open
`
	testSource := `package mysql
import dbsql "database/sql"
var _ = dbsql.Open
var _ = dbsql.OpenDB
`
	if err := os.WriteFile(filepath.Join(directory, "backend.go"), []byte(productionSource), 0o600); err != nil {
		t.Fatalf("write unrelated opener fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "fixture_test.go"), []byte(testSource), 0o600); err != nil {
		t.Fatalf("write test-only fixture: %v", err)
	}

	violations, err := connectionBoundaryViolations(directory)
	if err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %q, want no test-only or unrelated opener findings", violations)
	}
}

func TestConnectionBoundaryScannerAllowsShadowedSQLImportAliases(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{
			name: "function parameter shadows database sql alias",
			source: `package mysql
import dbsql "database/sql"
type localOpener struct { Open func() }
func inspect(dbsql localOpener) { _ = dbsql.Open }
var _ *dbsql.DB
`,
		},
		{
			name: "local variable shadows database sql alias",
			source: `package mysql
import dbsql "database/sql"
type localOpener struct{}
func (localOpener) OpenDB() {}
func inspect() {
	dbsql := localOpener{}
	dbsql.OpenDB()
}
var _ *dbsql.DB
`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			if err := os.WriteFile(filepath.Join(directory, "backend.go"), []byte("package mysql\n"), 0o600); err != nil {
				t.Fatalf("write clean backend fixture: %v", err)
			}
			if err := os.WriteFile(filepath.Join(directory, "connection.go"), []byte(test.source), 0o600); err != nil {
				t.Fatalf("write shadowing fixture: %v", err)
			}

			violations, err := connectionBoundaryViolations(directory)
			if err != nil {
				t.Fatalf("scan fixture: %v", err)
			}
			if len(violations) != 0 {
				t.Fatalf("violations = %q, want shadowed import alias to be allowed", violations)
			}
		})
	}
}
