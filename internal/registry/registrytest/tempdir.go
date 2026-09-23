package registrytest

import (
	"path/filepath"
	"testing"
)

// TempDir is t.TempDir with its symbolic links resolved, as the TypeScript
// helpers' realpath(mkdtemp(...)). The registry works in physical paths, and
// on macOS the temporary directory lives under /var, a symbolic link to
// /private/var: a fixture built on the unresolved path would register one
// spelling while the code under test looks up the other.
func TempDir(t testing.TB) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
