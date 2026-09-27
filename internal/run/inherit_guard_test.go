package run_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// Hooks must never inherit a lock (the owner's rule): a hook that daemonizes
// would hold it for as long as it lives. Only Docker calls do, and the one
// place that says so is internal/docker. This fails the moment anything else
// sets Spec.InheritLocks, however it is spelled.
func TestOnlyDockerInheritsLocks(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join("internal", "docker", "docker.go")
	var setters []string
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
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.KeyValueExpr:
				if id, ok := n.Key.(*ast.Ident); ok && id.Name == "InheritLocks" && rel != allowed {
					setters = append(setters, rel)
				}
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "InheritLocks" && rel != allowed {
						setters = append(setters, rel)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d files from %s; the walk is not seeing the module", scanned, root)
	}
	if len(setters) > 0 {
		t.Fatalf("InheritLocks is set outside %s, in: %s", allowed, strings.Join(setters, ", "))
	}
}
