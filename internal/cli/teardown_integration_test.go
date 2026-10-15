//go:build integration

package cli_test

// `teardown pin` and `teardown status` as a real process (#85): which
// stream each line lands on, the exit codes, the confirmation on a real
// terminal and without one, and `status` finding the running binary to be the
// pinned one. No Docker is involved.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/creack/pty"

	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/run/ptytest"
)

// fileSHA256 is the hex sha256 of the file at path.
func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// scratchTeardownRoot is a scratch directory to run treerig in, with the
// treerig root's settings written the way every scratch root here has them.
func scratchTeardownRoot(t *testing.T) string {
	t.Helper()
	dir := registrytest.TempDir(t)
	registrytest.WriteScratchSettings(t, filepath.Join(dir, "control-home"), "")
	return dir
}

// buildVersioned builds the CLI stamped with version, as a release build is.
func buildVersioned(t *testing.T, version string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	binary := filepath.Join(registrytest.TempDir(t), "treerig")
	build := exec.Command("go", "build", "-ldflags", "-X github.com/sofired/treerig/internal/brand.Version="+version, "-o", binary, "./cmd/treerig")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	return binary
}

func TestTeardownPinAndStatus(t *testing.T) {
	binary := buildBinary(t)
	digest := fileSHA256(t, binary)

	t.Run("pins the running binary, leaves stdout empty, and status finds it intact and the running build to be the pin", func(t *testing.T) {
		dir := scratchTeardownRoot(t)
		pinned := runTreerig(t, binary, dir, []string{"teardown", "pin", "--file", binary, "--sha256", digest, "--release", "my-build", "--yes"}, treerigOptions{})
		if pinned.Code != 0 || pinned.Stdout != "" || !strings.HasPrefix(pinned.Stderr, "[treerig] Pinned the teardown binary: release my-build (") {
			t.Fatalf("pin = %+v", pinned)
		}
		teardownDir := filepath.Join(dir, "control-home", "control-home", "teardown")
		stored := filepath.Join(teardownDir, digest)
		if got := fileSHA256(t, stored); got != digest {
			t.Fatalf("stored sha256 = %s", got)
		}
		for path, mode := range map[string]os.FileMode{teardownDir: 0o700, stored: 0o700, filepath.Join(teardownDir, "pin.json"): 0o600} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != mode {
				t.Errorf("%s: %v, %v; want mode %v", path, info, err, mode)
			}
		}

		status := runTreerig(t, binary, dir, []string{"teardown", "status"}, treerigOptions{})
		if status.Code != 0 || status.Stderr != "" || !strings.Contains(status.Stdout, "  binary  "+stored+" sha256 "+digest+" ok\n") {
			t.Fatalf("status = %+v", status)
		}
		report := runTreerig(t, binary, dir, []string{"teardown", "status", "--json"}, treerigOptions{})
		var parsed struct {
			Pinned  bool `json:"pinned"`
			OK      bool `json:"ok"`
			Running struct {
				IsPin bool `json:"isPin"`
			} `json:"running"`
		}
		if err := json.Unmarshal([]byte(report.Stdout), &parsed); err != nil || report.Code != 0 || !parsed.Pinned || !parsed.OK || !parsed.Running.IsPin {
			t.Fatalf("status --json = %+v (%v), parsed %+v", report, err, parsed)
		}
	})

	t.Run("with no pin: status says so on stderr and exits 1, and --json prints {pinned: false} first", func(t *testing.T) {
		dir := scratchTeardownRoot(t)
		want := "[treerig] ERROR: No teardown binary is pinned. Pin one with `treerig teardown pin --release X.Y.Z`.\n"
		text := runTreerig(t, binary, dir, []string{"teardown", "status"}, treerigOptions{})
		if text.Code != 1 || text.Stdout != "" || text.Stderr != want {
			t.Fatalf("status = %+v", text)
		}
		asJSON := runTreerig(t, binary, dir, []string{"teardown", "status", "--json"}, treerigOptions{})
		if asJSON.Code != 1 || asJSON.Stdout != "{\n  \"pinned\": false\n}\n" || asJSON.Stderr != want {
			t.Fatalf("status --json = %+v", asJSON)
		}
	})

	t.Run("without a terminal it refuses to ask, names the phrase, and pins nothing", func(t *testing.T) {
		dir := scratchTeardownRoot(t)
		result := runTreerig(t, binary, dir, []string{"teardown", "pin", "--file", binary, "--sha256", digest, "--release", "my-build"}, treerigOptions{})
		if result.Code != 1 || result.Stdout != "" || result.Stderr != "[treerig] ERROR: Interactive confirmation required: \"pin teardown binary\".\n" {
			t.Fatalf("pin = %+v", result)
		}
		if _, err := os.Lstat(filepath.Join(dir, "control-home", "control-home", "teardown")); !os.IsNotExist(err) {
			t.Fatalf("teardown directory: %v", err)
		}
	})

	t.Run("refuses a digest that is not the file's, and pins nothing", func(t *testing.T) {
		dir := scratchTeardownRoot(t)
		wrong := strings.Repeat("0", 64)
		result := runTreerig(t, binary, dir, []string{"teardown", "pin", "--file", binary, "--sha256", wrong, "--release", "my-build", "--yes"}, treerigOptions{})
		want := "[treerig] ERROR: Refusing to pin my-build: " + binary + " has sha256 " + digest + ", not the " + wrong + " you supplied. Nothing was pinned. Check that you are pinning the file you verified and that --sha256 is its digest.\n"
		if result.Code != 1 || result.Stdout != "" || result.Stderr != want {
			t.Fatalf("pin = %+v", result)
		}
		if _, err := os.Lstat(filepath.Join(dir, "control-home", "control-home", "teardown")); !os.IsNotExist(err) {
			t.Fatalf("teardown directory: %v", err)
		}
	})

	t.Run("on a terminal it shows the plan, takes the phrase and pins; another answer cancels", func(t *testing.T) {
		for _, tc := range []struct {
			answer   string
			wantCode int
			wantText string
		}{
			{"pin teardown binary\n", 0, "Pinned the teardown binary"},
			{"yes\n", 1, "Confirmation did not match; operation cancelled."},
		} {
			dir := scratchTeardownRoot(t)
			cmd := exec.Command(binary, "teardown", "pin", "--file", binary, "--sha256", digest, "--release", "my-build")
			cmd.Dir = dir
			cmd.Env = []string{"TREERIG_HOME=" + filepath.Join(dir, "control-home"), "PATH=" + os.Getenv("PATH"), "HOME=" + dir}
			ptmx, err := pty.Start(cmd)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ptmx.Close() })
			ptytest.ReadUntil(t, ptmx, "Pin the teardown binary")
			ptytest.ReadUntil(t, ptmx, "  release   my-build  (current pin: none)")
			ptytest.ReadUntil(t, ptmx, "  source    file:"+binary)
			ptytest.ReadUntil(t, ptmx, "  signer    none (digest supplied by you); no signature was checked")
			ptytest.ReadUntil(t, ptmx, "sha256 "+digest)
			ptytest.ReadUntil(t, ptmx, "  `worktree remove` does not run this binary yet.")
			ptytest.ReadUntil(t, ptmx, "Type this exact phrase to continue:")
			ptytest.ReadUntil(t, ptmx, "> ")
			if _, err := ptmx.WriteString(tc.answer); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			code := 0
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			transcript := ptytest.Transcript(ptmx)
			if code != tc.wantCode || !strings.Contains(transcript, tc.wantText) {
				t.Fatalf("answer %q: exit %d, want %d; transcript %q", tc.answer, code, tc.wantCode, transcript)
			}
			_, statErr := os.Lstat(filepath.Join(dir, "control-home", "control-home", "teardown", "pin.json"))
			if (statErr == nil) != (tc.wantCode == 0) {
				t.Fatalf("answer %q: pin.json: %v", tc.answer, statErr)
			}
		}
	})

	t.Run("a file pin labelled like a version never drives the newer-build hint", func(t *testing.T) {
		dir := scratchTeardownRoot(t)
		newer := buildVersioned(t, "9.9.9")
		pinned := runTreerig(t, newer, dir, []string{"teardown", "pin", "--file", binary, "--sha256", digest, "--release", "1.0.0", "--yes"}, treerigOptions{})
		if pinned.Code != 0 {
			t.Fatalf("pin = %+v", pinned)
		}
		status := runTreerig(t, newer, dir, []string{"teardown", "status"}, treerigOptions{})
		if status.Code != 0 || status.Stderr != "" {
			t.Fatalf("status = %+v", status)
		}
		report := runTreerig(t, newer, dir, []string{"teardown", "status", "--json"}, treerigOptions{})
		if report.Code != 0 || report.Stderr != "" || !strings.Contains(report.Stdout, "\"newerThanPin\": false") || !strings.Contains(report.Stdout, "\"isPin\": false") {
			t.Fatalf("status --json = %+v", report)
		}
	})

	t.Run("refuses a digest-matching file that is not a treerig executable for this machine", func(t *testing.T) {
		dir := scratchTeardownRoot(t)
		empty := filepath.Join(dir, "empty")
		if err := os.WriteFile(empty, nil, 0o755); err != nil {
			t.Fatal(err)
		}
		result := runTreerig(t, binary, dir, []string{"teardown", "pin", "--file", empty, "--sha256", fileSHA256(t, empty), "--release", "x", "--yes"}, treerigOptions{})
		if result.Code != 1 || result.Stdout != "" || !strings.Contains(result.Stderr, "is not a "+runtime.GOOS+"_"+runtime.GOARCH+" executable (the file is empty). Nothing was pinned.") {
			t.Fatalf("pin = %+v", result)
		}
	})
}

