package commands_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/binstore"
	"github.com/sofired/treerig/internal/checkoutindex/card"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/registry"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/releasefetch"
	"github.com/sofired/treerig/internal/releasefetch/releasetest"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/run/fakerun"
	"github.com/sofired/treerig/internal/teardown"
	"github.com/sofired/treerig/internal/teardown/teardowntest"
)

// releaseFixture is a teardownFixture plus a release served over TLS and a
// cosign that is a fake: a file on PATH that a fakerun.Runner answers for. The
// fake models a signature over (tag, the digest of checksums.txt) and accepts
// verify-blob only for that identity and those bytes.
type releaseFixture struct {
	*teardownFixture
	release *releasetest.Release
	runner  *fakerun.Runner
	// cosign is the file on PATH; cosignDir is its directory.
	cosign, cosignDir string
	// signedTag and signedSum are what the fake signature covers.
	signedTag, signedSum string
	// version is what the fake cosign reports.
	version string
}

// newReleaseFixture serves release version and signs it, with cosign 3.0.6.
func newReleaseFixture(t *testing.T, version string) *releaseFixture {
	t.Helper()
	f := &releaseFixture{teardownFixture: newTeardownFixture(t), version: "v3.0.6"}
	f.release = releasetest.New(t, version)
	f.signedTag, f.signedSum = version, releasetest.Sum(f.release.File("checksums.txt"))
	f.cosignDir = filepath.Join(filepath.Dir(f.dir), "tools")
	f.cosign = filepath.Join(f.cosignDir, "cosign")
	if err := os.MkdirAll(f.cosignDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.cosign, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.runner = &fakerun.Runner{Handler: f.cosignHandler}
	f.env.Runner = f.runner
	f.env.Environ = []string{"PATH=" + f.cosignDir, "HOME=" + f.home}
	f.env.Releases = f.release.Fetcher()
	return f
}

// cosignHandler is the fake cosign.
func (f *releaseFixture) cosignHandler(_ context.Context, spec run.Spec) (run.Result, error) {
	switch {
	case len(spec.Args) > 0 && spec.Args[0] == "version":
		return run.Result{Stdout: `{"gitVersion": "` + f.version + `"}`}, nil
	case len(spec.Args) > 0 && spec.Args[0] == "verify-blob":
		var identity, blob string
		for i, arg := range spec.Args {
			if arg == "--certificate-identity" {
				identity = spec.Args[i+1]
			}
		}
		blob = spec.Args[len(spec.Args)-1]
		data, err := os.ReadFile(blob)
		if err != nil {
			return run.Result{}, err
		}
		if !strings.HasSuffix(identity, "@refs/tags/v"+f.signedTag) {
			return run.Result{ExitCode: 1, Stderr: "Error: none of the expected identities matched what was in the certificate, got subjects [https://github.com/sofired/treerig/.github/workflows/release.yml@refs/tags/v" + f.signedTag + "]\n"}, nil
		}
		if releasetest.Sum(data) != f.signedSum {
			return run.Result{ExitCode: 1, Stderr: "Error: invalid signature when validating ASN.1 encoded signature\n"}, nil
		}
		return run.Result{Stderr: "Verified OK\n"}, nil
	}
	return run.Result{ExitCode: 2, Stderr: "unexpected cosign call"}, nil
}

// options are the flags of `teardown pin --release <version>`.
func (f *releaseFixture) options() commands.TeardownPinOptions {
	return commands.TeardownPinOptions{Release: f.release.Version, Yes: true, Now: func() time.Time { return teardownTime }, Source: f.release.Source()}
}

// cosignCalls are the cosign runs, as "args" lines.
func (f *releaseFixture) cosignCalls() []string {
	var calls []string
	for _, spec := range f.runner.Calls() {
		calls = append(calls, spec.Args[0])
	}
	return calls
}

// wantNothingPinned asserts that there is no pin and no stored binary. The
// teardown directory may exist, holding cosign's own cache directory.
func (f *releaseFixture) wantNothingPinned(t *testing.T) {
	t.Helper()
	if _, found, err := teardown.ReadPin(f.control); err != nil || found {
		t.Fatalf("ReadPin = %v, %v, want no pin", found, err)
	}
	entries, err := os.ReadDir(f.control.TeardownDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "cosign" {
			t.Fatalf("teardown directory holds %s, want only cosign's cache", entry.Name())
		}
	}
}

// publish replaces the platform archive served for linux_amd64 with archive,
// and lists its digest in a checksums.txt the fake cosign signs.
func (f *releaseFixture) publish(archive []byte) {
	f.release.Set(releasefetch.ArchiveName(f.release.Version, "linux_amd64"), archive)
	f.release.UpdateChecksums()
	f.signedSum = releasetest.Sum(f.release.File("checksums.txt"))
}

func TestTeardownPinRelease(t *testing.T) {
	t.Run("downloads, verifies and pins a release: four archive digests, this platform's binary, the signer and the source", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		if err := f.pin(f.options()); err != nil {
			t.Fatal(err)
		}
		pin := f.readPin(t)
		binary := f.release.Binaries["linux_amd64"]
		checksums := f.release.File("checksums.txt")
		if pin.Release != "1.2.3" || pin.Source != "https://127.0.0.1:"+portOf(f.release.Server.URL)+"/releases/download/v1.2.3/" ||
			pin.Signer != "https://github.com/sofired/treerig/.github/workflows/release.yml@refs/tags/v1.2.3" ||
			pin.ChecksumsSHA256 != releasetest.Sum(checksums) || pin.PinnedAt != "2026-10-14T12:00:00Z" {
			t.Fatalf("pin = %+v", pin)
		}
		if err := pin.Validate(); err != nil {
			t.Fatalf("the pin does not validate: %v", err)
		}
		for _, platform := range releasefetch.Platforms {
			entry := pin.Platforms[platform]
			wantArchive := releasetest.Sum(f.release.File(releasefetch.ArchiveName("1.2.3", platform)))
			wantBinary := ""
			if platform == "linux_amd64" {
				wantBinary = binstore.Hash(binary)
			}
			if entry.ArchiveSHA256 != wantArchive || entry.BinarySHA256 != wantBinary {
				t.Fatalf("%s = %+v, want archive %s binary %q", platform, entry, wantArchive, wantBinary)
			}
		}
		stored, err := os.ReadFile(f.stored(binstore.Hash(binary)))
		if err != nil || !bytes.Equal(stored, binary) {
			t.Fatalf("stored = %d bytes, %v", len(stored), err)
		}
		if f.printed() != "" {
			t.Fatalf("stdout = %q", f.printed())
		}
		want := "info: Downloading " + f.release.Base() + "checksums.txt…\n" +
			"info: Downloading " + f.release.Base() + "checksums.txt.sigstore.json…\n" +
			"info: Downloading " + f.release.Base() + "treerig_1.2.3_linux_amd64.tar.gz…\n" +
			"info: Pinned the teardown binary: release 1.2.3 (linux_amd64), stored at " + f.stored(binstore.Hash(binary)) + "."
		if f.logged() != want {
			t.Fatalf("log = %q\nwant  %q", f.logged(), want)
		}
		// Only this platform's archive is fetched, after the signature was checked.
		wantRequests := "/releases/download/v1.2.3/checksums.txt,/releases/download/v1.2.3/checksums.txt.sigstore.json,/releases/download/v1.2.3/treerig_1.2.3_linux_amd64.tar.gz"
		if got := strings.Join(f.release.Requests(), ","); got != wantRequests {
			t.Fatalf("requests = %s\nwant      %s", got, wantRequests)
		}
		if got := strings.Join(f.cosignCalls(), ","); got != "version,verify-blob" {
			t.Fatalf("cosign calls = %s", got)
		}
	})

	t.Run("pins the snapshot's version, which has a pre-release part and a build part", func(t *testing.T) {
		f := newReleaseFixture(t, "0.0.0-dev+0123456789ab")
		if err := f.pin(f.options()); err != nil {
			t.Fatal(err)
		}
		if got := f.readPin(t); got.Release != "0.0.0-dev+0123456789ab" || !strings.HasSuffix(got.Source, "/releases/download/v0.0.0-dev+0123456789ab/") {
			t.Fatalf("pin = %+v", got)
		}
	})

	t.Run("pins the platform of the machine it runs on", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.env.OS, f.env.Arch = "darwin", "arm64"
		if err := f.pin(f.options()); err != nil {
			t.Fatal(err)
		}
		pin := f.readPin(t)
		if pin.Platforms["darwin_arm64"].BinarySHA256 != binstore.Hash(f.release.Binaries["darwin_arm64"]) || pin.Platforms["linux_amd64"].BinarySHA256 != "" {
			t.Fatalf("platforms = %+v", pin.Platforms)
		}
		if !strings.HasSuffix(f.release.Requests()[2], "treerig_1.2.3_darwin_arm64.tar.gz") {
			t.Fatalf("requests = %v", f.release.Requests())
		}
	})

	t.Run("runs cosign as resolved, with the contract's argv, timeouts and exactly the allowlisted environment", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.env.Environ = append(f.env.Environ,
			"LANG=en_GB.UTF-8", "LC_ALL=C", "HTTPS_PROXY=http://proxy:3128", "https_proxy=http://proxy:3128", "NO_PROXY=localhost", "SSL_CERT_FILE=/etc/ssl/ca.pem", "SSL_CERT_DIR=/etc/ssl/certs",
			"SIGSTORE_ROOT_FILE=/evil", "SIGSTORE_REKOR_PUBLIC_KEY=/evil", "SIGSTORE_ID_TOKEN=t", "TUF_ROOT=/evil/tuf", "TMPDIR=/evil/tmp",
			"COSIGN_YES=true", "COSIGN_TRUSTED_ROOT=/evil/trusted_root.json", "COSIGN_INSECURE_IGNORE_TLOG=true", "ALL_PROXY=http://other", "XDG_CONFIG_HOME=/evil")
		if err := f.pin(f.options()); err != nil {
			t.Fatal(err)
		}
		calls := f.runner.Calls()
		if len(calls) != 2 || calls[0].Command != f.cosign || calls[1].Command != f.cosign {
			t.Fatalf("calls = %v", f.runner.Argv())
		}
		if strings.Join(calls[0].Args, " ") != "version --json" || calls[0].Timeout != 10*time.Second {
			t.Fatalf("version call = %+v", calls[0])
		}
		verify := calls[1]
		dir := verify.Dir
		wantArgs := "verify-blob --bundle " + dir + "/checksums.txt.sigstore.json --certificate-identity https://github.com/sofired/treerig/.github/workflows/release.yml@refs/tags/v1.2.3 --certificate-oidc-issuer https://token.actions.githubusercontent.com " + dir + "/checksums.txt"
		if strings.Join(verify.Args, " ") != wantArgs || verify.Timeout != 60*time.Second {
			t.Fatalf("verify call = %+v", verify)
		}
		if filepath.Dir(dir) != f.control.CosignTmp {
			t.Fatalf("verify ran in %s, want a directory of %s", dir, f.control.CosignTmp)
		}
		// Both calls, not only the first: the exact slice, so an empty or nil Env cannot pass.
		want := []string{
			"PATH=" + f.cosignDir, "LANG=en_GB.UTF-8", "LC_ALL=C",
			"HTTPS_PROXY=http://proxy:3128", "https_proxy=http://proxy:3128", "NO_PROXY=localhost",
			"SSL_CERT_FILE=/etc/ssl/ca.pem", "SSL_CERT_DIR=/etc/ssl/certs",
			"HOME=" + f.control.CosignHome, "TUF_ROOT=" + f.control.CosignTUFRoot, "TMPDIR=" + f.control.CosignTmp,
		}
		for _, call := range calls {
			if got := strings.Join(call.Env, "\n"); got != strings.Join(want, "\n") {
				t.Fatalf("%s env =\n%s\nwant\n%s", call.Args[0], got, strings.Join(want, "\n"))
			}
		}
		for _, dir := range []string{f.control.CosignHome, f.control.CosignTUFRoot, f.control.CosignTmp} {
			if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("%s: %v, %v; want a 0700 directory", dir, info, err)
			}
		}
	})

	t.Run("--latest discovers the version, then fetches everything from the versioned URL", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.release.SetLatest("1.2.3")
		options := f.options()
		options.Release, options.Latest = "", true
		if err := f.pin(options); err != nil {
			t.Fatal(err)
		}
		want := "/releases/latest/download/checksums.txt,/releases/download/v1.2.3/checksums.txt,/releases/download/v1.2.3/checksums.txt.sigstore.json,/releases/download/v1.2.3/treerig_1.2.3_linux_amd64.tar.gz"
		if got := strings.Join(f.release.Requests(), ","); got != want {
			t.Fatalf("requests = %s\nwant      %s", got, want)
		}
		if got := f.readPin(t); got.Release != "1.2.3" || !strings.HasSuffix(got.Source, "/releases/download/v1.2.3/") {
			t.Fatalf("pin = %+v", got)
		}
	})

	t.Run("--latest is not fooled by a release published between discovery and the download", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.release.SetLatest("1.2.3")
		f.release.PublishAfterLatest("1.2.4") // from now on, "latest" names a release this server does not hold
		options := f.options()
		options.Release, options.Latest = "", true
		if err := f.pin(options); err != nil {
			t.Fatal(err)
		}
		if got := f.readPin(t); got.Release != "1.2.3" {
			t.Fatalf("pinned %s, want the release discovery named", got.Release)
		}
		latest := 0
		for _, path := range f.release.Requests() {
			if strings.Contains(path, "/latest/") {
				latest++
			} else if !strings.HasPrefix(path, "/releases/download/v1.2.3/") {
				t.Fatalf("a request left the discovered release: %s", path)
			}
		}
		if latest != 1 {
			t.Fatalf("latest was asked %d times", latest)
		}
	})

	t.Run("--latest with no release that is not a pre-release refuses and says how to pin a pre-release", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		options := f.options()
		options.Release, options.Latest = "", true
		wantRefusal(t, f.pin(options), "Refusing to pin the latest release: sofired/treerig has no release that is not a pre-release (or the repository is not reachable). Try `treerig teardown pin --latest` again later, or name a release with `treerig teardown pin --release <a version>`. Releases are listed at https://github.com/sofired/treerig/releases.")
		f.wantNothingPinned(t)
		if len(f.runner.Calls()) != 0 {
			t.Fatalf("cosign ran: %v", f.runner.Argv())
		}
	})

	t.Run("refuses a signature for another tag, and writes nothing", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.4")
		f.signedTag = "1.2.3" // a genuine signature, of the release before
		wantRefusal(t, f.pin(f.options()), "Refusing to pin 1.2.4: checksums.txt did not verify against the release workflow's signature. cosign said: \"Error: none of the expected identities matched what was in the certificate, got subjects [https://github.com/sofired/treerig/.github/workflows/release.yml@refs/tags/v1.2.3]\". Do not trust this release. Nothing was pinned.")
		f.wantNothingPinned(t)
		for _, path := range f.release.Requests() {
			if strings.HasSuffix(path, ".tar.gz") {
				t.Fatalf("the archive was fetched before the signature was verified: %s", path)
			}
		}
	})

	t.Run("refuses a checksums.txt that is not the bytes that were signed", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.release.Set("checksums.txt", append(f.release.File("checksums.txt"), []byte(strings.Repeat("a", 64)+"  extra\n")...))
		err := f.pin(f.options())
		if err == nil || !strings.Contains(err.Error(), `cosign said: "Error: invalid signature`) || !strings.Contains(err.Error(), "Do not trust this release.") || strings.Contains(err.Error(), "--file") {
			t.Fatalf("err = %v", err)
		}
		f.wantNothingPinned(t)
	})

	t.Run("refuses an archive whose digest is not the one in the verified checksums.txt", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		name := releasefetch.ArchiveName("1.2.3", "linux_amd64")
		tampered := releasetest.Archive(releasetest.Entry{Name: "treerig", Body: teardowntest.Executable("linux_amd64", " tampered")})
		listed := releasetest.Sum(f.release.File(name))
		f.release.Set(name, tampered) // the signed checksums.txt still lists the original
		err := f.pin(f.options())
		wantRefusal(t, err, "Refusing to pin 1.2.3: "+name+" has sha256 "+releasetest.Sum(tampered)+", but the verified checksums.txt lists "+listed+". Nothing was pinned.")
		f.wantNothingPinned(t)
	})

	t.Run("refuses when cosign is missing, or older than 3.0.0, or a development build", func(t *testing.T) {
		missing := newReleaseFixture(t, "1.2.3")
		missing.env.Environ = []string{"PATH=" + filepath.Join(missing.dir, "nowhere")}
		wantRefusal(t, missing.pin(missing.options()), "Refusing to pin 1.2.3: cosign 3.0.0 or newer is required to verify it, and cosign is not on PATH. Install cosign, or verify the download by hand and use `treerig teardown pin --file PATH --sha256 HEX --release LABEL`. Nothing was pinned.")
		missing.wantNothingPinned(t)
		if len(missing.release.Requests()) != 0 {
			t.Fatalf("a request was made before cosign was found: %v", missing.release.Requests())
		}

		for version, tc := range map[string][2]string{"v2.6.5": {"version v2.6.5", "Upgrade cosign"}, "devel": {"version devel", "Upgrade cosign"}, "": {"it reported no gitVersion", "Install cosign"}, "v3.0.0-rc.1": {"version v3.0.0-rc.1", "Upgrade cosign"}} {
			old := newReleaseFixture(t, "1.2.3")
			old.version = version
			wantRefusal(t, old.pin(old.options()), "Refusing to pin 1.2.3: cosign 3.0.0 or newer is required to verify it, and "+old.cosign+" does not qualify ("+tc[0]+"). "+tc[1]+", or verify the download by hand and use `treerig teardown pin --file PATH --sha256 HEX --release LABEL`. Nothing was pinned.")
			old.wantNothingPinned(t)
		}
	})

	t.Run("refuses a release that is not there, naming the URL", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.release.Status("checksums.txt", 404)
		err := f.pin(f.options())
		wantRefusal(t, err, "Refusing to pin 1.2.3: "+f.release.Base()+"checksums.txt was not found (HTTP 404). Nothing was pinned. Check that the release exists and has these files.")
		f.wantNothingPinned(t)
		if len(f.runner.Calls()) != 1 {
			t.Fatalf("cosign calls = %v, want only the version check", f.runner.Argv())
		}
	})

	t.Run("refuses a missing bundle, a missing archive and a server error", func(t *testing.T) {
		for _, tc := range []struct {
			file   string
			status int
			text   string
		}{
			{"checksums.txt.sigstore.json", 404, "was not found (HTTP 404)"},
			{"treerig_1.2.3_linux_amd64.tar.gz", 404, "was not found (HTTP 404)"},
			{"checksums.txt", 500, "answered HTTP 500"},
		} {
			f := newReleaseFixture(t, "1.2.3")
			f.release.Status(tc.file, tc.status)
			err := f.pin(f.options())
			if err == nil || !strings.Contains(err.Error(), f.release.Base()+tc.file+" "+tc.text) || !strings.Contains(err.Error(), "Nothing was pinned.") {
				t.Fatalf("%s: err = %v", tc.file, err)
			}
			f.wantNothingPinned(t)
		}
	})

	t.Run("refuses a redirect to plain http, and never reaches it", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.release.Redirect("checksums.txt", "http://127.0.0.1:1/checksums.txt")
		err := f.pin(f.options())
		if err == nil || !strings.Contains(err.Error(), "only https is allowed") || !strings.HasSuffix(err.Error(), "Nothing was pinned. Check your network connection and HTTPS_PROXY settings, then run it again.") {
			t.Fatalf("err = %v", err)
		}
		f.wantNothingPinned(t)
	})

	t.Run("refuses files past their size caps", func(t *testing.T) {
		for _, tc := range []struct {
			file string
			size int
			cap  string
		}{
			{"checksums.txt", 1<<20 + 1, "1 MiB"},
			{"checksums.txt.sigstore.json", 1<<20 + 1, "1 MiB"},
			{"treerig_1.2.3_linux_amd64.tar.gz", 100<<20 + 1, "100 MiB"},
		} {
			f := newReleaseFixture(t, "1.2.3")
			f.release.Set(tc.file, bytes.Repeat([]byte("z"), tc.size))
			if tc.file != "treerig_1.2.3_linux_amd64.tar.gz" {
				f.signedSum = releasetest.Sum(f.release.File("checksums.txt"))
			}
			wantRefusal(t, f.pin(f.options()), "Refusing to pin 1.2.3: "+f.release.Base()+tc.file+" is larger than "+tc.cap+". Nothing was pinned.")
			f.wantNothingPinned(t)
		}
	})

	t.Run("refuses a hostile archive of every kind, even one the signed checksums.txt lists", func(t *testing.T) {
		name := releasefetch.ArchiveName("1.2.3", "linux_amd64")
		good := releasetest.Entry{Name: "treerig", Mode: 0o755, Body: teardowntest.Executable("linux_amd64", " from a hostile archive")}
		for reason, entries := range map[string][]releasetest.Entry{
			"has a link entry":                                       {good, {Name: "x", Type: tar.TypeSymlink, Linkname: "/etc/passwd"}},
			"has a link entry ":                                      {{Name: "treerig", Type: tar.TypeSymlink, Linkname: "/bin/sh"}},
			"has an entry with an absolute name":                     {good, {Name: "/etc/cron.d/x", Body: []byte("x")}},
			"has an entry with a `..` in its name":                   {good, {Name: "../../.bashrc", Body: []byte("x")}},
			"has more than one `treerig` entry":                      {good, {Name: "./treerig", Body: []byte("other")}},
			"has no `treerig` entry at its root":                     {{Name: "LICENSE", Body: []byte("x")}},
			"has an entry that is not a regular file or a directory": {good, {Name: "dev", Type: tar.TypeChar}},
		} {
			f := newReleaseFixture(t, "1.2.3")
			f.publish(releasetest.Archive(entries...))
			wantRefusal(t, f.pin(f.options()), "Refusing to pin 1.2.3: the archive "+name+" "+strings.TrimSpace(reason)+". Nothing was pinned.")
			f.wantNothingPinned(t)
		}
	})

	t.Run("uses the documented extraction bounds by default", func(t *testing.T) {
		if got := commands.ReleaseLimitsForTest(); got != releasefetch.DefaultLimits || got.Expanded != 500<<20 || got.Binary != 200<<20 {
			t.Fatalf("limits = %+v", got)
		}
	})

	t.Run("refuses a decompression bomb past the bound", func(t *testing.T) {
		// The production bound is 500 MiB (the subtest above pins it); a
		// small one proves the same refusal without expanding half a gigabyte,
		// which takes over a minute under the race detector.
		defer commands.SetReleaseLimitsForTest(releasefetch.Limits{Expanded: 1 << 20, Binary: releasefetch.DefaultLimits.Binary})()
		f := newReleaseFixture(t, "1.2.3")
		name := releasefetch.ArchiveName("1.2.3", "linux_amd64")
		f.publish(releasetest.Archive(
			releasetest.Entry{Name: "padding", Body: make([]byte, 2<<20)},
			releasetest.Entry{Name: "treerig", Body: teardowntest.Executable("linux_amd64", "")},
		))
		wantRefusal(t, f.pin(f.options()), "Refusing to pin 1.2.3: the archive "+name+" expands to more than 1 MiB. Nothing was pinned.")
		f.wantNothingPinned(t)
	})

	t.Run("refuses a treerig that is not an executable for this platform", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		name := releasefetch.ArchiveName("1.2.3", "linux_amd64")
		f.publish(releasetest.Archive(releasetest.Entry{Name: "treerig", Body: teardowntest.Executable("linux_arm64", "")}))
		wantRefusal(t, f.pin(f.options()), "Refusing to pin 1.2.3: the treerig in "+name+" is not a linux_amd64 executable (an ELF file for EM_AARCH64, not EM_X86_64). Nothing was pinned.")
		f.wantNothingPinned(t)
	})

	t.Run("refuses a verified checksums.txt that does not list all four archives", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		short := "" + releasetest.Sum(nil) + "  " + releasefetch.ArchiveName("1.2.3", "linux_amd64") + "\n"
		f.release.Set("checksums.txt", []byte(short))
		f.signedSum = releasetest.Sum([]byte(short))
		wantRefusal(t, f.pin(f.options()), "Refusing to pin 1.2.3: the verified checksums.txt does not list treerig_1.2.3_linux_arm64.tar.gz. Nothing was pinned.")
		f.wantNothingPinned(t)
	})

	t.Run("refuses a download this process cannot make", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.env.Releases = nil
		wantRefusal(t, f.pin(f.options()), "Refusing to pin 1.2.3: this process has no way to download a release. Nothing was pinned.")
	})

	t.Run("refuses a platform treerig does not build", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.env.Arch = "riscv64"
		wantRefusal(t, f.pin(f.options()), "Refusing to pin 1.2.3: linux_riscv64 is not a platform treerig builds (linux and darwin, on amd64 and arm64).")
	})

	t.Run("a newer or foreign pin refuses before anything is downloaded", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		if err := os.MkdirAll(f.control.TeardownDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.control.PinPath, []byte(`{"version": 2}`), 0o600); err != nil {
			t.Fatal(err)
		}
		err := f.pin(f.options())
		if err == nil || !strings.Contains(err.Error(), "Upgrade treerig") {
			t.Fatalf("err = %v", err)
		}
		if err := os.WriteFile(f.control.PinPath, []byte(`{"nonsense": true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := f.pin(f.options()); !errors.Is(err, teardown.ErrForeign) {
			t.Fatalf("err = %v", err)
		}
		if len(f.release.Requests()) != 0 || len(f.runner.Calls()) != 0 {
			t.Fatalf("requests %v, cosign %v", f.release.Requests(), f.runner.Argv())
		}
	})

	t.Run("a test policy supplies the argv and the signer", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		options := f.options()
		options.Policy = func(release string) releasefetch.Policy { return testPolicy{release} }
		f.signedTag = "test-signer-1.2.3" // so the fake accepts its identity
		f.runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
			if spec.Args[0] == "verify-blob" && spec.Args[1] == "--only-this" {
				return run.Result{}, nil
			}
			return f.cosignHandler(ctx, spec)
		}
		if err := f.pin(options); err != nil {
			t.Fatal(err)
		}
		if f.readPin(t).Signer != "test-signer-1.2.3" {
			t.Fatalf("signer = %q", f.readPin(t).Signer)
		}
	})

	t.Run("gives up when the downloads outlast their timeout, and says so", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.env.Releases = blockingFetcher{}
		options := f.options()
		options.DownloadTimeout = 50 * time.Millisecond
		wantRefusal(t, f.pin(options), "Refusing to pin 1.2.3: the download and verification did not finish within 50ms. Nothing was pinned. Check your network connection and HTTPS_PROXY settings, then run it again.")
		f.wantNothingPinned(t)
	})

	t.Run("stops at once when the command is cancelled", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.env.Releases = blockingFetcher{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := commands.TeardownPin(ctx, f.env, f.options()); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

// testPolicy is a policy a test supplies.
type testPolicy struct{ release string }

// Signer names the release in the test identity.
func (p testPolicy) Signer() string { return "test-signer-" + p.release }

// Args is an argv of the test's own.
func (p testPolicy) Args(bundle, blob string) []string {
	return []string{"--only-this", bundle, blob}
}

// blockingFetcher never answers: it returns when its context ends.
type blockingFetcher struct{}

// Get waits for the context and returns its error.
func (blockingFetcher) Get(ctx context.Context, _ string, _ int64) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// portOf is the port of an http://host:port URL.
func portOf(url string) string { return url[strings.LastIndex(url, ":")+1:] }

func TestTeardownPinReleasePlan(t *testing.T) {
	t.Run("shows the plan after verification, and pins on the phrase", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.terminal("pin teardown binary\n")
		options := f.options()
		options.Yes = false
		if err := f.pin(options); err != nil {
			t.Fatal(err)
		}
		binary := binstore.Hash(f.release.Binaries["linux_amd64"])
		want := "Pin the teardown binary\n" +
			"  release   1.2.3  (current pin: none)\n" +
			"  source    " + f.release.Base() + "\n" +
			"  signer    https://github.com/sofired/treerig/.github/workflows/release.yml@refs/tags/v1.2.3  (cosign verified)\n" +
			"  cosign    " + f.cosign + "  (3.0.6)\n" +
			"  platform  linux_amd64  sha256 " + binary + "\n" +
			"  `worktree remove` will run this binary instead of the build you invoke.\n\n" +
			"Type this exact phrase to continue:\n\n  pin teardown binary\n\n> "
		if f.stderr.String() != want {
			t.Fatalf("plan:\n%s\nwant:\n%s", f.stderr.String(), want)
		}
		if f.readPin(t).Release != "1.2.3" {
			t.Fatal("not pinned")
		}
	})

	t.Run("shows both cosign paths when PATH reaches it through a link", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		shims := filepath.Join(filepath.Dir(f.dir), "shims")
		if err := os.MkdirAll(shims, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(f.cosign, filepath.Join(shims, "cosign")); err != nil {
			t.Fatal(err)
		}
		f.env.Environ = []string{"PATH=" + shims}
		f.terminal("pin teardown binary\n")
		options := f.options()
		options.Yes = false
		if err := f.pin(options); err != nil {
			t.Fatal(err)
		}
		canonical, _ := filepath.EvalSymlinks(f.cosign)
		if want := "  cosign    " + filepath.Join(shims, "cosign") + " -> " + canonical + "  (3.0.6)\n"; !strings.Contains(f.stderr.String(), want) {
			t.Fatalf("plan:\n%s\nwant a line %q", f.stderr.String(), want)
		}
		if f.runner.Calls()[0].Command != filepath.Join(shims, "cosign") {
			t.Fatalf("ran %s, want the PATH-resolved path", f.runner.Calls()[0].Command)
		}
	})

	t.Run("a wrong phrase cancels after the download, and nothing is written", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.terminal("yes\n")
		options := f.options()
		options.Yes = false
		wantRefusal(t, f.pin(options), "Confirmation did not match; operation cancelled.")
		f.wantNothingPinned(t)
	})

	t.Run("without a terminal and without --yes it refuses to ask", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		options := f.options()
		options.Yes = false
		wantRefusal(t, f.pin(options), `Interactive confirmation required: "pin teardown binary".`)
		f.wantNothingPinned(t)
	})

	t.Run("names the release it replaces, and a downgrade", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.releasePin(t, "1.3.0", "a newer release")
		f.terminal("pin teardown binary\n")
		options := f.options()
		options.Yes = false
		if err := f.pin(options); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(f.stderr.String(), "release   1.2.3  (current pin: 1.3.0; this is older)") {
			t.Fatalf("plan:\n%s", f.stderr.String())
		}
		if got := f.readPin(t); got.Release != "1.2.3" || len(got.BinaryHashes()) != 1 {
			t.Fatalf("pin = %+v", got)
		}
	})

	t.Run("quotes the control characters in what the network told it", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.release.Status("checksums.txt", 404)
		err := f.pin(f.options())
		if err == nil || strings.ContainsAny(err.Error(), "\x1b\r") {
			t.Fatalf("err = %q", err)
		}
	})
}

func TestTeardownPinReleaseUpdate(t *testing.T) {
	t.Run("the same release from another platform merges: both binaries, one pin", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		if err := f.pin(f.options()); err != nil {
			t.Fatal(err)
		}
		f.env.Arch = "arm64"
		later := f.options()
		later.Now = func() time.Time { return teardownTime.Add(time.Hour) }
		if err := f.pin(later); err != nil {
			t.Fatal(err)
		}
		pin := f.readPin(t)
		if pin.Platforms["linux_amd64"].BinarySHA256 != binstore.Hash(f.release.Binaries["linux_amd64"]) ||
			pin.Platforms["linux_arm64"].BinarySHA256 != binstore.Hash(f.release.Binaries["linux_arm64"]) {
			t.Fatalf("platforms = %+v", pin.Platforms)
		}
		if pin.PinnedAt != teardown.FormatTime(teardownTime.Add(time.Hour)) {
			t.Fatalf("pinned_at = %s", pin.PinnedAt)
		}
		if _, err := os.Stat(f.stored(binstore.Hash(f.release.Binaries["linux_amd64"]))); err != nil {
			t.Fatalf("the first platform's binary was swept: %v", err)
		}
	})

	t.Run("another release replaces the pin", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.releasePin(t, "1.1.0", "an older release")
		if err := f.pin(f.options()); err != nil {
			t.Fatal(err)
		}
		if pin := f.readPin(t); pin.Release != "1.2.3" || len(pin.BinaryHashes()) != 1 {
			t.Fatalf("pin = %+v", pin)
		}
	})

	t.Run("a release replaces a file pin, and a file pin replaces a release", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		if err := f.pin(f.pinOptions()); err != nil {
			t.Fatal(err)
		}
		if err := f.pin(f.options()); err != nil {
			t.Fatal(err)
		}
		if pin := f.readPin(t); pin.IsFile() || pin.Release != "1.2.3" {
			t.Fatalf("pin = %+v", pin)
		}
		if err := f.pin(f.pinOptions()); err != nil {
			t.Fatal(err)
		}
		if pin := f.readPin(t); !pin.IsFile() {
			t.Fatalf("pin = %+v", pin)
		}
	})
}

func TestTeardownPinReleaseContainment(t *testing.T) {
	// install puts a cosign at dir/cosign and makes it the one on PATH.
	install := func(t *testing.T, f *releaseFixture, dir string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "cosign")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		f.cosign = path
		f.env.Environ = []string{"PATH=" + dir}
		return path
	}
	refusal := func(release, path, canonical, where string) string {
		shown := path
		if canonical != path {
			shown += " (" + canonical + ")"
		}
		return "Refusing to pin " + release + ": cosign resolves to " + shown + ", which is inside " + where + ", where branch code could have put it. Install cosign outside any checkout, or verify the download by hand and use `treerig teardown pin --file PATH --sha256 HEX --release LABEL`. Nothing was pinned."
	}
	canonical := func(t *testing.T, path string) string {
		t.Helper()
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}

	t.Run("refuses a cosign inside the current checkout, running nothing and fetching nothing", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		checkout := filepath.Join(filepath.Dir(f.dir), "checkout")
		if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		path := install(t, f, filepath.Join(checkout, "node_modules", ".bin"))
		f.env.Cwd = filepath.Join(checkout, "src")
		if err := os.MkdirAll(f.env.Cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		wantRefusal(t, f.pin(f.options()), refusal("1.2.3", path, canonical(t, path), "this checkout"))
		if len(f.runner.Calls()) != 0 || len(f.release.Requests()) != 0 {
			t.Fatalf("cosign %v, requests %v", f.runner.Argv(), f.release.Requests())
		}
		f.wantNothingPinned(t)
	})

	t.Run("refuses a cosign in the repository of a linked worktree, which holds the sibling worktrees", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		parent := filepath.Dir(f.dir)
		repository := filepath.Join(parent, "repo")
		linked := filepath.Join(parent, "repo.feature")
		gitDir := filepath.Join(repository, ".git", "worktrees", "feature")
		for _, dir := range []string{gitDir, linked} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		path := install(t, f, filepath.Join(repository, "tools"))
		f.env.Cwd = linked
		wantRefusal(t, f.pin(f.options()), refusal("1.2.3", path, canonical(t, path), "the repository of this checkout"))
	})

	t.Run("a cosign beside the checkout, in a directory that shares its name as a prefix, is not inside it", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		parent := filepath.Dir(f.dir)
		checkout := filepath.Join(parent, "treerig")
		if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		install(t, f, filepath.Join(parent, "treerig.tools"))
		f.env.Cwd = checkout
		if err := f.pin(f.options()); err != nil {
			t.Fatalf("treerig.tools was taken to be inside treerig: %v", err)
		}
	})

	t.Run("refuses a cosign inside the control home", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		registrytest.ClaimControlHome(t, f.control.ControlHome)
		path := install(t, f, filepath.Join(f.control.ControlHome, "bin"))
		wantRefusal(t, f.pin(f.options()), refusal("1.2.3", path, canonical(t, path), "the control home"))
	})

	t.Run("refuses a cosign inside a registered worktree, named by its slug", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		registrytest.ClaimControlHome(t, f.control.ControlHome)
		worktree := filepath.Join(filepath.Dir(f.dir), "elsewhere", "feature-login")
		path := install(t, f, filepath.Join(worktree, "bin"))
		const repositoryID, worktreeID = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
		if _, err := registrytest.WriteRecord(f.control.ControlHome, registrytest.RecordJSON{
			Version: 1, RepositoryID: repositoryID, WorktreeID: worktreeID, Slug: "feature-login", Domain: "acme.test", Initialization: string(registry.Complete),
		}); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(layout.WorktreeLayout(f.control.ControlHome, repositoryID, worktreeID).Home, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := card.WriteIndexCard(f.control.ControlHome, repositoryID, worktreeID, card.IndexCard{
			Project: "p", Domain: "acme.test", Slug: "feature-login", Host: "feature-login.acme.test", Root: worktree, Branch: "main",
			LabelNamespace: "dev.treerig", UpdatedAt: "2026-01-01T00:00:00.000Z",
		}); err != nil {
			t.Fatal(err)
		}
		wantRefusal(t, f.pin(f.options()), refusal("1.2.3", path, canonical(t, path), "the worktree feature-login"))
	})

	t.Run("an unreadable registry is skipped with a note, not a refusal", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("permission bits do not stop root")
		}
		f := newReleaseFixture(t, "1.2.3")
		registrytest.ClaimControlHome(t, f.control.ControlHome)
		repositories := layout.ControlLayout(f.control.ControlHome).RepositoriesDir
		if err := os.MkdirAll(repositories, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(repositories, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(repositories, 0o700) })
		if err := f.pin(f.options()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(f.logged(), "warn: Could not read the registry (") || !strings.Contains(f.logged(), "so registered worktrees are not checked for cosign.") {
			t.Fatalf("log = %q", f.logged())
		}
	})
}

func TestTeardownPinReleaseCosignFailures(t *testing.T) {
	t.Run("a shim that needs the user's HOME is refused with the reason, and the file form", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.runner.Handler = func(_ context.Context, spec run.Spec) (run.Result, error) {
			return run.Result{ExitCode: 1, Stderr: "asdf: No version is set for command cosign"}, nil
		}
		wantRefusal(t, f.pin(f.options()), "Refusing to pin 1.2.3: cosign 3.0.0 or newer is required to verify it, and "+f.cosign+" does not qualify (`cosign version --json` failed: asdf: No version is set for command cosign; a version-manager shim such as asdf or mise needs an environment treerig withholds, so install a cosign binary on PATH). Install cosign, or verify the download by hand and use `treerig teardown pin --file PATH --sha256 HEX --release LABEL`. Nothing was pinned.")
		f.wantNothingPinned(t)
	})

	t.Run("a refusal that ran nothing leaves no cosign directories behind", func(t *testing.T) {
		missing := newReleaseFixture(t, "1.2.3")
		missing.env.Environ = []string{"PATH=" + filepath.Join(missing.dir, "nowhere")}
		if err := missing.pin(missing.options()); err == nil {
			t.Fatal("pinned")
		}
		old := newReleaseFixture(t, "1.2.3")
		old.version = "v2.6.5"
		if err := old.pin(old.options()); err == nil {
			t.Fatal("pinned")
		}
		for _, f := range []*releaseFixture{missing, old} {
			if _, err := os.Lstat(f.control.TeardownDir); !os.IsNotExist(err) {
				t.Fatalf("teardown directory: %v, want none", err)
			}
		}
	})

	t.Run("a command cancelled while cosign reports its version returns the cancellation", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		ctx, cancel := context.WithCancel(context.Background())
		f.runner.Handler = func(_ context.Context, spec run.Spec) (run.Result, error) {
			cancel()
			return run.Result{ExitCode: 1, Stderr: "signal: killed"}, nil
		}
		if err := commands.TeardownPin(ctx, f.env, f.options()); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestTeardownPinReleaseVerifyFailures(t *testing.T) {
	fail := func(f *releaseFixture, result run.Result, err error) {
		f.runner.Handler = func(ctx context.Context, spec run.Spec) (run.Result, error) {
			if spec.Args[0] == "verify-blob" {
				return result, err
			}
			return f.cosignHandler(ctx, spec)
		}
	}
	const initialize = "`cosign initialize`"
	const fileForm = "`treerig teardown pin --file PATH --sha256 HEX --release LABEL`"

	t.Run("a trust root that could not be loaded may be fixed or bypassed by hand: initialize, or the file form", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		fail(f, run.Result{ExitCode: 1, Stderr: "WARNING: Could not fetch trusted_root.json from the TUF repository.\nError: trusted root is required when using new bundle format\n"}, nil)
		wantRefusal(t, f.pin(f.options()), "Refusing to pin 1.2.3: cosign could not load the Sigstore trust root. cosign said: \"WARNING: Could not fetch trusted_root.json from the TUF repository. Error: trusted root is required when using new bundle format\". Run "+initialize+" once with network access, or verify the download by hand and use "+fileForm+". Nothing was pinned.")
		f.wantNothingPinned(t)
	})

	t.Run("a signature or identity failure says not to trust the release, and never offers the file form", func(t *testing.T) {
		for _, stderr := range []string{
			"Error: none of the expected identities matched what was in the certificate\n",
			"Error: failed to verify signature: invalid signature\n",
			"WARNING: Could not fetch trusted_root.json from the TUF repository.\nError: failed to verify log inclusion: 0 < 1\n",
		} {
			f := newReleaseFixture(t, "1.2.3")
			fail(f, run.Result{ExitCode: 1, Stderr: stderr}, nil)
			err := f.pin(f.options())
			if err == nil || !strings.Contains(err.Error(), "Do not trust this release. Nothing was pinned.") || strings.Contains(err.Error(), "--file") || strings.Contains(err.Error(), "initialize") {
				t.Fatalf("%q: err = %v", stderr, err)
			}
		}
	})

	t.Run("a failure it cannot tell apart is quoted and gets no advice", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		fail(f, run.Result{ExitCode: 1, Stderr: "Error: invalid character 'j' looking for beginning of value\n"}, nil)
		wantRefusal(t, f.pin(f.options()), "Refusing to pin 1.2.3: cosign could not verify checksums.txt. cosign said: \"Error: invalid character 'j' looking for beginning of value\". Nothing was pinned.")
	})

	t.Run("a cosign that cannot start, and one that times out, point at initialize but not at the file form", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		fail(f, run.Result{}, &run.StartError{Command: f.cosign, Code: "ENOEXEC", Detail: "is not a valid executable (ENOEXEC).", ExitCode: 126})
		err := f.pin(f.options())
		if err == nil || !strings.Contains(err.Error(), "cosign could not be run: it is not a valid executable (ENOEXEC).") || !strings.Contains(err.Error(), initialize) || strings.Contains(err.Error(), "--file") || !strings.HasSuffix(err.Error(), "Nothing was pinned.") {
			t.Fatalf("start: err = %v", err)
		}
		fail(f, run.Result{ExitCode: 1, TimedOut: true}, nil)
		wantRefusal(t, f.pin(f.options()), "Refusing to pin 1.2.3: cosign did not finish within 60 seconds. If it is fetching its trust root on a slow connection, run "+initialize+" once with network access, then run this again. Nothing was pinned.")
	})

	t.Run("a cancelled or timed-out command returns that, not a cosign refusal", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		ctx, cancel := context.WithCancel(context.Background())
		f.runner.Handler = func(c context.Context, spec run.Spec) (run.Result, error) {
			if spec.Args[0] == "verify-blob" {
				cancel() // the user hits Ctrl-C while cosign runs, and cosign is killed
				return run.Result{ExitCode: 1, Stderr: "signal: killed"}, nil
			}
			return f.cosignHandler(c, spec)
		}
		if err := commands.TeardownPin(ctx, f.env, f.options()); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		f2 := newReleaseFixture(t, "1.2.3")
		f2.runner.Handler = func(c context.Context, spec run.Spec) (run.Result, error) {
			if spec.Args[0] == "verify-blob" {
				time.Sleep(80 * time.Millisecond) // past the download timeout
				return run.Result{ExitCode: 1, Stderr: "killed"}, nil
			}
			return f2.cosignHandler(c, spec)
		}
		options := f2.options()
		options.DownloadTimeout = 40 * time.Millisecond
		wantRefusal(t, f2.pin(options), "Refusing to pin 1.2.3: the download and verification did not finish within 40ms. Nothing was pinned. Check your network connection and HTTPS_PROXY settings, then run it again.")
	})

	t.Run("a network failure says what to check, and a timeout reads as minutes", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		f.env.Releases = failingFetcher{errors.New("dial tcp: no route to host")}
		wantRefusal(t, f.pin(f.options()), "Refusing to pin 1.2.3: dial tcp: no route to host. Nothing was pinned. Check your network connection and HTTPS_PROXY settings, then run it again.")
		f.env.Releases = failingFetcher{context.DeadlineExceeded}
		wantRefusal(t, f.pin(f.options()), "Refusing to pin 1.2.3: the download and verification did not finish within 10 minutes. Nothing was pinned. Check your network connection and HTTPS_PROXY settings, then run it again.")
	})
}

// failingFetcher fails every fetch with err.
type failingFetcher struct{ err error }

// Get returns the error.
func (f failingFetcher) Get(context.Context, string, int64) ([]byte, error) { return nil, f.err }

func TestTeardownPinReleaseRootDiscovery(t *testing.T) {
	// installAt puts a cosign at dir/cosign and makes it the one on PATH.
	installAt := func(t *testing.T, f *releaseFixture, dir string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "cosign")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		f.cosign = path
		f.env.Environ = []string{"PATH=" + dir}
		return path
	}

	t.Run("refuses, fetching nothing, when the current checkout's .git cannot be followed", func(t *testing.T) {
		for name, setup := range map[string]func(t *testing.T, checkout string){
			"a .git file that does not say gitdir": func(t *testing.T, checkout string) {
				if err := os.WriteFile(filepath.Join(checkout, ".git"), []byte("nonsense\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			"a commondir that cannot be read": func(t *testing.T, checkout string) {
				gitDir := filepath.Join(checkout, "gitdir")
				if err := os.MkdirAll(filepath.Join(gitDir, "commondir"), 0o755); err != nil { // a directory where a file should be
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(checkout, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			"a .git file that is a directory-sized lie": func(t *testing.T, checkout string) {
				if err := os.WriteFile(filepath.Join(checkout, ".git"), bytes.Repeat([]byte("x"), 5000), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		} {
			f := newReleaseFixture(t, "1.2.3")
			checkout := filepath.Join(filepath.Dir(f.dir), "checkout")
			if err := os.MkdirAll(checkout, 0o755); err != nil {
				t.Fatal(err)
			}
			setup(t, checkout)
			f.env.Cwd = checkout
			err := f.pin(f.options())
			if err == nil || !strings.HasPrefix(err.Error(), "Refusing to pin 1.2.3: could not work out where cosign must not live: the current checkout") || !strings.HasSuffix(err.Error(), "Nothing was pinned.") {
				t.Fatalf("%s: err = %v", name, err)
			}
			if len(f.runner.Calls()) != 0 || len(f.release.Requests()) != 0 {
				t.Fatalf("%s: cosign %v, requests %v", name, f.runner.Argv(), f.release.Requests())
			}
		}
	})

	t.Run("reads a relative gitdir, as git 2.48 writes with worktree.useRelativePaths, against the entry's directory", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		parent := filepath.Dir(f.dir)
		repository := filepath.Join(parent, "treerig")
		sibling := filepath.Join(parent, "treerig.feat-y")
		listing := filepath.Join(repository, ".git", "worktrees", "feat-y")
		for _, dir := range []string{listing, sibling, filepath.Join(repository, ".git")} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(listing, "gitdir"), []byte("../../../../treerig.feat-y/.git\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		path := installAt(t, f, filepath.Join(sibling, "tools"))
		f.env.Cwd = repository
		err := f.pin(f.options())
		resolved, _ := filepath.EvalSymlinks(path)
		if err == nil || !strings.Contains(err.Error(), "cosign resolves to "+resolved+", which is inside a worktree of this repository,") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a submodule's .git file with no commondir covers the checkout alone, and does not guess a repository", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		parent := filepath.Dir(f.dir)
		super := filepath.Join(parent, "super")
		checkout := filepath.Join(super, "vendor", "lib")
		gitDir := filepath.Join(super, ".git", "modules", "lib")
		for _, dir := range []string{checkout, gitDir} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(checkout, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		installAt(t, f, filepath.Join(super, "tools"))
		f.env.Cwd = checkout
		if err := f.pin(f.options()); err != nil {
			t.Fatalf("a cosign in the superproject, outside the submodule's checkout, was refused: %v", err)
		}
	})

	t.Run("refuses a cosign in a worktree the repository lists but treerig never registered", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		parent := filepath.Dir(f.dir)
		repository := filepath.Join(parent, "treerig")
		sibling := filepath.Join(parent, "treerig.feat-x") // the worktrunk layout: outside the repository
		listing := filepath.Join(repository, ".git", "worktrees", "feat-x")
		for _, dir := range []string{listing, sibling} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(sibling, ".git"), []byte("gitdir: "+listing+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(listing, "gitdir"), []byte(filepath.Join(sibling, ".git")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		path := installAt(t, f, filepath.Join(sibling, "tools"))
		f.env.Cwd = repository
		if err := os.MkdirAll(filepath.Join(repository, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		err := f.pin(f.options())
		resolved, _ := filepath.EvalSymlinks(path)
		if err == nil || !strings.Contains(err.Error(), "cosign resolves to "+resolved+", which is inside a worktree of this repository,") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("warns when the repository's worktree list exists but cannot be read", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		repository := filepath.Join(filepath.Dir(f.dir), "treerig")
		// A file where the worktrees directory should be: it exists, and
		// reading it as a directory fails (ENOTDIR), even as root.
		if err := os.MkdirAll(filepath.Join(repository, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repository, ".git", "worktrees"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		f.env.Cwd = repository
		if err := f.pin(f.options()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(f.logged(), "warn: 1 worktree could not be checked for cosign (an index card, a gitdir file or the worktree list could not be read).") {
			t.Fatalf("logged %q, want the unreadable worktree list reported", f.logged())
		}
	})

	t.Run("warns once, with a count, about registered worktrees whose index card cannot be read, and about unreadable records", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		registrytest.ClaimControlHome(t, f.control.ControlHome)
		const repositoryID = "11111111-1111-4111-8111-111111111111"
		for _, worktreeID := range []string{"22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"} {
			if _, err := registrytest.WriteRecord(f.control.ControlHome, registrytest.RecordJSON{
				Version: 1, RepositoryID: repositoryID, WorktreeID: worktreeID, Slug: "wt-" + worktreeID[:2], Domain: "acme.test", Initialization: string(registry.Complete),
			}); err != nil {
				t.Fatal(err)
			}
			// A directory where the card file should be: it exists and cannot be read.
			if err := os.MkdirAll(card.IndexCardPath(f.control.ControlHome, repositoryID, worktreeID), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.pin(f.options()); err != nil {
			t.Fatal(err)
		}
		warnings := 0
		for _, line := range strings.Split(f.logged(), "\n") {
			if strings.HasPrefix(line, "warn: ") {
				warnings++
				if line != "warn: 2 worktrees could not be checked for cosign (an index card, a gitdir file or the worktree list could not be read)." {
					t.Fatalf("warning = %q", line)
				}
			}
		}
		if warnings != 1 {
			t.Fatalf("%d warnings in %q, want one", warnings, f.logged())
		}
	})

	t.Run("one unreadable registry record is reported in the singular", func(t *testing.T) {
		f := newReleaseFixture(t, "1.2.3")
		registrytest.ClaimControlHome(t, f.control.ControlHome)
		dir := registry.RegistryDirectory(f.control.ControlHome, "11111111-1111-4111-8111-111111111111")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "22222222-2222-4222-8222-222222222222.json"), []byte("{broken"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := f.pin(f.options()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(f.logged(), "warn: 1 registry record could not be read, so its worktree is not checked for cosign.") {
			t.Fatalf("log = %q", f.logged())
		}
	})
}

func TestPlatformListsAgree(t *testing.T) {
	t.Run("the fetcher's platforms, the pin's and the pin's key pattern are one list", func(t *testing.T) {
		if strings.Join(releasefetch.Platforms, ",") != strings.Join(teardown.ReleasePlatforms, ",") {
			t.Fatalf("releasefetch %v, teardown %v", releasefetch.Platforms, teardown.ReleasePlatforms)
		}
		digests := map[string]string{}
		for _, platform := range releasefetch.Platforms {
			digests[platform] = strings.Repeat("a", 64)
		}
		for _, platform := range releasefetch.Platforms {
			pin := teardown.ReleasePin("1.2.3", "https://h/releases/download/v1.2.3/", "id", strings.Repeat("b", 64), digests, platform, strings.Repeat("c", 64), teardownTime)
			pin.Version = 1
			if err := pin.Validate(); err != nil {
				t.Fatalf("%s: %v", platform, err)
			}
		}
	})
}
