package commands_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/sofired/treerig/internal/binstore"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/stateformat"
	"github.com/sofired/treerig/internal/teardown"
	"github.com/sofired/treerig/internal/teardown/teardowntest"
)

// execCall is one call of a fixture's fake Exec.
type execCall struct {
	path        string
	argv, envv  []string
	pathContent []byte
}

// gateFixture is a teardownFixture wired the way cli.MainProcess wires an Env:
// an Executable naming a build outside the control home and an Exec that
// records its call instead of replacing the test process.
type gateFixture struct {
	*teardownFixture
	// running is the invoking build's file, outside the control home.
	running string
	calls   []execCall
	// execErr is what the fake Exec returns; nil stands for "it returned".
	execErr error
	// beforeExec, when set, runs inside the fake Exec before it reads its path.
	beforeExec func()
}

// newGateFixture builds a gateFixture whose argv is `trig worktree remove --yes`.
func newGateFixture(t *testing.T) *gateFixture {
	t.Helper()
	f := &gateFixture{teardownFixture: newTeardownFixture(t)}
	f.running = filepath.Join(f.dir, "invoked-treerig")
	if err := os.WriteFile(f.running, teardowntestExecutable("an invoked branch build"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.env.Args = []string{"trig", "worktree", "remove", "--yes"}
	f.env.Environ = []string{"PATH=/usr/bin", "LD_PRELOAD=/x.so", "HOME=" + f.home, "DYLD_INSERT_LIBRARIES=/y.dylib", "GODEBUG=x=1", "GOFLAGS=-mod=mod", "LD_LIBRARY_PATH=/z", "LDX=kept"}
	f.env.Executable = func() (string, error) { return f.running, nil }
	f.env.Exec = func(path string, argv, envv []string) error {
		if f.beforeExec != nil {
			f.beforeExec()
		}
		content, _ := os.ReadFile(path)
		f.calls = append(f.calls, execCall{path: path, argv: argv, envv: envv, pathContent: content})
		return f.execErr
	}
	f.execErr = errExecStopped
	return f
}

// errExecStopped is what the fake Exec returns by default: the stand-in for
// the exec having happened, which the real one never returns from.
var errExecStopped = errors.New("test: exec stopped here")

// remove runs `worktree remove` as version with options' other fields as given.
func (f *gateFixture) remove(options commands.RemoveOptions) error {
	if options.Version == "" {
		options.Version = "0.0.0-dev"
	}
	return commands.Remove(context.Background(), f.env, options)
}

// pinnedFile pins the fixture's own binary and returns its digest.
func (f *gateFixture) pinnedFile(t *testing.T) string {
	t.Helper()
	if err := f.pin(f.pinOptions()); err != nil {
		t.Fatal(err)
	}
	f.log.Lines, f.log.Printed = nil, nil
	return f.digest
}

// asPinnedChild makes the fake Executable report the stored copy of digest.
func (f *gateFixture) asPinnedChild(digest string) {
	f.env.Executable = func() (string, error) { return f.stored(digest), nil }
}

// devNoPin is the reason and next steps of the no-pin refusal of a development
// build (version 0.0.0-dev), on its continuation lines.
const devNoPin = "this is a development build (0.0.0-dev), which may carry a branch's code, and no teardown binary is pinned. A development build removes only with a pinned one (a release build needs none).\n" +
	"Pin the latest release: `treerig teardown pin --latest`\n" +
	"or a treerig you trust: `treerig teardown pin --file <a treerig you trust> --sha256 <its sha256> --release <a label>`\n" +
	"(`treerig teardown pin --help` explains both). To remove with this build's own code while you work on removal itself, add `--dev-teardown`."

// teardownRefusal is the gate's refusal text for target with detail as its
// reason and next step.
func teardownRefusal(target, detail string) string {
	return "Refusing to remove " + target + ": " + detail + " Nothing was removed."
}

func TestTeardownGate(t *testing.T) {
	const thisWorktree = "this worktree"

	t.Run("does nothing when Executable or Exec is not wired", func(t *testing.T) {
		f := newGateFixture(t)
		f.env.Exec = nil
		err := f.remove(commands.RemoveOptions{Yes: true})
		if err != nil && strings.Contains(err.Error(), "Refusing to remove") {
			t.Fatalf("err = %v, want the gate skipped", err)
		}
		f.env.Exec = func(string, []string, []string) error { t.Fatal("Exec called"); return nil }
		f.env.Executable = nil
		err = f.remove(commands.RemoveOptions{Yes: true})
		if err != nil && strings.Contains(err.Error(), "Refusing to remove") {
			t.Fatalf("err = %v, want the gate skipped", err)
		}
	})

	t.Run("refuses without a pin, with the exact text, before any context is built", func(t *testing.T) {
		f := newGateFixture(t)
		err := f.remove(commands.RemoveOptions{Yes: true})
		wantRefusal(t, err, teardownRefusal(thisWorktree, devNoPin))
		if len(f.calls) != 0 || f.printed() != "" || f.logged() != "" {
			t.Fatalf("calls %v, stdout %q, stderr %q; want nothing", f.calls, f.printed(), f.logged())
		}
		f2 := newGateFixture(t)
		err = f2.remove(commands.RemoveOptions{Identifier: "feature-login"})
		wantRefusal(t, err, teardownRefusal("the worktree feature-login", devNoPin))
	})

	t.Run("with no pin, a development build refuses and names itself, whatever its version looks like", func(t *testing.T) {
		f := newGateFixture(t)
		for _, version := range []string{"0.0.0-dev", "0.0.0-dev+abc123def456", "garbage", "1.2.3+metadata", "0.1.0-rc.2-5-gabc1234", "0.1.0-rc.2-5-gabc1234-dirty", "0.1.0-dirty", "0.0.0-rc.1"} {
			err := f.remove(commands.RemoveOptions{Yes: true, Version: version})
			if err == nil || !strings.Contains(err.Error(), "this is a development build ("+version+")") || !strings.Contains(err.Error(), "`treerig teardown pin --help` explains both") {
				t.Fatalf("version %q: err = %v", version, err)
			}
		}
		if len(f.calls) != 0 {
			t.Fatalf("exec was called: %v", f.calls)
		}
	})

	t.Run("with no pin, a release build removes with itself, relocated through bin and with nothing said", func(t *testing.T) {
		for _, version := range []string{"0.1.0-rc.3", "1.2.3"} {
			f := newGateFixture(t)
			f.execErr = nil // the relocation's exec "succeeds"
			err := f.remove(commands.RemoveOptions{Yes: true, Version: version})
			if err != nil && strings.Contains(err.Error(), "Refusing") {
				t.Fatalf("%s: err = %v, want no refusal", version, err)
			}
			hash, herr := binstore.HashFile(f.running)
			if herr != nil {
				t.Fatal(herr)
			}
			if len(f.calls) != 1 || f.calls[0].path != filepath.Join(f.control.BinDir, hash) {
				t.Fatalf("%s: exec calls = %+v, want the relocation to bin/", version, f.calls)
			}
			if f.logged() != "" || f.printed() != "" {
				t.Fatalf("%s: stderr %q, stdout %q; want nothing said", version, f.logged(), f.printed())
			}
			if _, err := os.Lstat(f.control.TeardownDir); !os.IsNotExist(err) {
				t.Fatalf("%s: the teardown directory exists (%v); an unpinned release writes none", version, err)
			}
		}
	})

	t.Run("with no pin, a release build that cannot find its own binary warns once and goes on, without an exec", func(t *testing.T) {
		f := newGateFixture(t)
		f.env.Executable = func() (string, error) { return "", errors.New("no /proc") }
		err := f.remove(commands.RemoveOptions{Yes: true, Version: "0.1.0-rc.3"})
		if err != nil && strings.Contains(err.Error(), "Refusing") {
			t.Fatalf("err = %v, want no refusal", err)
		}
		if want := "warn: Could not find its own running binary before removing: no /proc; continuing with this binary."; f.logged() != want {
			t.Fatalf("stderr = %q, want exactly %q", f.logged(), want)
		}
		if len(f.calls) != 0 {
			t.Fatalf("exec was called: %v", f.calls)
		}
	})

	t.Run("with no pin, a release build already running as its bin copy goes on without another exec", func(t *testing.T) {
		f := newGateFixture(t)
		f.execErr = nil
		// A first removal installs the copy, as the real relocation does.
		_ = f.remove(commands.RemoveOptions{Yes: true, Version: "0.1.0-rc.3"})
		if len(f.calls) != 1 {
			t.Fatalf("setup: exec calls = %+v", f.calls)
		}
		bin := f.calls[0].path
		f.calls = nil
		f.log.Lines, f.log.Printed = nil, nil
		f.env.Executable = func() (string, error) { return bin, nil }
		err := f.remove(commands.RemoveOptions{Yes: true, Version: "0.1.0-rc.3"})
		if err != nil && strings.Contains(err.Error(), "Refusing") {
			t.Fatalf("err = %v, want no refusal", err)
		}
		if len(f.calls) != 0 || f.logged() != "" {
			t.Fatalf("exec calls %v, stderr %q; want none", f.calls, f.logged())
		}
	})

	t.Run("a pin wins for a release build too: it is verified and used, and a broken one refuses", func(t *testing.T) {
		f := newGateFixture(t)
		digest := f.pinnedFile(t)
		err := f.remove(commands.RemoveOptions{Yes: true, Version: "0.1.0-rc.3"})
		if !errors.Is(err, errExecStopped) || len(f.calls) != 1 || binstore.Hash(f.calls[0].pathContent) != digest {
			t.Fatalf("err = %v, calls %+v; want the pinned binary run", err, f.calls)
		}
		if err := os.WriteFile(f.stored(digest), []byte("tampered"), 0o700); err != nil {
			t.Fatal(err)
		}
		f.calls = nil
		err = f.remove(commands.RemoveOptions{Yes: true, Version: "0.1.0-rc.3"})
		if err == nil || !strings.Contains(err.Error(), "has sha256 "+binstore.Hash([]byte("tampered"))) || len(f.calls) != 0 {
			t.Fatalf("err = %v, calls %v; want a broken pin to refuse a release build", err, f.calls)
		}
	})

	t.Run("a foreign or newer pin refuses a release build as well", func(t *testing.T) {
		for _, body := range []string{`{"version": 1, "extra": true}`, `{"version": 99}`} {
			f := newGateFixture(t)
			if err := os.MkdirAll(f.control.TeardownDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.control.PinPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			err := f.remove(commands.RemoveOptions{Yes: true, Version: "0.1.0-rc.3"})
			if err == nil || len(f.calls) != 0 {
				t.Fatalf("%s: err = %v, calls %v; want a refusal", body, err, f.calls)
			}
		}
	})

	t.Run("runs the stored copy with the caller's argv and environment, and says so first", func(t *testing.T) {
		f := newGateFixture(t)
		digest := f.pinnedFile(t)
		err := f.remove(commands.RemoveOptions{Yes: true})
		if !errors.Is(err, errExecStopped) {
			t.Fatalf("err = %v, want the fake exec's stop", err)
		}
		if len(f.calls) != 1 {
			t.Fatalf("exec calls = %d, want 1", len(f.calls))
		}
		call := f.calls[0]
		if strings.Join(call.argv, " ") != "trig worktree remove --yes" || call.argv[0] != "trig" {
			t.Fatalf("argv = %q, want the caller's, argv[0] included", call.argv)
		}
		if got, want := strings.Join(call.envv, " "), "PATH=/usr/bin HOME="+f.home+" LDX=kept"; got != want {
			t.Fatalf("env = %q, want the caller's without the loader and runtime variables: %q", got, want)
		}
		if binstore.Hash(call.pathContent) != digest {
			t.Fatalf("the path handed to Exec holds sha256 %s, want the pinned %s", binstore.Hash(call.pathContent), digest)
		}
		want := "info: Running the pinned teardown binary " + f.stored(digest) + " (release my-build) to remove this worktree."
		if !strings.HasPrefix(f.logged(), want) {
			t.Fatalf("stderr = %q, want it to start with %q", f.logged(), want)
		}
	})

	t.Run("hands the pinned child no -v, --verbose or --progress, which an older pin would refuse", func(t *testing.T) {
		for _, c := range []struct{ name, args, want string }{
			{"short", "trig worktree remove x -v --yes", "trig worktree remove x --yes"},
			{"repeated short", "trig worktree remove -vv x", "trig worktree remove x"},
			{"long", "trig worktree remove --verbose x", "trig worktree remove x"},
			{"long with a value", "trig worktree remove --verbose=true x", "trig worktree remove x"},
			{"progress with a value", "trig -v --progress plain worktree remove x", "trig worktree remove x"},
			{"progress joined", "trig worktree remove --progress=plain x", "trig worktree remove x"},
			{"after the dashes they are the caller's", "trig worktree remove -- -v", "trig worktree remove -- -v"},
			{"a flag's value is never stripped", "trig worktree remove --domain -v slug", "trig worktree remove --domain -v slug"},
			{"nor a value after a stripped flag", "trig worktree remove -v --label-namespace --progress x", "trig worktree remove --label-namespace --progress x"},
			{"others stay", "trig worktree remove --domain d --yes x", "trig worktree remove --domain d --yes x"},
		} {
			f := newGateFixture(t)
			f.pinnedFile(t)
			f.env.Args = strings.Fields(c.args)
			// The fake pinned child: like a 0.1.x build, it knows none of the flags.
			f.env.Exec = func(path string, argv, envv []string) error {
				for i := 1; i < len(argv); i++ {
					a := argv[i]
					if a == "--" {
						break
					}
					if a == "--domain" || a == "--label-namespace" {
						i++ // its value is the child's to read
						continue
					}
					if a == "-v" || a == "-vv" || strings.HasPrefix(a, "--verbose") || strings.HasPrefix(a, "--progress") || a == "plain" {
						f.calls = append(f.calls, execCall{path: path, argv: argv})
						return fmt.Errorf("unknown flag: %s", a)
					}
				}
				f.calls = append(f.calls, execCall{path: path, argv: argv})
				return errExecStopped
			}
			if err := f.remove(commands.RemoveOptions{Yes: true}); !errors.Is(err, errExecStopped) {
				t.Errorf("%s: err = %v, want the fake child to accept its argv", c.name, err)
				continue
			}
			if got := strings.Join(f.calls[0].argv, " "); got != c.want {
				t.Errorf("%s: argv = %q, want %q", c.name, got, c.want)
			}
		}
	})

	t.Run("hands the relocated --dev-teardown copy the caller's argv as it is", func(t *testing.T) {
		f := newGateFixture(t)
		f.env.Args = []string{"trig", "worktree", "remove", "--dev-teardown", "-v", "--progress", "plain"}
		f.execErr = nil
		_ = f.remove(commands.RemoveOptions{Yes: true, DevTeardown: true})
		if len(f.calls) != 1 || strings.Join(f.calls[0].argv, " ") != "trig worktree remove --dev-teardown -v --progress plain" {
			t.Fatalf("exec calls = %+v, want the caller's argv unchanged", f.calls)
		}
	})

	t.Run("names the worktree in the notice when one is given", func(t *testing.T) {
		f := newGateFixture(t)
		digest := f.pinnedFile(t)
		_ = f.remove(commands.RemoveOptions{Identifier: "feature-login", Yes: true})
		want := "info: Running the pinned teardown binary " + f.stored(digest) + " (release my-build) to remove the worktree feature-login."
		if !strings.HasPrefix(f.logged(), want) {
			t.Fatalf("stderr = %q, want it to start with %q", f.logged(), want)
		}
	})

	t.Run("executes the bytes it verified even if the stored path is replaced in between", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("only Linux can execute through the verified descriptor; elsewhere ExecPath refuses a replaced file (internal/teardown)")
		}
		f := newGateFixture(t)
		digest := f.pinnedFile(t)
		f.beforeExec = func() {
			evil := f.stored(digest) + ".evil"
			if err := os.WriteFile(evil, []byte("evil"), 0o700); err != nil {
				t.Error(err)
				return
			}
			if err := os.Rename(evil, f.stored(digest)); err != nil {
				t.Error(err)
			}
		}
		_ = f.remove(commands.RemoveOptions{Yes: true})
		if len(f.calls) != 1 || binstore.Hash(f.calls[0].pathContent) != digest {
			t.Fatalf("calls = %+v, want the verified bytes to be what the exec path names", f.calls)
		}
	})

	t.Run("refuses a stored binary that is not the pinned one", func(t *testing.T) {
		f := newGateFixture(t)
		digest := f.pinnedFile(t)
		if err := os.WriteFile(f.stored(digest), []byte("tampered"), 0o700); err != nil {
			t.Fatal(err)
		}
		err := f.remove(commands.RemoveOptions{Yes: true})
		wantRefusal(t, err, teardownRefusal(thisWorktree, "the pinned teardown binary "+f.stored(digest)+" has sha256 "+binstore.Hash([]byte("tampered"))+", not the pinned "+digest+". Pin it again with "+f.pinAgain()+"."))
		if len(f.calls) != 0 {
			t.Fatalf("exec was called: %v", f.calls)
		}
	})

	t.Run("refuses a missing stored binary, a symlink in its place and an unreadable one", func(t *testing.T) {
		f := newGateFixture(t)
		digest := f.pinnedFile(t)
		if err := os.Remove(f.stored(digest)); err != nil {
			t.Fatal(err)
		}
		wantRefusal(t, f.remove(commands.RemoveOptions{Yes: true}), teardownRefusal(thisWorktree, "the pinned teardown binary "+f.stored(digest)+" is missing. Pin it again with "+f.pinAgain()+"."))
		if err := os.Symlink(f.file, f.stored(digest)); err != nil {
			t.Fatal(err)
		}
		wantRefusal(t, f.remove(commands.RemoveOptions{Yes: true}), teardownRefusal(thisWorktree, "the pinned teardown binary "+f.stored(digest)+" is not a regular file. Pin it again with "+f.pinAgain()+"."))
		if len(f.calls) != 0 {
			t.Fatalf("exec was called: %v", f.calls)
		}
	})

	t.Run("refuses a pin with no binary for this platform", func(t *testing.T) {
		f := newGateFixture(t)
		f.pinnedFile(t)
		f.env.Arch = "arm64"
		wantRefusal(t, f.remove(commands.RemoveOptions{Yes: true}), teardownRefusal(thisWorktree, "the teardown pin has no binary for linux_arm64. Pin one for this machine with "+pinCommand+"."))
	})

	t.Run("refuses a foreign pin and a newer one, and never rewrites them", func(t *testing.T) {
		for _, tc := range []struct {
			name, body string
			is         error
			want       string
		}{
			{"foreign", `{"version": 1, "extra": true}`, teardown.ErrForeign, ""},
			{"newer", `{"version": 99}`, stateformat.ErrNewer, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newGateFixture(t)
				if err := os.MkdirAll(f.control.TeardownDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.control.PinPath, []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
				err := f.remove(commands.RemoveOptions{Yes: true})
				if !errors.Is(err, tc.is) {
					t.Fatalf("err = %v, want %v", err, tc.is)
				}
				if tc.name == "foreign" {
					wantRefusal(t, err, teardownRefusal(thisWorktree, "the teardown pin "+f.control.PinPath+" is not one this build wrote. Delete "+f.control.TeardownDir+"/, then pin again with "+pinCommand+"."))
				}
				if got, _ := os.ReadFile(f.control.PinPath); string(got) != tc.body {
					t.Fatalf("pin.json = %q, want it untouched", got)
				}
				if len(f.calls) != 0 {
					t.Fatalf("exec was called: %v", f.calls)
				}
			})
		}
	})

	t.Run("refuses a pin it cannot read", func(t *testing.T) {
		f := newGateFixture(t)
		if err := os.MkdirAll(f.control.PinPath, 0o700); err != nil { // a directory where the file belongs
			t.Fatal(err)
		}
		err := f.remove(commands.RemoveOptions{Yes: true})
		if err == nil || !strings.HasPrefix(err.Error(), "Refusing to remove this worktree: the teardown pin "+f.control.PinPath+" could not be read (") || !strings.HasSuffix(err.Error(), "Nothing was removed.") {
			t.Fatalf("err = %v", err)
		}
		if len(f.calls) != 0 {
			t.Fatalf("exec was called: %v", f.calls)
		}
	})

	t.Run("adds the status hint when the running build is newer than the pinned release", func(t *testing.T) {
		f := newGateFixture(t)
		f.releasePin(t, "1.2.3", "release build")
		digest := binstore.Hash([]byte("release build"))
		if err := os.Remove(f.stored(digest)); err != nil {
			t.Fatal(err)
		}
		err := f.remove(commands.RemoveOptions{Yes: true, Version: "1.3.0"})
		wantRefusal(t, err, teardownRefusal(thisWorktree, "the pinned teardown binary "+f.stored(digest)+" is missing. Pin it again with `treerig teardown pin --release 1.2.3`. "+
			"The running build (1.3.0) is newer than the pinned release (1.2.3). Pin the new build with `treerig teardown pin --latest` to update it."))
		// No hint for a build that is not newer.
		err = f.remove(commands.RemoveOptions{Yes: true, Version: "1.2.3"})
		if err == nil || strings.Contains(err.Error(), "newer") {
			t.Fatalf("err = %v, want no hint", err)
		}
	})

	t.Run("refuses a control home inside a git work tree, whatever the pin there says", func(t *testing.T) {
		f := newGateFixture(t)
		f.pinnedFile(t)
		checkout := filepath.Dir(f.dir)
		if err := os.Mkdir(filepath.Join(checkout, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		err := f.remove(commands.RemoveOptions{Yes: true})
		wantRefusal(t, err, teardownRefusal(thisWorktree, f.control.ControlHome+"/ is inside the git work tree "+checkout+"/"+
			", where a branch could plant its own teardown pin. Keep the control home outside every checkout: move TREERIG_HOME (or TREERIG_CONTROL_HOME, if you set it) to a directory that is not inside a git work tree."))
		var inside *teardown.InsideWorkTreeError
		if !errors.As(err, &inside) || len(f.calls) != 0 {
			t.Fatalf("err = %v, calls %v; want the location error and no exec", err, f.calls)
		}
		// A pinned child is not asked: it is already running, and only reads the pin.
		f.asPinnedChild(f.digest)
		if err := f.remove(commands.RemoveOptions{Yes: true}); err != nil && strings.Contains(err.Error(), "git work tree") {
			t.Fatalf("a pinned child was refused for where it lives: %v", err)
		}
	})

	t.Run("the dotfiles exemption needs the user database's home to be the same directory as HOME, and nothing a checkout can set", func(t *testing.T) {
		// A work tree rooted at the home directory, as a dotfiles repository is,
		// with the control home under it.
		for name, tc := range map[string]struct {
			passwdHome func(base string) string
			refused    bool
		}{
			"the database's home is HOME":               {func(base string) string { return base }, false},
			"another spelling of it":                    {func(base string) string { return base + "/home/../" }, false},
			"no database home":                          {func(string) string { return "" }, true},
			"a database home that is another directory": {func(base string) string { return filepath.Join(base, "work") }, true},
			"a database home that does not exist":       {func(base string) string { return filepath.Join(base, "nope") }, true},
		} {
			f := newGateFixture(t)
			base := filepath.Dir(f.dir)
			f.home = base // HOME is the work tree's root: what a .envrc's HOME=$PWD gives
			if err := os.Mkdir(filepath.Join(base, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			f.pinnedFile(t)
			f.env.PasswdHome = tc.passwdHome(base)
			err := f.remove(commands.RemoveOptions{Yes: true})
			var inside *teardown.InsideWorkTreeError
			if refused := errors.As(err, &inside); refused != tc.refused {
				t.Fatalf("%s: err = %v, refused = %v, want %v", name, err, refused, tc.refused)
			}
			if tc.refused && inside.Root != base || !tc.refused && len(f.calls) != 1 {
				t.Fatalf("%s: err = %v, calls %v", name, err, f.calls)
			}
		}
	})

	t.Run("refuses a store another user could write, naming the path and the fix", func(t *testing.T) {
		for name, path := range map[string]func(*gateFixture, string) string{
			"the teardown directory": func(f *gateFixture, _ string) string { return f.control.TeardownDir },
			"pin.json":               func(f *gateFixture, _ string) string { return f.control.PinPath },
			"the stored binary":      func(f *gateFixture, digest string) string { return f.stored(digest) },
		} {
			f := newGateFixture(t)
			digest := f.pinnedFile(t)
			target := path(f, digest)
			if err := os.Chmod(target, 0o770); err != nil {
				t.Fatal(err)
			}
			err := f.remove(commands.RemoveOptions{Yes: true})
			shown := target
			if target == f.control.TeardownDir {
				shown += "/" // a directory is written with its slash, here and where it is to be deleted
			}
			wantRefusal(t, err, teardownRefusal(thisWorktree, shown+" can be written by its group or others (mode 0770), so the teardown pin is not private to you. Do one of these: fix its owner and mode; delete "+f.control.TeardownDir+"/ and pin again with "+pinCommand+"; or move the control home to a local filesystem with TREERIG_CONTROL_HOME."))
			var untrusted *teardown.UntrustedError
			if !errors.As(err, &untrusted) || len(f.calls) != 0 {
				t.Fatalf("%s: err = %v, calls %v", name, err, f.calls)
			}
		}
	})

	t.Run("appends the newer-build hint to the line it prints before running a stale pin", func(t *testing.T) {
		f := newGateFixture(t)
		f.env.ConfigPath = filepath.Join(f.dir, "treerig.toml") // a treerig.toml is in reach: no config hint
		f.releasePin(t, "1.2.3", "release build")
		_ = f.remove(commands.RemoveOptions{Yes: true, Version: "1.3.0"})
		want := "info: Running the pinned teardown binary " + f.stored(binstore.Hash([]byte("release build"))) + " (release 1.2.3) to remove this worktree. " +
			"The running build (1.3.0) is newer than the pinned release (1.2.3). Pin the new build with `treerig teardown pin --latest` to update it."
		if !strings.HasPrefix(f.logged(), want+"\n") && f.logged() != want {
			t.Fatalf("stderr = %q\nwant     %q", f.logged(), want)
		}
	})

	t.Run("says what an older pin may need when there is no treerig.toml for it to read, and only then", func(t *testing.T) {
		const hint = "This pin (1.2.3) may need a treerig.toml: if it stops with \"No treerig.toml found\", run the removal from inside the worktree's repository, or pass --config <that repository's treerig.toml>."
		tomlAbove := func(t *testing.T, f *gateFixture) {
			if err := os.WriteFile(filepath.Join(f.dir, "treerig.toml"), []byte("[project]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		for _, tc := range []struct {
			name    string
			version string
			setup   func(t *testing.T, f *gateFixture)
			want    bool
		}{
			{"a pin older than the running build", "1.3.0", func(t *testing.T, f *gateFixture) { f.releasePin(t, "1.2.3", "release build") }, true},
			{"a pin equal to the running build", "1.2.3", func(t *testing.T, f *gateFixture) { f.releasePin(t, "1.2.3", "release build") }, false},
			{"a pin newer than the running build", "1.2.0", func(t *testing.T, f *gateFixture) { f.releasePin(t, "1.2.3", "release build") }, false},
			{"a release pin under a development build", "0.0.0-dev", func(t *testing.T, f *gateFixture) { f.releasePin(t, "1.2.3", "release build") }, false},
			{"a file pin", "1.3.0", func(t *testing.T, f *gateFixture) { f.pinnedFile(t) }, false},
			{"an older pin with a treerig.toml above the working directory", "1.3.0", func(t *testing.T, f *gateFixture) {
				tomlAbove(t, f)
				f.releasePin(t, "1.2.3", "release build")
			}, false},
			{"an older pin with --config", "1.3.0", func(t *testing.T, f *gateFixture) {
				f.env.ConfigPath = filepath.Join(f.dir, "elsewhere", "treerig.toml")
				f.releasePin(t, "1.2.3", "release build")
			}, false},
		} {
			f := newGateFixture(t)
			tc.setup(t, f)
			_ = f.remove(commands.RemoveOptions{Identifier: "feature-x", Yes: true, Version: tc.version})
			if len(f.calls) != 1 {
				t.Fatalf("%s: calls %v, want the pinned run", tc.name, f.calls)
			}
			if got := strings.Contains(f.logged(), hint); got != tc.want {
				t.Fatalf("%s: hint shown = %v, want %v\nstderr = %q", tc.name, got, tc.want, f.logged())
			}
		}
	})

	t.Run("maps EACCES and ENOEXEC to the noexec message, naming the control home", func(t *testing.T) {
		for _, errno := range []syscall.Errno{syscall.EACCES, syscall.ENOEXEC} {
			f := newGateFixture(t)
			digest := f.pinnedFile(t)
			f.execErr = errno
			err := f.remove(commands.RemoveOptions{Yes: true})
			wantRefusal(t, err, teardownRefusal(thisWorktree, "could not run the pinned teardown binary "+f.stored(digest)+" ("+errno.Error()+"). If the filesystem holding the control home ("+
				filepath.Dir(f.control.TeardownDir)+") cannot run programs, remount it with exec, or move the control home with TREERIG_CONTROL_HOME."))
			if !errors.Is(err, errno) {
				t.Fatalf("err = %v, want it to wrap %v", err, errno)
			}
		}
	})

	t.Run("refuses any other exec failure without the noexec advice", func(t *testing.T) {
		f := newGateFixture(t)
		digest := f.pinnedFile(t)
		f.execErr = syscall.EIO
		wantRefusal(t, f.remove(commands.RemoveOptions{Yes: true}), teardownRefusal(thisWorktree, "could not run the pinned teardown binary "+f.stored(digest)+" ("+syscall.EIO.Error()+"). Pin it again with "+f.pinAgain()+"."))
	})

	t.Run("says /proc is unavailable for an ENOENT on Linux, where the exec goes through /proc/self/fd", func(t *testing.T) {
		f := newGateFixture(t)
		digest := f.pinnedFile(t)
		f.execErr = syscall.ENOENT
		wantRefusal(t, f.remove(commands.RemoveOptions{Yes: true}), teardownRefusal(thisWorktree, "could not run the pinned teardown binary "+f.stored(digest)+" ("+syscall.ENOENT.Error()+
			"). treerig runs the file it verified through /proc/self/fd, and /proc is not available to this process; mount /proc, or run treerig where /proc is available."))
	})

	t.Run("refuses when the exec returns without replacing the process", func(t *testing.T) {
		f := newGateFixture(t)
		f.pinnedFile(t)
		f.execErr = nil
		err := f.remove(commands.RemoveOptions{Yes: true})
		if err == nil || !strings.Contains(err.Error(), "the exec call returned without starting it") {
			t.Fatalf("err = %v, want a refusal", err)
		}
	})

	t.Run("stops, without an exec, when a signal has already cancelled the context", func(t *testing.T) {
		f := newGateFixture(t)
		f.pinnedFile(t)
		cause := errors.New("test: interrupted")
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		err := commands.Remove(ctx, f.env, commands.RemoveOptions{Yes: true, Version: "0.0.0-dev"})
		if !errors.Is(err, cause) || len(f.calls) != 0 {
			t.Fatalf("err = %v, calls %v; want the cause and no exec", err, f.calls)
		}
	})

	t.Run("refuses when it cannot tell which build it is, since it might be the pinned child", func(t *testing.T) {
		f := newGateFixture(t)
		digest := f.pinnedFile(t)
		f.env.Executable = func() (string, error) { return "", errors.New("no /proc") }
		wantRefusal(t, f.remove(commands.RemoveOptions{Yes: true}), teardownRefusal(thisWorktree, "could not tell which treerig build this is (no /proc)."))
		// A file named like a stored binary that cannot be read is the same.
		if os.Geteuid() != 0 {
			if err := os.Chmod(f.stored(digest), 0); err != nil {
				t.Fatal(err)
			}
			f.asPinnedChild(digest)
			err := f.remove(commands.RemoveOptions{Yes: true})
			if err == nil || !strings.HasPrefix(err.Error(), "Refusing to remove this worktree: could not tell which treerig build this is (could not read ") {
				t.Fatalf("err = %v", err)
			}
		}
		if len(f.calls) != 0 {
			t.Fatalf("exec was called: %v", f.calls)
		}
	})

	t.Run("a build whose file is merely missing or unnamed like a stored binary is not mistaken for the child", func(t *testing.T) {
		f := newGateFixture(t)
		f.pinnedFile(t)
		f.env.Executable = func() (string, error) { return filepath.Join(f.dir, "missing"), nil }
		_ = f.remove(commands.RemoveOptions{Yes: true})
		if len(f.calls) != 1 {
			t.Fatalf("exec calls = %d, want the pinned binary run", len(f.calls))
		}
	})

	t.Run("a pinned child proceeds when the pin names it, and says so", func(t *testing.T) {
		f := newGateFixture(t)
		digest := f.pinnedFile(t)
		f.asPinnedChild(digest)
		err := f.remove(commands.RemoveOptions{Yes: true})
		if err != nil && strings.Contains(err.Error(), "Refusing") {
			t.Fatalf("err = %v, want the gate to let the removal go on", err)
		}
		if len(f.calls) != 0 {
			t.Fatalf("a pinned child exec-ed again: %v", f.calls)
		}
		want := "info: Running as the pinned teardown binary (version 0.0.0-dev)."
		if !strings.HasPrefix(f.logged(), want) {
			t.Fatalf("stderr = %q, want it to start with %q", f.logged(), want)
		}
	})

	t.Run("a pinned child refuses --dev-teardown", func(t *testing.T) {
		f := newGateFixture(t)
		f.asPinnedChild(f.pinnedFile(t))
		wantRefusal(t, f.remove(commands.RemoveOptions{Yes: true, DevTeardown: true}), "Refusing --dev-teardown: this is a pinned teardown binary. Nothing was removed.")
	})

	t.Run("a stored binary the pin no longer names refuses, whatever the reason", func(t *testing.T) {
		f := newGateFixture(t)
		digest := f.pinnedFile(t)
		f.asPinnedChild(digest)
		// The pin is replaced by one that names another binary.
		f.releasePin(t, "2.0.0", "another release")
		want := teardownRefusal(thisWorktree, "this teardown binary "+f.stored(digest)+" is not the one the pin names, or the pin was replaced just now. Run this again; if it refuses again, pin it again with `treerig teardown pin --release 2.0.0`.")
		// releasePin's sweep keeps the superseded file inside its grace window.
		if _, err := os.Stat(f.stored(digest)); err != nil {
			t.Fatalf("the superseded binary is gone: %v", err)
		}
		wantRefusal(t, f.remove(commands.RemoveOptions{Yes: true}), want)
	})

	t.Run("a pinned child with no pin refuses", func(t *testing.T) {
		f := newGateFixture(t)
		digest := f.pinnedFile(t)
		f.asPinnedChild(digest)
		if err := os.Remove(f.control.PinPath); err != nil {
			t.Fatal(err)
		}
		wantRefusal(t, f.remove(commands.RemoveOptions{Yes: true}), teardownRefusal(thisWorktree, "this teardown binary "+f.stored(digest)+" is not the one the pin names, or the pin was replaced just now. Run this again; if it refuses again, pin it again with "+pinCommand+"."))
	})

	t.Run("a stored file whose bytes are not its name is not a pinned child", func(t *testing.T) {
		f := newGateFixture(t)
		digest := f.pinnedFile(t)
		f.asPinnedChild(digest)
		if err := os.WriteFile(f.stored(digest), []byte("other bytes"), 0o700); err != nil {
			t.Fatal(err)
		}
		// Not a child, so it is gated like any invoked build: the stored file fails its hash.
		err := f.remove(commands.RemoveOptions{Yes: true})
		if err == nil || !strings.Contains(err.Error(), "has sha256 "+binstore.Hash([]byte("other bytes"))+", not the pinned "+digest) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a bin copy without the flag is never trusted as the pin", func(t *testing.T) {
		f := newGateFixture(t)
		content := teardowntestExecutable("a relocated build")
		bin := filepath.Join(f.control.BinDir, binstore.Hash(content))
		if err := os.MkdirAll(f.control.BinDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, content, 0o700); err != nil {
			t.Fatal(err)
		}
		f.env.Executable = func() (string, error) { return bin, nil }
		err := f.remove(commands.RemoveOptions{Yes: true})
		if err == nil || !strings.HasPrefix(err.Error(), "Refusing to remove this worktree: this is a development build") {
			t.Fatalf("err = %v, want the no-pin refusal", err)
		}
	})
}

func TestDevTeardown(t *testing.T) {
	t.Run("warns on every run, then relocates through bin and execs the copy with the flag in argv", func(t *testing.T) {
		f := newGateFixture(t)
		f.env.Args = []string{"trig", "worktree", "remove", "--dev-teardown", "--yes"}
		f.execErr = nil
		err := f.remove(commands.RemoveOptions{Yes: true, DevTeardown: true, Version: "9.9.9"})
		if err != nil && strings.Contains(err.Error(), "Refusing") {
			t.Fatalf("err = %v", err)
		}
		hash, herr := binstore.HashFile(f.running)
		if herr != nil {
			t.Fatal(herr)
		}
		warning := "warn: Removing with this build's own code (version 9.9.9, sha256 " + hash + "), not a pinned teardown binary: code from this build runs during the removal."
		lines := strings.Split(f.logged(), "\n")
		if lines[0] != warning {
			t.Fatalf("first stderr line = %q\nwant %q", lines[0], warning)
		}
		if len(f.calls) != 1 || f.calls[0].path != filepath.Join(f.control.BinDir, hash) || strings.Join(f.calls[0].argv, " ") != "trig worktree remove --dev-teardown --yes" {
			t.Fatalf("exec calls = %+v, want the bin copy with the caller's argv", f.calls)
		}
	})

	t.Run("is allowed on a release build, with or without a pin, and bypasses the pin with the same warning", func(t *testing.T) {
		for _, pinned := range []bool{false, true} {
			f := newGateFixture(t)
			if pinned {
				f.pinnedFile(t)
			}
			f.env.Args = []string{"trig", "worktree", "remove", "--dev-teardown", "--yes"}
			f.execErr = nil
			// The fake exec returns nil, so the relocation "succeeds" and the removal goes on.
			if err := f.remove(commands.RemoveOptions{Yes: true, DevTeardown: true, Version: "0.1.0-rc.3"}); err == nil || !strings.Contains(err.Error(), "There is no worktree to remove here") {
				t.Fatalf("pinned=%v: err = %v, want the removal to go on to find no worktree here", pinned, err)
			}
			hash, _ := binstore.HashFile(f.running)
			if !strings.HasPrefix(f.logged(), "warn: Removing with this build's own code (version 0.1.0-rc.3, sha256 "+hash+")") {
				t.Fatalf("pinned=%v: stderr = %q, want the warning", pinned, f.logged())
			}
			if len(f.calls) != 1 || f.calls[0].path != filepath.Join(f.control.BinDir, hash) {
				t.Fatalf("pinned=%v: exec calls = %+v, want the relocation, not the pin", pinned, f.calls)
			}
		}
	})

	t.Run("works with no pin at all, and is not mistaken for one", func(t *testing.T) {
		f := newGateFixture(t)
		err := f.remove(commands.RemoveOptions{Yes: true, DevTeardown: true})
		if err != nil && strings.Contains(err.Error(), "Refusing") {
			t.Fatalf("err = %v, want --dev-teardown to need no pin", err)
		}
	})

	t.Run("the relocated copy is silent and goes on", func(t *testing.T) {
		f := newGateFixture(t)
		content, err := os.ReadFile(f.running)
		if err != nil {
			t.Fatal(err)
		}
		hash := binstore.Hash(content)
		if err := os.MkdirAll(f.control.BinDir, 0o700); err != nil {
			t.Fatal(err)
		}
		bin := filepath.Join(f.control.BinDir, hash)
		if err := os.WriteFile(bin, content, 0o700); err != nil {
			t.Fatal(err)
		}
		f.env.Executable = func() (string, error) { return bin, nil }
		err = f.remove(commands.RemoveOptions{Yes: true, DevTeardown: true})
		if err != nil && strings.Contains(err.Error(), "Refusing") {
			t.Fatalf("err = %v", err)
		}
		if f.logged() != "" || len(f.calls) != 0 {
			t.Fatalf("stderr %q, exec calls %v; want a silent copy that does not exec again", f.logged(), f.calls)
		}
	})
}

// teardowntestExecutable is a linux_amd64 executable whose bytes carry extra,
// so that two of them hash differently.
func teardowntestExecutable(extra string) []byte {
	return teardowntest.Executable("linux_amd64", " "+extra)
}

func TestRepinCommandIsRunnableWhateverThePathAndLabel(t *testing.T) {
	f := newTeardownFixture(t)
	spaced := filepath.Join(f.dir, "sp dir", "my treerig")
	if err := os.MkdirAll(filepath.Dir(spaced), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spaced, f.content, 0o755); err != nil {
		t.Fatal(err)
	}
	options := f.pinOptions()
	options.File = spaced
	if err := f.pin(options); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.stored(f.digest)); err != nil {
		t.Fatal(err)
	}
	want := "Pin it again with `treerig teardown pin --file '" + spaced + "' --sha256 " + f.digest + " --release my-build`."
	err := f.status(false, "0.0.0-dev")
	if err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("status error = %v, want it to end %q", err, want)
	}
}

func TestTeardownWarnsWhenRemovalWouldRefuse(t *testing.T) {
	noexec := func(path string) (bool, error) { return true, nil }
	advice := func(f *teardownFixture) string {
		return "warn: The filesystem holding the control home (" + f.control.ControlHome + ") is mounted noexec, so the pinned teardown binary cannot run and `treerig worktree remove` will refuse. " +
			"Remount it with exec, or move the control home with TREERIG_CONTROL_HOME."
	}

	t.Run("pin warns after pinning when the control home is mounted noexec, and not otherwise", func(t *testing.T) {
		f := newTeardownFixture(t)
		if err := f.pin(f.pinOptions()); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(f.logged(), "noexec") {
			t.Fatalf("stderr = %q, want no warning on a mount that can run programs", f.logged())
		}
		g := newTeardownFixture(t)
		g.env.NoExec = noexec
		if err := g.pin(g.pinOptions()); err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(g.logged(), advice(g)) {
			t.Fatalf("stderr = %q, want it to end with the warning", g.logged())
		}
	})

	t.Run("status warns, and its closing line says removal would refuse", func(t *testing.T) {
		f := newTeardownFixture(t)
		if err := f.pin(f.pinOptions()); err != nil {
			t.Fatal(err)
		}
		f.log.Lines, f.log.Printed = nil, nil
		f.env.NoExec = noexec
		wantRefusal(t, f.status(false, "0.0.0-dev"), "The pin is intact, but `treerig worktree remove` would refuse; see the warning above.")
		if f.logged() != advice(f) || !strings.HasSuffix(f.printed(), "\nworktree remove: would refuse (see the warning below)") {
			t.Fatalf("stdout = %q, stderr = %q", f.printed(), f.logged())
		}
	})

	t.Run("status --json keeps stdout a document and puts the same warning on stderr", func(t *testing.T) {
		f := newTeardownFixture(t)
		if err := f.pin(f.pinOptions()); err != nil {
			t.Fatal(err)
		}
		f.log.Lines, f.log.Printed = nil, nil
		f.env.NoExec = noexec
		wantRefusal(t, f.status(true, "0.0.0-dev"), "The pin is intact, but `treerig worktree remove` would refuse; see the warning above.")
		var document struct {
			Pinned  bool   `json:"pinned"`
			OK      bool   `json:"ok"`
			Removal string `json:"removal"`
		}
		if err := json.Unmarshal([]byte(f.printed()), &document); err != nil || !document.Pinned || !document.OK || document.Removal != "refuse" {
			t.Fatalf("stdout is not just the status document (%v): %q", err, f.printed())
		}
		if f.logged() != advice(f) {
			t.Fatalf("stderr = %q, want exactly the noexec warning", f.logged())
		}
	})

	t.Run("status warns about a control home inside a checkout or a store others can write", func(t *testing.T) {
		f := newTeardownFixture(t)
		if err := f.pin(f.pinOptions()); err != nil {
			t.Fatal(err)
		}
		f.log.Lines, f.log.Printed = nil, nil
		if err := os.Chmod(f.control.PinPath, 0o666); err != nil {
			t.Fatal(err)
		}
		wantRefusal(t, f.status(false, "0.0.0-dev"), "The pin is intact, but `treerig worktree remove` would refuse; see the warning above.")
		want := "warn: " + f.control.PinPath + " can be written by its group or others (mode 0666), so the teardown pin is not private to you. Do one of these: fix its owner and mode; delete " + f.control.TeardownDir + "/ and pin again with " + pinCommand + "; or move the control home to a local filesystem with TREERIG_CONTROL_HOME."
		if f.logged() != want || !strings.HasSuffix(f.printed(), "\nworktree remove: would refuse (see the warning below)") {
			t.Fatalf("stdout = %q, stderr = %q", f.printed(), f.logged())
		}
	})
}
