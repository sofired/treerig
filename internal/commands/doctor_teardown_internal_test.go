package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/binstore"
	"github.com/sofired/treerig/internal/files"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/teardown"
)

// TestTeardownPinChecks pins what doctor says about the teardown pin in each
// state, and that it only looks (docs/design/teardown.md, "How `worktree remove`
// uses it"): the severity, the id and instance, and the gate's own sentences.
func TestTeardownPinChecks(t *testing.T) {
	const command = "`treerig teardown pin --release <a published version, such as 0.1.0-rc.2>` (or `--file`; see `treerig teardown pin --help`)"
	newOptions := func(t *testing.T) (doctorOptions, layout.Control) {
		t.Helper()
		base := registrytest.TempDir(t)
		home := filepath.Join(base, "home")
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		control := layout.ControlLayout(filepath.Join(base, "treerig", "control-home"))
		if err := files.SecureControlDirectories(control, home); err != nil {
			t.Fatal(err)
		}
		return doctorOptions{CommandName: "treerig", ControlHome: control.ControlHome, UserHome: home, OS: "linux", Arch: "amd64", Version: "0.0.0-dev"}, control
	}
	// pin writes a file pin of content for linux_amd64 through the real writer.
	pin := func(t *testing.T, opts doctorOptions, control layout.Control, release, content string) string {
		t.Helper()
		digest := binstore.Hash([]byte(content))
		value := teardown.FilePin(filepath.Join(filepath.Dir(control.ControlHome), "src"), release, "linux_amd64", digest, time.Now())
		if _, err := teardown.Save(control, opts.UserHome, []byte(content), value, time.Now()); err != nil {
			t.Fatal(err)
		}
		return digest
	}
	summary := func(checks []Check) string {
		var parts []string
		for _, check := range checks {
			parts = append(parts, string(check.Status)+" "+check.ID+" "+check.Instance)
		}
		return strings.Join(parts, "; ")
	}

	t.Run("warns when nothing is pinned", func(t *testing.T) {
		opts, _ := newOptions(t)
		checks := teardownPinChecks(opts)
		if summary(checks) != "warn registry.teardown-pin " || checks[0].Detail != "none pinned; `treerig worktree remove` will refuse until you pin one with "+command {
			t.Fatalf("checks = %+v", checks)
		}
	})

	t.Run("is ok, not needed, for a release build with no pin, and says how to pin anyway", func(t *testing.T) {
		opts, _ := newOptions(t)
		opts.Version = "0.1.0-rc.3"
		checks := teardownPinChecks(opts)
		want := "not needed: `treerig worktree remove` runs this release build; pin one with `treerig teardown pin --release 0.1.0-rc.3` to keep removal on a verified build"
		if summary(checks) != "ok registry.teardown-pin " || checks[0].Detail != want {
			t.Fatalf("checks = %+v", checks)
		}
		// A snapshot or a git-describe version is not a release.
		for _, version := range []string{"0.0.0-dev+abc123def456", "0.1.0-rc.2-5-gabc1234"} {
			opts.Version = version
			if got := summary(teardownPinChecks(opts)); got != "warn registry.teardown-pin " {
				t.Errorf("version %s: %s", version, got)
			}
		}
	})

	t.Run("is ok for an intact pin, and says which", func(t *testing.T) {
		opts, control := newOptions(t)
		pin(t, opts, control, "my-build", "bytes")
		checks := teardownPinChecks(opts)
		if summary(checks) != "ok registry.teardown-pin " || checks[0].Detail != "release my-build (linux_amd64), binary intact" {
			t.Fatalf("checks = %+v", checks)
		}
	})

	t.Run("fails for a missing or different binary, and warns for a platform with none", func(t *testing.T) {
		opts, control := newOptions(t)
		digest := pin(t, opts, control, "my-build", "bytes")
		if err := os.WriteFile(teardown.BinaryPath(control, digest), []byte("other"), 0o700); err != nil {
			t.Fatal(err)
		}
		if got := summary(teardownPinChecks(opts)); got != "fail registry.teardown-pin " {
			t.Fatalf("different: %s", got)
		}
		if err := os.Remove(teardown.BinaryPath(control, digest)); err != nil {
			t.Fatal(err)
		}
		if got := teardownPinChecks(opts); summary(got) != "fail registry.teardown-pin " || !strings.Contains(got[0].Detail, " is missing, so `treerig worktree remove` will refuse; pin it again with `treerig teardown pin --file ") {
			t.Fatalf("missing: %+v", got)
		}
		opts.Arch = "arm64"
		if got := summary(teardownPinChecks(opts)); got != "warn registry.teardown-pin " {
			t.Fatalf("no platform: %s", got)
		}
	})

	t.Run("fails for a foreign or newer pin", func(t *testing.T) {
		for _, body := range []string{`{"version": 1, "extra": true}`, `{"version": 99}`} {
			opts, control := newOptions(t)
			if err := os.MkdirAll(control.TeardownDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(control.PinPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := summary(teardownPinChecks(opts)); got != "fail registry.teardown-pin " {
				t.Fatalf("%s: %s", body, got)
			}
		}
	})

	t.Run("warns when the running build is newer than a release pin", func(t *testing.T) {
		opts, control := newOptions(t)
		content := "release build"
		digest := binstore.Hash([]byte(content))
		value := teardown.ReleasePin("1.2.3", "https://github.com/sofired/treerig/releases/download/v1.2.3/", "https://github.com/sofired/treerig/.github/workflows/release.yml@refs/tags/v1.2.3",
			binstore.Hash([]byte("sums")), map[string]string{"linux_amd64": binstore.Hash([]byte("a")), "linux_arm64": binstore.Hash([]byte("b")), "darwin_amd64": binstore.Hash([]byte("c")), "darwin_arm64": binstore.Hash([]byte("d"))},
			"linux_amd64", digest, time.Now())
		if _, err := teardown.Save(control, opts.UserHome, []byte(content), value, time.Now()); err != nil {
			t.Fatal(err)
		}
		opts.Version = "1.3.0"
		got := teardownPinChecks(opts)
		if summary(got) != "warn registry.teardown-pin " || !strings.Contains(got[0].Detail, "The running build (1.3.0) is newer than the pinned release (1.2.3). Pin the new build with `treerig teardown pin --release 1.3.0` to update it.") {
			t.Fatalf("checks = %+v", got)
		}
	})

	t.Run("reports where the store lives and who can write it, and a noexec mount", func(t *testing.T) {
		opts, control := newOptions(t)
		pin(t, opts, control, "my-build", "bytes")
		if err := os.Chmod(control.PinPath, 0o666); err != nil {
			t.Fatal(err)
		}
		opts.NoExec = func(string) (bool, error) { return true, nil }
		if got := summary(teardownPinChecks(opts)); got != "ok registry.teardown-pin ; fail registry.teardown-store ownership; warn registry.teardown-store noexec" {
			t.Fatalf("checks = %s", got)
		}
		if err := os.Mkdir(filepath.Join(filepath.Dir(control.ControlHome), ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		if got := summary(teardownPinChecks(opts)); !strings.Contains(got, "fail registry.teardown-store location") {
			t.Fatalf("checks = %s", got)
		}
	})
}