func TestTeardownPinDownloadsThroughTheProxyTheEnvironmentNames(t *testing.T) {
	binary := buildBinary(t)
	dir := scratchTeardownRoot(t)
	// A cosign that reports a version is all the pin needs before it fetches.
	tools := registrytest.TempDir(t)
	if err := os.WriteFile(filepath.Join(tools, "cosign"), []byte("#!/bin/sh\necho '{\"gitVersion\": \"v3.0.6\"}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A proxy that records what it is asked to reach, and refuses.
	var mu sync.Mutex
	var asked []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		asked = append(asked, req.Method+" "+req.Host)
		mu.Unlock()
		http.Error(w, "no", http.StatusBadGateway)
	}))
	defer proxy.Close()

	result := runTreerig(t, binary, dir, []string{"teardown", "pin", "--release", "1.2.3", "--yes"}, treerigOptions{
		path: tools + ":" + os.Getenv("PATH"),
		env:  map[string]string{"HTTPS_PROXY": proxy.URL, "https_proxy": proxy.URL, "NO_PROXY": "", "no_proxy": ""},
	})
	if result.Code != 1 || result.Stdout != "" || !strings.Contains(result.Stderr, "[treerig] Downloading https://github.com/sofired/treerig/releases/download/v1.2.3/checksums.txt…\n[treerig] ERROR: Refusing to pin 1.2.3: ") || !strings.HasSuffix(result.Stderr, " Nothing was pinned. Check your network connection and HTTPS_PROXY settings, then run it again.\n") {
		t.Fatalf("pin = %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) == 0 || asked[0] != "CONNECT github.com:443" {
		t.Fatalf("the proxy was asked %v, want a CONNECT to github.com:443", asked)
	}
	if _, err := os.Lstat(filepath.Join(dir, "control-home", "control-home", "teardown", "pin.json")); !os.IsNotExist(err) {
		t.Fatalf("pin.json: %v", err)
	}
}
