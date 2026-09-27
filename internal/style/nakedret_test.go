package style

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// No naked returns: a function with named results still says what each
// return returns. A bare `return` makes the reader find the last assignment
// to every result to know what leaves the function. This covers every Go
// file in the module, tests and tools included, and function literals too.
func TestNoNakedReturns(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var naked []string
	scanned := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "dist", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		var check func(ftype *ast.FuncType, body *ast.BlockStmt)
		check = func(ftype *ast.FuncType, body *ast.BlockStmt) {
			if body == nil {
				return
			}
			named := ftype.Results != nil && len(ftype.Results.List) > 0 && len(ftype.Results.List[0].Names) > 0
			ast.Inspect(body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.FuncLit:
					check(n.Type, n.Body) // its returns answer to its own results
					return false
				case *ast.ReturnStmt:
					if named && len(n.Results) == 0 {
						naked = append(naked, fmt.Sprintf("%s:%d", rel, fset.Position(n.Pos()).Line))
					}
				}
				return true
			})
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				check(fn.Type, fn.Body)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d files from %s; the walk is not seeing the module", scanned, root)
	}
	if len(naked) > 0 {
		t.Fatalf("naked returns (return the named results explicitly):\n  %s", strings.Join(naked, "\n  "))
	}
}
