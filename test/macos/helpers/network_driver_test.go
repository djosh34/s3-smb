// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

func TestNetworkNotTestedDoesNotPass(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "../harness_test.go", nil, 0)
	must(t, err)
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		message, ok := call.Args[0].(*ast.BasicLit)
		if !ok || message.Kind != token.STRING {
			return true
		}
		text, err := strconv.Unquote(message.Value)
		must(t, err)
		if !strings.HasPrefix(text, "not tested:") {
			return true
		}
		if method.Sel.Name == "Skip" {
			t.Fatal("not tested must not produce a green Mac check")
		}
		if method.Sel.Name == "Fatal" {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("missing nonpassing not-tested result")
	}
}

// Check the Darwin driver's selection order on Linux. remoteBackup and
// resumeBackup hold a directory until detach; restore does not release it.
func TestNetworkBackupSelectionLifecycle(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "../network_test.go", nil, 0)
	must(t, err)
	checked := 0
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || (function.Name.Name != "networkScenario" && function.Name.Name != "networkOutage") {
			continue
		}
		checked++
		t.Run(function.Name.Name, func(t *testing.T) {
			held, selections, restores := false, 0, 0
			ast.Inspect(function.Body, func(node ast.Node) bool {
				method := harnessCall(node)
				switch method {
				case "remoteBackup", "resumeBackup":
					if held {
						t.Fatal("backup directory already held open before", method)
					}
					held = true
					selections++
				case "restore":
					if !held {
						t.Fatal("restore without a held selection")
					}
					restores++
				case "detach", "stopClient":
					held = false
				case "mount":
					if held {
						t.Fatal("remount before releasing the selected backup")
					}
				}
				return true
			})
			if held || selections != 2 || restores != 2 {
				t.Fatal("both restores must select and release their backups", held, selections, restores)
			}
		})
	}
	if checked != 2 {
		t.Fatal("network scenario functions not found", checked)
	}
}

func harnessCall(node ast.Node) string {
	call, ok := node.(*ast.CallExpr)
	if !ok {
		return ""
	}
	method, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	receiver, ok := method.X.(*ast.Ident)
	if !ok || receiver.Name != "h" {
		return ""
	}
	return method.Sel.Name
}
