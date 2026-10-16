package style

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The retired product name is spelled in pieces so this file does not match
// itself.
var previousName = regexp.MustCompile(`(?i)` + "be" + `rth`)

// previousNameAllowed lists the tracked files that may still carry the
// retired name, each with the reason. At present it covers Markdown only: the
// docs rename pull request empties it, and from then on no tracked file or
// path may contain the name. A key must be a tracked file that really matches,
// so an entry cannot outlive the text it excuses.
var previousNameAllowed = map[string]string{
	"AGENTS.md":               "Markdown, renamed with the rest of the docs",
	"CONTRIBUTING.md":         "Markdown, renamed with the rest of the docs",
	"README.md":               "Markdown, renamed with the rest of the docs",
	"docs/design/config.md":   "Markdown, renamed with the rest of the docs",
	"docs/design/locks.md":    "Markdown, renamed with the rest of the docs",
	"docs/design/release.md":  "Markdown, renamed with the rest of the docs",
	"docs/design/routing.md":  "Markdown, renamed with the rest of the docs",
	"docs/design/state.md":    "Markdown, renamed with the rest of the docs",
	"docs/design/teardown.md": "Markdown, renamed with the rest of the docs",
	"docs/hooks.md":           "Markdown, renamed with the rest of the docs",
}

// TestNoPreviousName fails when a tracked file's path or contents still carry
// the retired product name in any letter case. It lists files with git ls-files,
// not a directory walk, because a checkout also holds untracked local notes
// that may mention the name. The allowlist may name Markdown files only.
func TestNoPreviousName(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is not on PATH, so the tracked files cannot be listed: %v", err)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(git, "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" {
			files = append(files, name)
		}
	}
	if len(files) < 500 {
		t.Fatalf("git ls-files listed only %d files from %s; the listing is not seeing the repository", len(files), root)
	}

	for path := range previousNameAllowed {
		if !strings.HasSuffix(path, ".md") {
			t.Errorf("the allowlist entry %q is not a Markdown file; only Markdown may be excused", path)
		}
	}

	matched := map[string]bool{}
	var problems []string
	for _, name := range files {
		found, err := mentionsPreviousName(root, name)
		if err != nil {
			t.Fatal(err)
		}
		if found == "" {
			continue
		}
		matched[name] = true
		if _, ok := previousNameAllowed[name]; !ok {
			problems = append(problems, fmt.Sprintf("%s: %s", name, found))
		}
	}
	for path, reason := range previousNameAllowed {
		if !matched[path] {
			problems = append(problems, fmt.Sprintf("%s: allowlisted (%s) but no longer carries the name; remove the entry", path, reason))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("the retired product name is back:\n  %s", strings.Join(problems, "\n  "))
	}
}

// mentionsPreviousName reports where the retired name appears in the tracked
// file name, in its path or its contents, or "" when it does not. A tracked
// file that is missing from the working tree is checked by path only, and a
// tracked symlink by its path and its target.
func mentionsPreviousName(root, name string) (string, error) {
	if previousName.MatchString(name) {
		return "the path carries the name", nil
	}
	info, err := os.Lstat(filepath.Join(root, name))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", name, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(filepath.Join(root, name))
		if err != nil {
			return "", fmt.Errorf("read link %s: %w", name, err)
		}
		if previousName.MatchString(target) {
			return "the link's target carries the name", nil
		}
		return "", nil
	}
	if !info.Mode().IsRegular() {
		return "", nil
	}
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	if loc := previousName.FindIndex(data); loc != nil {
		return fmt.Sprintf("line %d carries the name", bytes.Count(data[:loc[0]], []byte("\n"))+1), nil
	}
	return "", nil
}

// TestPreviousNameDetector proves the check can see what it looks for, in
// every letter case and in a path, so a clean repository means clean and not
// a pattern that stopped matching.
func TestPreviousNameDetector(t *testing.T) {
	dir := t.TempDir()
	word := "be" + "rth"
	if err := os.WriteFile(filepath.Join(dir, "lower.txt"), []byte("a\nuses "+word+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "upper.txt"), []byte(strings.ToUpper(word)+"_HOME"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "clean.txt"), []byte("nothing here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		want string
	}{
		{"lower.txt", "line 2 carries the name"},
		{"upper.txt", "line 1 carries the name"},
		{"clean.txt", ""},
		{"missing.txt", ""},
		{"dir/" + strings.ToUpper(word[:1]) + word[1:] + ".toml", "the path carries the name"},
	} {
		got, err := mentionsPreviousName(dir, tc.name)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
