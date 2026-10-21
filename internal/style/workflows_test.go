package style

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cosignInstaller matches the sigstore/cosign-installer step of a workflow: the
// commit it is pinned to and the cosign-release input, with only comment lines
// allowed between the step's with: and the input.
var cosignInstaller = regexp.MustCompile(`(?m)^\s*(?:- )?uses:\s*sigstore/cosign-installer@([0-9a-f]{40})[^\n]*\n\s*with:\s*\n(?:\s*#[^\n]*\n)*\s*cosign-release:\s*(\S+)`)

// The release workflow signs with the cosign that go.yml's release-snapshot job
// pins the snapshot with and release-smoke.yml verifies the published release
// with. If they drifted, the pin flow would be tested with a tool the release
// does not use: the installer commit and cosign-release are set in all three
// files and change together (docs/design/release.md).
func TestCosignPinnedTheSameInAllWorkflows(t *testing.T) {
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
	release := read("release.yml")
	for _, name := range []string{"go.yml", "release-smoke.yml"} {
		if other := read(name); other != release {
			t.Fatalf("release.yml pins cosign-installer %s with cosign-release %s, but %s pins %s with %s: change them together",
				release.sha, release.release, name, other.sha, other.release)
		}
	}
}

// The release smoke workflow checks the real release: the real signature, the
// real URLs and the real pin. The script's local-testing options (a local
// --base-url, skipping the signature or the packages, pinning a file instead of
// a release, expecting a development build, the CI snapshot mode) would turn
// that into a check of something else, and the script refuses them under
// Actions anyway. So the workflow must not even name them.
func TestReleaseSmokeWorkflowUsesNoLocalTestingOptions(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release-smoke.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, option := range []string{"--base-url", "--no-signature-check", "--no-packages", "--pin", "--dev-build", "--ci-snapshot"} {
		if strings.Contains(string(data), option) {
			t.Errorf("release-smoke.yml mentions %s, an option of .github/scripts/release-smoke.sh for local testing only", option)
		}
	}
}
