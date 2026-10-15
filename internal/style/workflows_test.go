package style

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// cosignInstaller matches the sigstore/cosign-installer step of a workflow: the
// commit it is pinned to and the cosign-release input, with only comment lines
// allowed between the step's with: and the input.
var cosignInstaller = regexp.MustCompile(`(?m)^\s*(?:- )?uses:\s*sigstore/cosign-installer@([0-9a-f]{40})[^\n]*\n\s*with:\s*\n(?:\s*#[^\n]*\n)*\s*cosign-release:\s*(\S+)`)

// The release workflow signs with the cosign that go.yml's release-snapshot job
// pins the snapshot with. If the two drifted, the pin flow would be tested with
// a tool the release does not use: the installer commit and cosign-release are
// set in both files and change together (docs/design/release.md).
func TestCosignPinnedTheSameInBothWorkflows(t *testing.T) {
	type pin struct{ sha, release string }
	read := func(name string) pin {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		m := cosignInstaller.FindAllSubmatch(data, -1)
		if len(m) != 1 {
			t.Fatalf("%s: want exactly one sigstore/cosign-installer step pinned by a 40-hex commit with `with:` on the next line and a cosign-release input under it, found %d", name, len(m))
		}
		return pin{string(m[0][1]), string(m[0][2])}
	}
	release, snapshot := read("release.yml"), read("go.yml")
	if release != snapshot {
		t.Fatalf("release.yml pins cosign-installer %s with cosign-release %s, but go.yml pins %s with %s: change them together",
			release.sha, release.release, snapshot.sha, snapshot.release)
	}
}
