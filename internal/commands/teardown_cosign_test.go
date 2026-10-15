package commands_test

// `teardown pin --release` against a real cosign (#85, docs/design/teardown.md,
// "CI"). The signature is hermetic: a throwaway key, a signing config that
// names no Rekor or TSA, and a test-only policy that verifies against the
// throwaway public key with `--insecure-ignore-tlog`. The production policy is
// keyless and cannot be exercised without a real release; its argv is pinned by
// the fakerun tests, and its identity is first exercised at #147.
//
// The tests skip without a cosign 3.0.0 or newer on PATH. With
// BERTH_REQUIRE_COSIGN=1 (the release-snapshot job) a missing cosign fails
// instead. The snapshot tests run against a GoReleaser snapshot's dist/ named
// by BERTH_SNAPSHOT_DIST and BERTH_SNAPSHOT_VERSION, and skip without them,
// or fail, under BERTH_REQUIRE_COSIGN=1.

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sofired/berth/internal/binstore"
	"github.com/sofired/berth/internal/commands"
	"github.com/sofired/berth/internal/registry/registrytest"
	"github.com/sofired/berth/internal/releasefetch"
	"github.com/sofired/berth/internal/releasefetch/releasetest"
	"github.com/sofired/berth/internal/run/osrun"
	"github.com/sofired/berth/internal/semver"
	"github.com/sofired/berth/internal/teardown"
)

// keyPolicy is the test-only policy: verify-blob against a public key, with the
// transparency log ignored. It exists only in tests.
type keyPolicy struct{ pub string }

// Signer is the throwaway key's name.
func (keyPolicy) Signer() string { return "throwaway-test-key" }

// Args verifies against the public key, ignoring the transparency log.
func (p keyPolicy) Args(bundle, blob string) []string {
	return []string{"--key", p.pub, "--bundle", bundle, "--insecure-ignore-tlog=true", blob}
}

// cosignTool is a real cosign 3.
type cosignTool struct {
	path string
	home string
}

// skipUnlessRequired skips the test with the reason, or fails it when
// BERTH_REQUIRE_COSIGN=1: the release-snapshot job sets that so that a missing
// cosign, or a missing snapshot, cannot pass as a skip.
func skipUnlessRequired(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("BERTH_REQUIRE_COSIGN") == "1" {
		t.Fatalf("BERTH_REQUIRE_COSIGN=1, so this test cannot skip: "+format, args...)
	}
	t.Skipf(format, args...)
}

// requireSnapshot returns the GoReleaser snapshot's dist/ and version named by
// BERTH_SNAPSHOT_DIST and BERTH_SNAPSHOT_VERSION. When they are unset or there
// is no checksums.txt there, it hands the test to skipUnlessRequired.
func requireSnapshot(t *testing.T) (dist, version string) {
	t.Helper()
	dist, version = os.Getenv("BERTH_SNAPSHOT_DIST"), os.Getenv("BERTH_SNAPSHOT_VERSION")
	if dist == "" || version == "" {
		skipUnlessRequired(t, "BERTH_SNAPSHOT_DIST and BERTH_SNAPSHOT_VERSION are not set to a GoReleaser snapshot's dist/ and its version")
	}
	if _, err := os.Stat(filepath.Join(dist, "checksums.txt")); err != nil {
		skipUnlessRequired(t, "no snapshot in %s: %v", dist, err)
	}
	return dist, version
}

// requireCosign3 finds a cosign 3.0.0 or newer on PATH, skipping the test
// when there is none (or failing it, when BERTH_REQUIRE_COSIGN is set).
func requireCosign3(t *testing.T) cosignTool {
	t.Helper()
	unavailable := func(format string, args ...any) {
		t.Helper()
		skipUnlessRequired(t, "cosign 3.0.0 or newer is needed, but "+format, args...)
	}
	path, err := exec.LookPath("cosign")
	if err != nil {
		unavailable("cosign is not on PATH")
	}
	home := registrytest.TempDir(t)
	cmd := exec.Command(path, "version", "--json")
	cmd.Env = []string{"PATH=" + filepath.Dir(path), "HOME=" + home}
	out, err := cmd.Output()
	if err != nil {
		unavailable("`cosign version --json` failed: %v", err)
	}
	var version struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := json.Unmarshal(out, &version); err != nil || !semver.AtLeast(strings.TrimPrefix(version.GitVersion, "v"), releasefetch.MinCosign) {
		unavailable("%s is %q", path, version.GitVersion)
	}
	return cosignTool{path: path, home: home}
}

// run runs cosign in dir with an empty key password and no home of the
// developer's own.
func (c cosignTool) run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(c.path, args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + filepath.Dir(c.path), "HOME=" + c.home, "COSIGN_PASSWORD="}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cosign %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// signer is a throwaway key and a signing config that names no services.
type signer struct {
	tool         cosignTool
	dir          string
	key, pub, sc string
}

