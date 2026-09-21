package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestProcessExitHappensOnlyAfterRunReturns(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var runFound bool
	var exits []string
	var finalExitCallsRun bool
	var runDefersAppClose bool
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		if function.Name.Name == "run" {
			runFound = true
			ast.Inspect(function.Body, func(node ast.Node) bool {
				deferred, ok := node.(*ast.DeferStmt)
				if !ok {
					return true
				}
				ast.Inspect(deferred.Call, func(deferredNode ast.Node) bool {
					call, ok := deferredNode.(*ast.CallExpr)
					if !ok {
						return true
					}
					selector, ok := call.Fun.(*ast.SelectorExpr)
					identifier, identifierOK := selectorReceiver(selector, ok)
					if identifierOK && identifier.Name == "application" && selector.Sel.Name == "Close" {
						runDefersAppClose = true
					}
					return true
				})
				return true
			})
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			identifier, identifierOK := selector.X.(*ast.Ident)
			if identifierOK && identifier.Name == "os" && selector.Sel.Name == "Exit" {
				exits = append(exits, function.Name.Name)
				if len(call.Args) == 1 {
					if runCall, ok := call.Args[0].(*ast.CallExpr); ok {
						if runIdentifier, ok := runCall.Fun.(*ast.Ident); ok && runIdentifier.Name == "run" {
							finalExitCallsRun = true
						}
					}
				}
			}
			return true
		})
	}
	if !runFound {
		t.Fatal("run() helper is missing; main cannot defer App cleanup before process exit")
	}
	if len(exits) != 1 || exits[0] != "main" {
		t.Fatalf("os.Exit call owners = %v, want one final exit in main", exits)
	}
	if !finalExitCallsRun {
		t.Fatal("main must exit with run()'s result")
	}
	if !runDefersAppClose {
		t.Fatal("run must defer application.Close before returning")
	}
}

func selectorReceiver(selector *ast.SelectorExpr, ok bool) (*ast.Ident, bool) {
	if !ok {
		return nil, false
	}
	identifier, identifierOK := selector.X.(*ast.Ident)
	return identifier, identifierOK
}
