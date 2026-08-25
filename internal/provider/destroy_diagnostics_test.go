package provider

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// TestDeleteNeverWarns guards a class of defect rather than one instance.
//
// Five of the six resources had drifted into downgrading every destroy failure
// to a warning, so OpenTofu reported a successful destroy over work the
// homeserver refused, with exit status 0 and the resource gone from state.
// matrix_room_state and matrix_space_child performed the same operation and
// handled failure in opposite ways. See issue #45.
//
// Every Delete now reports a refusal as an error, through failedDestroy. This
// reads the package's own source so a new resource cannot quietly reintroduce
// the old shape.
func TestDeleteNeverWarns(t *testing.T) {
	files, err := filepath.Glob("*_resource.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no resource files found: %v", err)
	}

	var checked int
	for _, file := range files {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "Delete" || fn.Recv == nil || fn.Body == nil {
				continue
			}
			checked++
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "AddWarning" {
					t.Errorf("%s: Delete calls AddWarning at %s. A destroy that the homeserver "+
						"refused must report an error, or it claims success over work it never "+
						"did (issue #45). Use failedDestroy.", file, fset.Position(sel.Pos()))
				}
				return true
			})
		}
	}

	// A guard that inspects nothing passes for the wrong reason.
	if checked < 6 {
		t.Fatalf("only %d Delete methods inspected; the provider has at least 6", checked)
	}
}
