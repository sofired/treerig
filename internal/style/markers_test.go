package style

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The markers are spelled in pieces so this file does not match itself.
var (
	spikeMarker = regexp.MustCompile(`(?i)\b` + "spi" + `ke:`)
	// The to-do marker in any letter case, its plural, or its fix-me
	// cousin, then the issue that will do it, a space allowed.
	todoMarker = regexp.MustCompile(`(?i)\b(?:` + "to" + `dos?|` + "fix" + `me)\b(\s*\(#[0-9]+\))?`)
)

// Deferred work is tracked, not left in comments: the spike's shortcut
// markers are gone for good, and every remaining to-do names the issue that
// will do it, as TODO(#41). Only comments are checked, in every Go file in
// the module.
func TestCommentMarkers(t *testing.T) {
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
			case "node_modules", "testdata", "dist", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		problems = append(problems, commentProblems(fset, file, rel)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d files from %s; the walk is not seeing the module", scanned, root)
	}
	if len(problems) > 0 {
		t.Fatalf("comment markers:\n  %s", strings.Join(problems, "\n  "))
	}
}

// TestCommentMarkersDetector proves the check can see what it looks for, so
// a clean module means clean and not a detector that stopped matching.
func TestCommentMarkersDetector(t *testing.T) {
	source := "package p\n\n" +
		"// " + "SPI" + "KE: a shortcut\n" +
		"// " + "spi" + "ke: another\n" +
		"// " + "TO" + "DO: untracked\n" +
		"// " + "to" + "do: lower case\n" +
		"// " + "To" + "do: mixed case\n" +
		"// the " + "TO" + "DOs below\n" +
		"// " + "FIX" + "ME: this\n" +
		"// " + "TO" + "DO(#41): tracked\n" +
		"// " + "TO" + "DO (#41): tracked, with a space\n" +
		"// a todolist and an unfixmeable word are not markers\n" +
		"var x = 1\n"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", source, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	problems := commentProblems(fset, file, "p.go")
	var lines []string
	for _, p := range problems {
		lines = append(lines, strings.SplitN(p, ": ", 2)[0])
	}
	want := []string{"p.go:3", "p.go:4", "p.go:5", "p.go:6", "p.go:7", "p.go:8", "p.go:9"}
	if strings.Join(lines, " ") != strings.Join(want, " ") {
		t.Fatalf("flagged %v, want %v:\n%s", lines, want, strings.Join(problems, "\n"))
	}
}

// commentProblems scans file's comments for a spike marker or an untracked
// to-do (one with no "(#issue)" reference) and returns one "path:line:
// reason" string per problem found, in comment order.
func commentProblems(fset *token.FileSet, file *ast.File, rel string) []string {
	var problems []string
	for _, group := range file.Comments {
		for _, comment := range group.List {
			at := fmt.Sprintf("%s:%d", rel, fset.Position(comment.Pos()).Line)
			if spikeMarker.MatchString(comment.Text) {
				problems = append(problems, at+": a spike marker; resolve it, or track it as TODO(#issue)")
			}
			for _, match := range todoMarker.FindAllStringSubmatch(comment.Text, -1) {
				if match[1] == "" {
					problems = append(problems, at+": a to-do with no issue; write it as TODO(#issue)")
				}
			}
		}
	}
	return problems
}
