package style

import (
	"fmt"
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
	spikeMarker = regexp.MustCompile(`\b` + "SPI" + `KE:`)
	todoMarker  = regexp.MustCompile(`\b` + "TO" + `DO\b(\(#[0-9]+\))?`)
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
		for _, group := range file.Comments {
			for _, comment := range group.List {
				at := func() string { return fmt.Sprintf("%s:%d", rel, fset.Position(comment.Pos()).Line) }
				if spikeMarker.MatchString(comment.Text) {
					problems = append(problems, at()+": a spike marker; resolve it, or track it as TODO(#issue)")
				}
				for _, match := range todoMarker.FindAllStringSubmatch(comment.Text, -1) {
					if match[1] == "" {
						problems = append(problems, at()+": a to-do with no issue; write it as TODO(#issue)")
					}
				}
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
	if len(problems) > 0 {
		t.Fatalf("comment markers:\n  %s", strings.Join(problems, "\n  "))
	}
}
