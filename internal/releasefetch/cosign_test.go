package releasefetch_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/releasefetch"
	"github.com/sofired/treerig/internal/run"
	"github.com/sofired/treerig/internal/run/fakerun"
)

// cosignFixture is a directory on PATH holding an executable named cosign
// (never run: a fakerun.Runner answers for it), a fake runner and the
// environment cosign is to run in.
type cosignFixture struct {
	dir     string
	dirs    releasefetch.Dirs
	cosign  string
	runner  *fakerun.Runner
	environ []string
}

// newCosignFixture answers `cosign version --json` with version.
func newCosignFixture(t *testing.T, version string) *cosignFixture {
	t.Helper()
	dir := registrytest.TempDir(t)
	f := &cosignFixture{dir: dir, cosign: filepath.Join(dir, "bin", "cosign"), runner: &fakerun.Runner{}}
	if err := os.MkdirAll(filepath.Dir(f.cosign), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.cosign, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.dirs = releasefetch.Dirs{Home: filepath.Join(dir, "cosign"), TUFRoot: filepath.Join(dir, "cosign", "tuf"), Tmp: filepath.Join(dir, "cosign", "tmp")}
	for _, d := range []string{f.dirs.Home, f.dirs.TUFRoot, f.dirs.Tmp} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.environ = []string{"PATH=" + filepath.Dir(f.cosign), "HOME=/home/me", "COSIGN_EXPERIMENTAL=1", "SIGSTORE_ID_TOKEN=t"}
	f.runner.Handler = f.answer(version, run.Result{})
	return f
}

// answer is a handler that reports version for `version --json` and gives
// verify the result for `verify-blob`.
func (f *cosignFixture) answer(version string, verify run.Result) fakerun.Handler {
	return func(_ context.Context, spec run.Spec) (run.Result, error) {
		if len(spec.Args) > 0 && spec.Args[0] == "version" {
			return run.Result{Stdout: `{"gitVersion": "` + version + `", "goVersion": "go1.26"}`}, nil
		}
		return verify, nil
	}
}

// locate is Locate over f, on linux, with roots.
func (f *cosignFixture) locate(t *testing.T, roots ...releasefetch.Root) (*releasefetch.Cosign, error) {
	t.Helper()
	return releasefetch.Locate(context.Background(), f.runner, f.environ, f.dir, f.dirs, roots)
}

func TestLocate(t *testing.T) {
	t.Run("runs the PATH-resolved path, absolute, and reports the canonical one beside it", func(t *testing.T) {
		f := newCosignFixture(t, "v3.0.6")
		shim := filepath.Join(f.dir, "shims", "cosign")
		if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(f.cosign, shim); err != nil {
			t.Fatal(err)
		}
		f.environ[0] = "PATH=" + filepath.Dir(shim)
		cosign, err := f.locate(t)
		if err != nil {
			t.Fatal(err)
		}
		canonical, _ := filepath.EvalSymlinks(f.cosign)
		if cosign.Path != shim || cosign.Canonical != canonical || cosign.Version != "3.0.6" {
			t.Fatalf("Locate = %+v", cosign)
		}
		calls := f.runner.Calls()
		if len(calls) != 1 || calls[0].Command != shim {
			t.Fatalf("calls = %+v, want one, of the PATH-resolved path %s", calls, shim)
		}
	})

	t.Run("makes a relative PATH entry absolute against the working directory it is given, not the process's", func(t *testing.T) {
		f := newCosignFixture(t, "3.0.0")
		f.environ[0] = "PATH=bin"
		cosign, err := f.locate(t)
		if err != nil || cosign.Path != f.cosign {
			t.Fatalf("Locate = %+v, %v; want path %s", cosign, err, f.cosign)
		}
		if got := f.runner.Calls()[0].Command; got != f.cosign {
			t.Fatalf("ran %q", got)
		}
	})

	t.Run("asks for the version in the contract's form, with only the allowlisted environment and treerig's own HOME, TUF_ROOT and TMPDIR", func(t *testing.T) {
		f := newCosignFixture(t, "3.0.6")
		f.environ = append(f.environ, "LANG=C", "LC_ALL=C", "https_proxy=p", "SSL_CERT_DIR=/c",
			"SIGSTORE_ROOT_FILE=/evil/root.json", "SIGSTORE_REKOR_PUBLIC_KEY=/evil/rekor.pub", "TUF_ROOT=/evil/tuf", "TMPDIR=/evil",
			"COSIGN_TRUSTED_ROOT=/evil/trusted_root.json", "COSIGN_INSECURE_IGNORE_TLOG=true", "ALL_PROXY=x")
		if _, err := f.locate(t); err != nil {
			t.Fatal(err)
		}
		spec := f.runner.Calls()[0]
		if strings.Join(spec.Args, " ") != "version --json" || spec.Timeout != 10*time.Second || !spec.AllowFailure {
			t.Fatalf("spec = %+v", spec)
		}
		want := []string{"PATH=" + filepath.Dir(f.cosign), "LANG=C", "LC_ALL=C", "https_proxy=p", "SSL_CERT_DIR=/c", "HOME=" + f.dirs.Home, "TUF_ROOT=" + f.dirs.TUFRoot, "TMPDIR=" + f.dirs.Tmp}
		if strings.Join(spec.Env, "\n") != strings.Join(want, "\n") {
			t.Fatalf("env = %v\nwant  %v", spec.Env, want)
		}
	})

	t.Run("requires 3.0.0 or newer by semver, and refuses anything it cannot read as that", func(t *testing.T) {
		for _, tc := range []struct {
			version string
			ok      bool
		}{
			{"v3.0.0", true}, {"3.0.0", true}, {"v3.0.6", true}, {"v3.1.0", true}, {"v10.0.0", true}, {"v3.0.0+build", true},
			{"v2.6.5", false}, {"v2.99.99", false}, {"v3.0.0-rc.1", false}, {"devel", false}, {"", false}, {"v3", false},
			{"v3.0.0-dev", false}, {"0.0.0-dev+abc", false}, {"three", false}, {"vv3.0.0", false},
		} {
			f := newCosignFixture(t, tc.version)
			_, err := f.locate(t)
			var refusal *releasefetch.VersionError
			if tc.ok && err != nil {
				t.Errorf("%q: err = %v", tc.version, err)
			}
			if !tc.ok && (!errors.As(err, &refusal) || refusal.Path != f.cosign) {
				t.Errorf("%q: err = %#v, want a VersionError naming %s", tc.version, err, f.cosign)
			}
		}
	})

	t.Run("a cosign that needs the user's HOME, as an asdf or mise shim does, fails its version call under treerig's environment", func(t *testing.T) {
		f := newCosignFixture(t, "3.0.6")
		f.runner.Handler = func(_ context.Context, spec run.Spec) (run.Result, error) {
			for _, entry := range spec.Env {
				if entry == "HOME=/home/me" {
					return run.Result{Stdout: `{"gitVersion": "v3.0.6"}`}, nil
				}
			}
			return run.Result{ExitCode: 1, Stderr: "asdf: No version is set for command cosign"}, nil
		}
		var refusal *releasefetch.VersionError
		_, err := f.locate(t)
		if !errors.As(err, &refusal) || refusal.Path != f.cosign || !strings.Contains(refusal.Found, "`cosign version --json` failed: asdf: No version is set") {
			t.Fatalf("err = %#v", err)
		}
	})

	t.Run("refuses output that is not JSON, a failing run, and a timeout", func(t *testing.T) {
		for name, result := range map[string]run.Result{
			"not JSON":   {Stdout: "cosign 3.0.6"},
			"empty":      {},
			"exit 1":     {ExitCode: 1, Stderr: "boom"},
			"timed out":  {ExitCode: 1, TimedOut: true},
			"JSON array": {Stdout: "[]"},
		} {
			t.Run(name, func(t *testing.T) {
				f := newCosignFixture(t, "")
				f.runner.Handler = func(context.Context, run.Spec) (run.Result, error) { return result, nil }
				var refusal *releasefetch.VersionError
				if _, err := f.locate(t); !errors.As(err, &refusal) || refusal.Path != f.cosign {
					t.Fatalf("err = %#v", err)
				}
			})
		}
	})

	t.Run("refuses when none is on PATH, running nothing", func(t *testing.T) {
		f := newCosignFixture(t, "3.0.6")
		f.environ[0] = "PATH=" + filepath.Join(f.dir, "nowhere")
		_, err := f.locate(t)
		var refusal *releasefetch.VersionError
		if !errors.As(err, &refusal) || refusal.Path != "" || refusal.Found != "cosign is not on PATH" || len(f.runner.Calls()) != 0 {
			t.Fatalf("err = %#v, calls = %v", err, f.runner.Calls())
		}
	})

	t.Run("refuses a cosign that cannot be executed", func(t *testing.T) {
		f := newCosignFixture(t, "3.0.6")
		if err := os.Chmod(f.cosign, 0o644); err != nil {
			t.Fatal(err)
		}
		var refusal *releasefetch.VersionError
		if _, err := f.locate(t); !errors.As(err, &refusal) || refusal.Path != "" || len(f.runner.Calls()) != 0 {
			t.Fatalf("err = %#v", err)
		}
	})
}

func TestLocateContainment(t *testing.T) {
	// canonical is a path with its symlinks resolved, as the roots must be.
	canonical := func(t *testing.T, path string) string {
		t.Helper()
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}

	t.Run("refuses a cosign inside a root, naming where and running nothing", func(t *testing.T) {
		f := newCosignFixture(t, "3.0.6")
		for _, where := range []string{"the worktree feature", "this checkout", "the control home"} {
			f.runner = &fakerun.Runner{Handler: f.answer("3.0.6", run.Result{})}
			_, err := f.locate(t, releasefetch.Root{Path: canonical(t, f.dir), Where: where})
			var inside *releasefetch.InsideError
			if !errors.As(err, &inside) || inside.Where != where || inside.Path != f.cosign || inside.Canonical != canonical(t, f.cosign) {
				t.Fatalf("%s: err = %#v", where, err)
			}
			if len(f.runner.Calls()) != 0 {
				t.Fatalf("cosign was run: %v", f.runner.Argv())
			}
		}
	})

	t.Run("refuses a cosign whose symlink is outside a root but whose target is inside", func(t *testing.T) {
		f := newCosignFixture(t, "3.0.6")
		outside := filepath.Join(registrytest.TempDir(t), "cosign")
		if err := os.Symlink(f.cosign, outside); err != nil {
			t.Fatal(err)
		}
		f.environ[0] = "PATH=" + filepath.Dir(outside)
		_, err := f.locate(t, releasefetch.Root{Path: canonical(t, f.dir), Where: "this checkout"})
		var inside *releasefetch.InsideError
		if !errors.As(err, &inside) || inside.Path != outside || inside.Canonical != canonical(t, f.cosign) {
			t.Fatalf("err = %#v", err)
		}
	})

	t.Run("a sibling that shares a prefix with a root is not inside it", func(t *testing.T) {
		f := newCosignFixture(t, "3.0.6")
		parent := registrytest.TempDir(t)
		root := filepath.Join(parent, "treerig")
		sibling := filepath.Join(parent, "treerig.foo")
		for _, dir := range []string{root, sibling} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		cosign := filepath.Join(sibling, "cosign")
		if err := os.WriteFile(cosign, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		f.environ[0] = "PATH=" + sibling
		if _, err := f.locate(t, releasefetch.Root{Path: canonical(t, root), Where: "this checkout"}); err != nil {
			t.Fatalf("treerig.foo was taken to be inside treerig: %v", err)
		}
		// And the reverse, so the test sees the difference: inside root itself.
		if err := os.WriteFile(filepath.Join(root, "cosign"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		f.environ[0] = "PATH=" + root
		var inside *releasefetch.InsideError
		if _, err := f.locate(t, releasefetch.Root{Path: canonical(t, root), Where: "this checkout"}); !errors.As(err, &inside) {
			t.Fatalf("err = %v, want inside", err)
		}
	})

	t.Run("a root with no connection to cosign changes nothing", func(t *testing.T) {
		f := newCosignFixture(t, "3.0.6")
		other := registrytest.TempDir(t)
		if _, err := f.locate(t, releasefetch.Root{Path: canonical(t, other), Where: "this checkout"}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a root spelled another way for the same directory still contains cosign", func(t *testing.T) {
		if _, err := os.Stat("/proc/self/root"); err != nil || runtime.GOOS != "linux" {
			t.Skip("needs /proc/self/root to spell one directory two ways")
		}
		f := newCosignFixture(t, "3.0.6")
		// Same directory, different spelling: what a case-insensitive volume
		// gives (/Users/Me against /users/me), and what filepath.Rel cannot see.
		respelled := "/proc/self/root" + canonical(t, f.dir)
		_, err := f.locate(t, releasefetch.Root{Path: respelled, Where: "this checkout"})
		var inside *releasefetch.InsideError
		if !errors.As(err, &inside) {
			t.Fatalf("err = %#v, want inside", err)
		}
	})
}

func TestVerify(t *testing.T) {
	const checksums = "the checksums\n"
	bundle := []byte(`{"bundle":true}`)

	t.Run("runs verify-blob with exactly the contract's argv, files and environment", func(t *testing.T) {
		f := newCosignFixture(t, "3.0.6")
		f.environ = append(f.environ, "TUF_ROOT=/evil")
		cosign, err := f.locate(t)
		if err != nil {
			t.Fatal(err)
		}
		var dirMode, blobMode, bundleMode os.FileMode
		var seenBlob, seenBundle []byte
		f.runner.Handler = func(_ context.Context, spec run.Spec) (run.Result, error) {
			dir := spec.Dir
			info, err := os.Stat(dir)
			if err != nil {
				t.Error(err)
			}
			dirMode = info.Mode().Perm()
			if info, err := os.Stat(filepath.Join(dir, "checksums.txt")); err == nil {
				blobMode = info.Mode().Perm()
			}
			if info, err := os.Stat(filepath.Join(dir, "checksums.txt.sigstore.json")); err == nil {
				bundleMode = info.Mode().Perm()
			}
			seenBlob, _ = os.ReadFile(filepath.Join(dir, "checksums.txt"))
			seenBundle, _ = os.ReadFile(filepath.Join(dir, "checksums.txt.sigstore.json"))
			return run.Result{}, nil
		}
		if err := cosign.Verify(context.Background(), releasefetch.KeylessPolicy{Release: "1.2.3"}, []byte(checksums), bundle); err != nil {
			t.Fatal(err)
		}
		calls := f.runner.Calls()
		spec := calls[len(calls)-1]
		dir := spec.Dir
		want := []string{
			"verify-blob",
			"--bundle", filepath.Join(dir, "checksums.txt.sigstore.json"),
			"--certificate-identity", "https://github.com/sofired/treerig/.github/workflows/release.yml@refs/tags/v1.2.3",
			"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
			filepath.Join(dir, "checksums.txt"),
		}
		if spec.Command != f.cosign || strings.Join(spec.Args, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("ran %s %q\nwant %s %q", spec.Command, spec.Args, f.cosign, want)
		}
		if spec.Timeout != 60*time.Second || !spec.AllowFailure {
			t.Fatalf("timeout = %v, allow failure = %v", spec.Timeout, spec.AllowFailure)
		}
		wantEnv := []string{"PATH=" + filepath.Dir(f.cosign), "HOME=" + f.dirs.Home, "TUF_ROOT=" + f.dirs.TUFRoot, "TMPDIR=" + f.dirs.Tmp}
		if strings.Join(spec.Env, "\n") != strings.Join(wantEnv, "\n") {
			t.Fatalf("verify env = %v\nwant        %v", spec.Env, wantEnv)
		}
		if filepath.Dir(dir) != f.dirs.Tmp {
			t.Fatalf("the temporary directory %s is not under %s", dir, f.dirs.Tmp)
		}
		if blobMode != 0o600 || bundleMode != 0o600 {
			t.Fatalf("file modes %v, %v, want 0600", blobMode, bundleMode)
		}
		if dirMode != 0o700 || string(seenBlob) != checksums || string(seenBundle) != string(bundle) {
			t.Fatalf("dir mode %v, blob %q, bundle %q", dirMode, seenBlob, seenBundle)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("the temporary directory was left behind: %v", err)
		}
	})

	t.Run("the identity carries the version being pinned, as an exact string", func(t *testing.T) {
		for _, release := range []string{"1.2.3", "1.2.4", "0.1.0-rc.1", "0.0.0-dev+abc123def456"} {
			policy := releasefetch.KeylessPolicy{Release: release}
			want := "https://github.com/sofired/treerig/.github/workflows/release.yml@refs/tags/v" + release
			if policy.Signer() != want {
				t.Fatalf("Signer = %q, want %q", policy.Signer(), want)
			}
			args := strings.Join(policy.Args("b", "c"), " ")
			if !strings.Contains(args, "--certificate-identity "+want+" ") || strings.Contains(args, "regexp") || strings.Contains(args, "--key") {
				t.Fatalf("args = %s", args)
			}
		}
	})

	t.Run("a failure carries cosign's stderr, printable and cut to about 2 KiB", func(t *testing.T) {
		f := newCosignFixture(t, "3.0.6")
		cosign, err := f.locate(t)
		if err != nil {
			t.Fatal(err)
		}
		f.runner.Handler = f.answer("3.0.6", run.Result{ExitCode: 1, Stderr: "Error: no matching signatures\x1b[2J\nsecond line\n" + strings.Repeat("x", 5000)})
		err = cosign.Verify(context.Background(), releasefetch.KeylessPolicy{Release: "1.2.3"}, []byte(checksums), bundle)
		var verify *releasefetch.VerifyError
		if !errors.As(err, &verify) {
			t.Fatalf("err = %#v", err)
		}
		if !strings.HasPrefix(verify.Stderr, "Error: no matching signatures�[2J second line xxx") || strings.ContainsAny(verify.Stderr, "\x1b\n") {
			t.Fatalf("stderr = %q", verify.Stderr[:80])
		}
		if n := len([]rune(verify.Stderr)); n != releasefetch.MaxStderr {
			t.Fatalf("stderr is %d characters, want it cut to %d", n, releasefetch.MaxStderr)
		}
	})

	t.Run("a failure that printed nothing, a timeout and a run that cannot start are all verify errors", func(t *testing.T) {
		for name, handler := range map[string]fakerun.Handler{
			"silent": func(context.Context, run.Spec) (run.Result, error) { return run.Result{ExitCode: 3}, nil },
			"timeout": func(context.Context, run.Spec) (run.Result, error) {
				return run.Result{ExitCode: 1, TimedOut: true}, nil
			},
			"cannot start": func(context.Context, run.Spec) (run.Result, error) {
				return run.Result{}, &run.StartError{Command: "cosign", Code: "ENOEXEC", Detail: "is not a valid executable (ENOEXEC).", ExitCode: 126}
			},
		} {
			t.Run(name, func(t *testing.T) {
				f := newCosignFixture(t, "3.0.6")
				cosign, err := f.locate(t)
				if err != nil {
					t.Fatal(err)
				}
				f.runner.Handler = handler
				var verify *releasefetch.VerifyError
				if err := cosign.Verify(context.Background(), releasefetch.KeylessPolicy{Release: "1.2.3"}, []byte(checksums), bundle); !errors.As(err, &verify) || verify.Stderr == "" {
					t.Fatalf("err = %#v", err)
				}
			})
		}
	})

	t.Run("refuses when the file on disk is not the bytes the caller holds", func(t *testing.T) {
		f := newCosignFixture(t, "3.0.6")
		cosign, err := f.locate(t)
		if err != nil {
			t.Fatal(err)
		}
		f.runner.Handler = func(_ context.Context, spec run.Spec) (run.Result, error) {
			return run.Result{}, os.WriteFile(filepath.Join(spec.Dir, "checksums.txt"), []byte("swapped\n"), 0o600)
		}
		err = cosign.Verify(context.Background(), releasefetch.KeylessPolicy{Release: "1.2.3"}, []byte(checksums), bundle)
		if err == nil || !strings.Contains(err.Error(), "changed on disk") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a test policy supplies its own argv", func(t *testing.T) {
		f := newCosignFixture(t, "3.0.6")
		cosign, err := f.locate(t)
		if err != nil {
			t.Fatal(err)
		}
		if err := cosign.Verify(context.Background(), fixedPolicy{}, []byte(checksums), bundle); err != nil {
			t.Fatal(err)
		}
		calls := f.runner.Calls()
		if got := strings.Join(calls[len(calls)-1].Args, " "); !strings.HasPrefix(got, "verify-blob --only-this ") {
			t.Fatalf("args = %s", got)
		}
	})
}

// fixedPolicy is a policy a test supplies: its own argv and signer.
type fixedPolicy struct{}

// Signer is a fixed test identity.
func (fixedPolicy) Signer() string { return "test-signer" }

// Args is an argv of the test's own.
func (fixedPolicy) Args(bundle, blob string) []string {
	return []string{"--only-this", bundle, blob}
}

func TestContractConstants(t *testing.T) {
	t.Run("the caps and bounds are the note's", func(t *testing.T) {
		if releasefetch.MaxChecksums != 1<<20 || releasefetch.MaxBundle != 1<<20 || releasefetch.MaxArchive != 100<<20 {
			t.Fatal("a size cap changed")
		}
		if releasefetch.DefaultLimits.Expanded != 500<<20 || releasefetch.DefaultLimits.Binary != 200<<20 {
			t.Fatal("an extraction bound changed")
		}
		if releasefetch.MinCosign != "3.0.0" || releasefetch.VersionTimeout != 10*time.Second || releasefetch.VerifyTimeout != 60*time.Second || releasefetch.MaxStderr != 2048 {
			t.Fatal("a cosign constant changed")
		}
	})

	t.Run("CosignEnv lets through only the allowlist, then sets HOME, TUF_ROOT and TMPDIR, and never returns nil", func(t *testing.T) {
		dirs := releasefetch.Dirs{Home: "/h", TUFRoot: "/h/tuf", Tmp: "/h/tmp"}
		got := releasefetch.CosignEnv([]string{
			"A=1", "HOME=/me", "TUF_ROOT=z", "TMPDIR=/t", "SIGSTORE_ROOT_FILE=x", "SIGSTORE_ID_TOKEN=t", "COSIGN_YES=1", "COSIGN_TRUSTED_ROOT=r", "COSIGN_INSECURE_IGNORE_TLOG=true",
			"HTTPS_PROXY=p", "http_proxy=q", "NO_PROXY=n", "ALL_PROXY=a", "LANG=C", "LC_CTYPE=C", "LCX=1", "SSL_CERT_FILE=f", "XDG_CONFIG_HOME=/x", "PATH=/usr/bin:rel::/abs",
		}, "/cwd", dirs)
		want := "HTTPS_PROXY=p http_proxy=q NO_PROXY=n LANG=C LC_CTYPE=C SSL_CERT_FILE=f PATH=/usr/bin:/cwd/rel:/cwd:/abs HOME=/h TUF_ROOT=/h/tuf TMPDIR=/h/tmp"
		if strings.Join(got, " ") != want {
			t.Fatalf("env = %v\nwant  %v", got, want)
		}
		if releasefetch.CosignEnv(nil, "/cwd", dirs) == nil {
			t.Fatal("a nil environment would let the child inherit the process's")
		}
	})
}

func TestVerifyClassification(t *testing.T) {
	for name, tc := range map[string]struct {
		stderr string
		want   releasefetch.VerifyKind
	}{
		"wrong identity": {"Error: none of the expected identities matched what was in the certificate, got subjects [x]\nerror during command execution: none of the expected identities matched", releasefetch.FailSignature},
		"bad signature":  {"WARNING: Skipping tlog\nError: failed to verify signature: could not verify message: invalid signature when validating ASN.1 encoded signature", releasefetch.FailSignature},
		"no log entry":   {"Error: failed to verify log inclusion: not enough verified log entries from transparency log: 0 < 1", releasefetch.FailSignature},
		"no trust root":  {"WARNING: Could not fetch trusted_root.json from the TUF repository.\nError: trusted root is required when using new bundle format", releasefetch.FailTrustRoot},
		"tuf failure":    {"Error: initializing tuf: failed to create TUF client", releasefetch.FailTrustRoot},
		"a trust-root warning before a real signature failure": {"WARNING: Could not fetch trusted_root.json from the TUF repository.\nError: failed to verify signature: invalid signature", releasefetch.FailSignature},
		"only a trust-root warning, no error line":             {"WARNING: Could not fetch trusted_root.json from the TUF repository.", releasefetch.FailUnknown},
		"a bundle that cannot be read":                         {"Error: invalid character 'j' looking for beginning of value", releasefetch.FailUnknown},
		"a tuf line before an identity failure":                {"Error: initializing tuf: failed to create TUF client\nError: none of the expected identities matched what was in the certificate", releasefetch.FailSignature},
		"an identity failure before a tuf line":                {"Error: none of the expected identities matched\nError: tuf refresh failed", releasefetch.FailSignature},
		"only a tuf line":                                      {"Error: tuf refresh failed: Get https://tuf-repo-cdn.sigstore.dev/14.root.json", releasefetch.FailTrustRoot},
		"tuf only inside another word":                         {"Error: the stuff it was given was unreadable", releasefetch.FailUnknown},
		"tuf only at the start of another word":                {"Error: the tufted input was unreadable", releasefetch.FailUnknown},
		"nothing":                                              {"", releasefetch.FailUnknown},
		"the words in a line that is not an error":             {"note: invalid signature", releasefetch.FailUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCosignFixture(t, "3.0.6")
			cosign, err := f.locate(t)
			if err != nil {
				t.Fatal(err)
			}
			f.runner.Handler = f.answer("3.0.6", run.Result{ExitCode: 1, Stderr: tc.stderr})
			var verify *releasefetch.VerifyError
			err = cosign.Verify(context.Background(), releasefetch.KeylessPolicy{Release: "1.2.3"}, []byte("c"), []byte("b"))
			if !errors.As(err, &verify) || verify.Kind != tc.want {
				t.Fatalf("err = %#v, want kind %v", err, tc.want)
			}
		})
	}

	t.Run("a run that cannot start and a timeout have their own kinds", func(t *testing.T) {
		f := newCosignFixture(t, "3.0.6")
		cosign, err := f.locate(t)
		if err != nil {
			t.Fatal(err)
		}
		for kind, handler := range map[releasefetch.VerifyKind]fakerun.Handler{
			releasefetch.FailStart: func(context.Context, run.Spec) (run.Result, error) {
				return run.Result{}, &run.StartError{Command: "cosign", ExitCode: 126}
			},
			releasefetch.FailTimeout: func(context.Context, run.Spec) (run.Result, error) {
				return run.Result{ExitCode: 1, TimedOut: true}, nil
			},
		} {
			f.runner.Handler = handler
			var verify *releasefetch.VerifyError
			if err := cosign.Verify(context.Background(), releasefetch.KeylessPolicy{Release: "1.2.3"}, []byte("c"), []byte("b")); !errors.As(err, &verify) || verify.Kind != kind {
				t.Fatalf("kind %v: err = %#v", kind, err)
			}
		}
	})
}
