package style

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// trackedFiles lists the files git tracks under root, as slash-separated
// paths relative to it. It uses git ls-files, not a directory walk, because a
// checkout also holds untracked local notes that the repository tests must
// not read. It fails the test when git is missing, fails (with git's own
// message) or lists so few files that it cannot be seeing the repository.
func trackedFiles(t *testing.T, root string) []string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is not on PATH, so the tracked files cannot be listed: %v", err)
	}
	out, err := exec.Command(git, "-C", root, "ls-files", "-z").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("git ls-files: %v: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
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
	return files
}