// newSigner makes the key pair and the signing config in a scratch directory.
func newSigner(t *testing.T, tool cosignTool) signer {
	t.Helper()
	dir := registrytest.TempDir(t)
	s := signer{tool: tool, dir: dir, key: filepath.Join(dir, "cosign.key"), pub: filepath.Join(dir, "cosign.pub"), sc: filepath.Join(dir, "signing-config.json")}
	tool.run(t, dir, "generate-key-pair")
	tool.run(t, dir, "signing-config", "create", "--no-default-fulcio", "--no-default-oidc", "--no-default-rekor", "--no-default-tsa", "--out", s.sc)
	return s
}

// sign returns the bundle of checksums, signed offline.
func (s signer) sign(t *testing.T, checksums []byte) []byte {
	t.Helper()
	blob := filepath.Join(s.dir, "blob")
	bundle := filepath.Join(s.dir, "blob.sigstore.json")
	if err := os.WriteFile(blob, checksums, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(bundle)
	s.tool.run(t, s.dir, "sign-blob", "--key", s.key, "--bundle", bundle, "--signing-config", s.sc, "--yes", blob)
	data, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// useRealCosign wires f's Env to a real runner and a PATH of just cosign's directory.
func (f *releaseFixture) useRealCosign(tool cosignTool) {
	f.env.Runner = osrun.New()
	f.env.Environ = []string{"PATH=" + filepath.Dir(tool.path), "HOME=" + tool.home}
	f.cosign = tool.path
}

func TestTeardownPinWithRealCosign(t *testing.T) {
	tool := requireCosign3(t)

	t.Run("verifies a release signed with a throwaway key, end to end", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.useRealCosign(tool)
		key := newSigner(t, tool)
		f.release.Set("checksums.txt.sigstore.json", key.sign(t, f.release.File("checksums.txt")))
		options := f.options()
		options.Policy = func(string) releasefetch.Policy { return keyPolicy{key.pub} }
		if err := f.pin(options); err != nil {
			t.Fatal(err)
		}
		if leftovers, _ := os.ReadDir(f.control.CosignTmp); len(leftovers) != 0 {
			t.Fatalf("cosign's temporary directory still holds %d entries", len(leftovers))
		}
		pin := f.readPin(t)
		if pin.Signer != "throwaway-test-key" || pin.Platforms["linux_amd64"].BinarySHA256 != binstore.Hash(f.release.Binaries["linux_amd64"]) {
			t.Fatalf("pin = %+v", pin)
		}
	})

	t.Run("refuses checksums.txt that were changed after signing, quoting cosign", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.useRealCosign(tool)
		key := newSigner(t, tool)
		f.release.Set("checksums.txt.sigstore.json", key.sign(t, f.release.File("checksums.txt")))
		f.release.Set("checksums.txt", append(f.release.File("checksums.txt"), '\n'))
		options := f.options()
		options.Policy = func(string) releasefetch.Policy { return keyPolicy{key.pub} }
		err := f.pin(options)
		if err == nil || !strings.Contains(err.Error(), `cosign said: "`) || !strings.Contains(err.Error(), "invalid signature") || !strings.Contains(err.Error(), "Do not trust this release.") {
			t.Fatalf("err = %v", err)
		}
		f.wantNothingPinned(t)
	})

	t.Run("refuses a signature made by another key", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.useRealCosign(tool)
		trusted, attacker := newSigner(t, tool), newSigner(t, tool)
		f.release.Set("checksums.txt.sigstore.json", attacker.sign(t, f.release.File("checksums.txt")))
		options := f.options()
		options.Policy = func(string) releasefetch.Policy { return keyPolicy{trusted.pub} }
		err := f.pin(options)
		if err == nil || !strings.Contains(err.Error(), "Do not trust this release.") {
			t.Fatalf("err = %v", err)
		}
		f.wantNothingPinned(t)
	})

	t.Run("refuses a bundle that is not a bundle", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.useRealCosign(tool)
		key := newSigner(t, tool)
		f.release.Set("checksums.txt.sigstore.json", []byte("this is not a bundle"))
		options := f.options()
		options.Policy = func(string) releasefetch.Policy { return keyPolicy{key.pub} }
		if err := f.pin(options); err == nil || !strings.Contains(err.Error(), "cosign could not verify checksums.txt. cosign said: ") || strings.Contains(err.Error(), "--file") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestTeardownPinWithRealCosignAndNoTrustRoot(t *testing.T) {
	tool := requireCosign3(t)
	f := newReleaseFixture(t, "1.2.3")
	f.useRealCosign(tool)
	// The production (keyless) policy needs the Sigstore trust root; the proxy
	// the user's environment names (allowlisted) refuses every connection, and
	// berth's own cold TUF cache holds nothing. cosign fails to load the trust
	// root, which says nothing about the release.
	f.env.Environ = append(f.env.Environ, "HTTPS_PROXY=http://127.0.0.1:1", "https_proxy=http://127.0.0.1:1")
	err := f.pin(f.options())
	if err == nil || !strings.Contains(err.Error(), "cosign could not load the Sigstore trust root. cosign said: ") ||
		!strings.Contains(err.Error(), "`cosign initialize`") || !strings.Contains(err.Error(), "`berth teardown pin --file PATH --sha256 HEX --release LABEL`") ||
		strings.Contains(err.Error(), "Do not trust") {
		t.Fatalf("err = %v", err)
	}
	f.wantNothingPinned(t)
}

func TestTeardownPinSnapshot(t *testing.T) {
	dist, version := requireSnapshot(t)
	tool := requireCosign3(t)

	f := newTeardownFixture(t)
	release := releasetest.FromDir(t, version, dist)
	key := newSigner(t, tool)
	release.Set("checksums.txt.sigstore.json", key.sign(t, release.File("checksums.txt")))
	f.env.Runner = osrun.New()
	f.env.Environ = []string{"PATH=" + filepath.Dir(tool.path), "HOME=" + tool.home}
	f.env.Releases = release.Fetcher()
	f.env.OS, f.env.Arch = runtime.GOOS, runtime.GOARCH

	options := commands.TeardownPinOptions{
		Release: version, Yes: true, Source: release.Source(),
		Policy: func(string) releasefetch.Policy { return keyPolicy{key.pub} },
		Now:    func() time.Time { return teardownTime },
	}
	if err := f.pin(options); err != nil {
		t.Fatal(err)
	}
	pin := f.readPin(t)
	platform := teardown.PlatformKey(runtime.GOOS, runtime.GOARCH)

	// All four archive digests are the snapshot's checksums.txt's.
	digests, err := releasefetch.ParseChecksums(release.File("checksums.txt"), version)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range releasefetch.Platforms {
		if pin.Platforms[key].ArchiveSHA256 != digests[key] {
			t.Fatalf("%s: archive_sha256 %q, checksums.txt lists %q", key, pin.Platforms[key].ArchiveSHA256, digests[key])
		}
	}
	// The host's binary_sha256 is the hash of the berth GoReleaser built.
	built, err := filepath.Glob(filepath.Join(dist, "berth_"+runtime.GOOS+"_"+runtime.GOARCH+"*", "berth"))
	if err != nil || len(built) != 1 {
		t.Fatalf("the built berth for %s: %v, %v", platform, built, err)
	}
	want, err := binstore.HashFile(built[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := pin.Platforms[platform].BinarySHA256; got != want {
		t.Fatalf("binary_sha256 = %s, want %s (the built %s)", got, want, built[0])
	}
	if pin.Release != version || !bytes.HasSuffix([]byte(pin.Source), []byte("/releases/download/v"+version+"/")) {
		t.Fatalf("pin = %+v", pin)
	}
	// status sees an intact pin.
	if err := f.status(false, "0.0.0-dev"); err != nil {
		t.Fatalf("status: %v", err)
	}
}

func TestTeardownPinSnapshotTampering(t *testing.T) {
	dist, version := requireSnapshot(t)
	tool := requireCosign3(t)
	archive := releasefetch.ArchiveName(version, teardown.PlatformKey(runtime.GOOS, runtime.GOARCH))

	// pin signs the snapshot's real checksums.txt, lets tamper change what is
	// served afterwards, and pins.
	pin := func(t *testing.T, tamper func(r *releasetest.Release)) (*teardownFixture, error) {
		t.Helper()
		f := newTeardownFixture(t)
		release := releasetest.FromDir(t, version, dist)
		key := newSigner(t, tool)
		release.Set("checksums.txt.sigstore.json", key.sign(t, release.File("checksums.txt")))
		tamper(release)
		f.env.Runner = osrun.New()
		f.env.Environ = []string{"PATH=" + filepath.Dir(tool.path), "HOME=" + tool.home}
		f.env.Releases = release.Fetcher()
		f.env.OS, f.env.Arch = runtime.GOOS, runtime.GOARCH
		return f, f.pin(commands.TeardownPinOptions{
			Release: version, Yes: true, Source: release.Source(),
			Policy: func(string) releasefetch.Policy { return keyPolicy{key.pub} },
			Now:    func() time.Time { return teardownTime },
		})
	}

	t.Run("a modified checksums.txt is a signature failure: do not trust it", func(t *testing.T) {
		f, err := pin(t, func(r *releasetest.Release) {
			r.Set("checksums.txt", append(r.File("checksums.txt"), []byte("# edited\n")...))
		})
		if err == nil || !strings.Contains(err.Error(), "did not verify against the release workflow's signature") || !strings.Contains(err.Error(), "Do not trust this release. Nothing was pinned.") || strings.Contains(err.Error(), "--file") {
			t.Fatalf("err = %v", err)
		}
		if _, found, _ := teardown.ReadPin(f.control); found {
			t.Fatal("pinned")
		}
	})

	t.Run("an archive that is not the one checksums.txt lists is refused after the signature checks out", func(t *testing.T) {
		f, err := pin(t, func(r *releasetest.Release) { r.Set(archive, append(r.File(archive), 0)) })
		if err == nil || !strings.Contains(err.Error(), "Refusing to pin "+version+": "+archive+" has sha256 ") || !strings.Contains(err.Error(), ", but the verified checksums.txt lists ") || !strings.HasSuffix(err.Error(), "Nothing was pinned.") {
			t.Fatalf("err = %v", err)
		}
		if _, found, _ := teardown.ReadPin(f.control); found {
			t.Fatal("pinned")
		}
	})
}
