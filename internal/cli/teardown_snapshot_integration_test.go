//go:build integration

package cli_test

// The GoReleaser snapshot's own binary through the removal gate: the one built
// with CGO_ENABLED=0 and -trimpath, the way a release is, run on the platform
// it was built for. It proves the exec through the verified descriptor works on
// a binary like the ones users get. It reads dist/ and writes only to a scratch
// root, and the release-snapshot job in go.yml runs it.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSnapshotBinaryThroughTheGate(t *testing.T) {
	dist, version := os.Getenv("TREERIG_SNAPSHOT_DIST"), os.Getenv("TREERIG_SNAPSHOT_VERSION")
	if dist == "" || version == "" {
		t.Skip("TREERIG_SNAPSHOT_DIST and TREERIG_SNAPSHOT_VERSION are not set to a GoReleaser snapshot's dist/ and its version")
	}
	built, err := filepath.Glob(filepath.Join(dist, "treerig_"+runtime.GOOS+"_"+runtime.GOARCH+"*", "treerig"))
	if err != nil || len(built) != 1 {
		t.Fatalf("the built treerig for %s/%s in %s: %v, %v", runtime.GOOS, runtime.GOARCH, dist, built, err)
	}
	binary := built[0]
	digest := fileSHA256(t, binary)
	dir := scratchTeardownRoot(t)

	pinned := runTreerig(t, binary, dir, []string{"teardown", "pin", "--file", binary, "--sha256", digest, "--release", "snapshot", "--yes"}, treerigOptions{})
	if pinned.Code != 0 {
		t.Fatalf("pin = %+v", pinned)
	}
	// An empty directory: the removal gets past the gate and stops at the first
	// thing that needs a treerig.toml, so nothing is touched.
	stored := filepath.Join(dir, "control-home", "control-home", "teardown", digest)
	result := runTreerig(t, binary, dir, []string{"worktree", "remove", "--yes"}, treerigOptions{timeout: time.Minute})
	want := "Running the pinned teardown binary " + stored + " (release snapshot) to remove this worktree.\n" +
		"Running as the pinned teardown binary (version " + version + ").\n"
	if result.Code != 1 || result.Stdout != "" || !strings.HasPrefix(result.Stderr, want) || strings.Contains(result.Stderr, "Refusing") {
		t.Fatalf("remove = %+v\nwant stderr to start with\n%s", result, want)
	}
}
