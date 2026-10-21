package style

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// envName is the shape of an environment variable name.
var envName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,}$`)

// notAlnum matches what is dropped when a name is compared as a word.
var notAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// configCode are the packages whose string literals are keys and names a
// configuration file or the environment can carry; the escape hatch must not
// appear in any of them (docs/design/teardown.md, "The developer escape
// hatch").
var configCode = []string{"internal/config", "internal/settings", "internal/shape", "internal/layout", "cmd/treerig"}

// devTeardownProblems lists, for one parsed file, each string literal that
// would make --dev-teardown something other than a per-invocation flag: any
// environment-variable-shaped name for it anywhere, and any mention at all in
// the packages that define configuration keys.
func devTeardownProblems(fset *token.FileSet, file *ast.File, rel string) []string {
	var problems []string
	inConfigCode := false
	for _, prefix := range configCode {
		if strings.HasPrefix(rel, prefix+"/") {
			inConfigCode = true
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil || !strings.Contains(notAlnum.ReplaceAllString(strings.ToLower(value), ""), "devteardown") {
			return true
		}
		at := fmt.Sprintf("%s:%d", rel, fset.Position(lit.Pos()).Line)
		switch {
		case envName.MatchString(value):
			problems = append(problems, at+": an environment variable name for --dev-teardown")
		case inConfigCode:
			problems = append(problems, at+": --dev-teardown named in code that defines configuration keys or reads the environment")
		}
		return true
	})
	return problems
}

// --dev-teardown is a per-invocation flag and nothing else: never a key in
// settings.toml or treerig.toml (the latter is branch content, so a worktree
// would authorise itself), never an environment variable, and never published
// in the config schema.
func TestDevTeardownIsOnlyAFlag(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
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
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		problems = append(problems, devTeardownProblems(fset, file, filepath.ToSlash(rel))...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d files from %s; the walk is not seeing the module", scanned, root)
	}
	schema, err := os.ReadFile(filepath.Join(root, "schema", "treerig.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(notAlnum.ReplaceAllString(strings.ToLower(string(schema)), ""), "devteardown") {
		problems = append(problems, "schema/treerig.schema.json: --dev-teardown named in the treerig.toml schema")
	}
	if len(problems) > 0 {
		t.Fatalf("--dev-teardown must stay a per-invocation flag:\n  %s", strings.Join(problems, "\n  "))
	}
}

// TestDevTeardownDetector proves the check can see what it looks for.
func TestDevTeardownDetector(t *testing.T) {
	source := "package p\n\n" +
		"var a = \"TREERIG_DEV_TEARDOWN\"\n" +
		"var b = \"devTeardown\"\n" +
		"var c = \"dev-teardown\"\n" +
		"var d = \"unrelated\"\n"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := devTeardownProblems(fset, file, "internal/cli/p.go"); len(got) != 1 || !strings.HasPrefix(got[0], "internal/cli/p.go:3:") {
		t.Fatalf("outside configuration code: flagged %v, want only the environment name", got)
	}
	if got := devTeardownProblems(fset, file, "internal/settings/p.go"); len(got) != 3 {
		t.Fatalf("in configuration code: flagged %v, want all three mentions", got)
	}
}
