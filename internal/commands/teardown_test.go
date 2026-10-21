package commands_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/binstore"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/commands"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/logger"
	"github.com/sofired/treerig/internal/registry/registrytest"
	"github.com/sofired/treerig/internal/stateformat"
	"github.com/sofired/treerig/internal/teardown"
	"github.com/sofired/treerig/internal/teardown/teardowntest"
)

// teardownTime is the clock every teardown test pins at.
var teardownTime = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// teardownFixture is a scratch treerig root with an Env over it, a recording
// logger, a stderr buffer for the confirmation screen and a local binary to
// pin.
type teardownFixture struct {
	env     *commands.Env
	log     *logger.Recording
	stderr  *strings.Builder
	control layout.Control
	home    string
	// dir holds the local binary and is the working directory.
	dir string
	// file and content are the local binary's path and bytes; digest is its sha256.
	file    string
	content []byte
	digest  string
}

// newTeardownFixture builds a fixture on linux_amd64 with no terminal.
func newTeardownFixture(t *testing.T) *teardownFixture {
	t.Helper()
	base := registrytest.TempDir(t)
	f := &teardownFixture{
		log:     &logger.Recording{},
		stderr:  &strings.Builder{},
		home:    filepath.Join(base, "home"),
		dir:     filepath.Join(base, "work"),
		content: teardowntest.Executable("linux_amd64", " a treerig build to pin"),
	}
	treerigHome := filepath.Join(base, "treerig-home")
	for _, dir := range []string{f.home, f.dir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.control = layout.ControlLayout(filepath.Join(treerigHome, "control-home"))
	f.file = filepath.Join(f.dir, "treerig-local")
	if err := os.WriteFile(f.file, f.content, 0o755); err != nil {
		t.Fatal(err)
	}
	f.digest = binstore.Hash(f.content)
	f.env = &commands.Env{
		CommandName: "treerig",
		Logger:      f.log,
		Getenv: func(key string) (string, bool) {
			switch key {
			case "HOME":
				return f.home, true
			case "TREERIG_HOME":
				return treerigHome, true
			}
			return "", false
		},
		OS:     "linux",
		Arch:   "amd64",
		Cwd:    f.dir,
		Stderr: f.stderr,
	}
	return f
}

// pinOptions are the flags of a valid `teardown pin --file` for f's binary.
func (f *teardownFixture) pinOptions() commands.TeardownPinOptions {
	return commands.TeardownPinOptions{File: f.file, SHA256: f.digest, Release: "my-build", Yes: true, Now: func() time.Time { return teardownTime }}
}

// pin runs `teardown pin` with options.
func (f *teardownFixture) pin(options commands.TeardownPinOptions) error {
	return commands.TeardownPin(context.Background(), f.env, options)
}

// terminal gives f's Env a terminal that types answer.
func (f *teardownFixture) terminal(answer string) {
	f.env.StdinIsTTY = true
	f.env.Stdin = strings.NewReader(answer)
}

// status runs `teardown status` as version.
func (f *teardownFixture) status(json bool, version string) error {
	return commands.TeardownStatus(context.Background(), f.env, commands.TeardownStatusOptions{JSON: json, Version: version})
}

// printed is what the last command printed on stdout, joined by newlines.
func (f *teardownFixture) printed() string {
	_, printed := f.log.Snapshot()
	return strings.Join(printed, "\n")
}

// logged is what the last command logged on stderr, joined by newlines.
func (f *teardownFixture) logged() string {
	lines, _ := f.log.Snapshot()
	return strings.Join(lines, "\n")
}

// readPin reads the pin on disk.
func (f *teardownFixture) readPin(t *testing.T) teardown.Pin {
	t.Helper()
	pin, found, err := teardown.ReadPin(f.control)
	if err != nil || !found {
		t.Fatalf("ReadPin = %v, %v", found, err)
	}
	return pin
}

// stored is the path the binary with digest is stored at.
func (f *teardownFixture) stored(digest string) string { return teardown.BinaryPath(f.control, digest) }

// releasePin writes a release pin of release whose linux_amd64 binary is
// content, through the same writer the commands use.
func (f *teardownFixture) releasePin(t *testing.T, release, content string) {
	t.Helper()
	sum := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return hex.EncodeToString(h[:])
	}
	pin := teardown.Pin{
		Release:         release,
		Source:          "https://github.com/sofired/treerig/releases/download/v" + release + "/",
		Signer:          "https://github.com/sofired/treerig/.github/workflows/release.yml@refs/tags/v" + release,
		ChecksumsSHA256: sum("checksums " + release),
		PinnedAt:        teardown.FormatTime(teardownTime),
		Platforms:       map[string]teardown.Platform{},
	}
	for _, key := range teardown.ReleasePlatforms {
		pin.Platforms[key] = teardown.Platform{ArchiveSHA256: sum("archive " + key)}
	}
	pin.Platforms["linux_amd64"] = teardown.Platform{ArchiveSHA256: pin.Platforms["linux_amd64"].ArchiveSHA256, BinarySHA256: binstore.Hash([]byte(content))}
	if _, err := teardown.Save(f.control, f.home, []byte(content), pin, teardownTime); err != nil {
		t.Fatal(err)
	}
}

// wantRefusal asserts err is an expected failure with exactly message.
func wantRefusal(t *testing.T, err error, message string) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %q", message)
	}
	if !clierr.Expected(err) || clierr.Code(err) != 1 {
		t.Fatalf("err = %#v, want an expected failure with exit 1", err)
	}
	if err.Error() != message {
		t.Fatalf("error = %q\nwant    %q", err.Error(), message)
	}
}

// pinAgain is the command that pins f's file pin again: the file, the digest it
// recorded and its label.
func (f *teardownFixture) pinAgain() string {
	return "`treerig teardown pin --file " + f.file + " --sha256 " + f.digest + " --release my-build`"
}

// pinCommand is the generic command the messages name for a development build,
// which has no release version to put in it.
const pinCommand = "`treerig teardown pin --release <a published version, such as 0.1.0-rc.2>` (or `--file`; see `treerig teardown pin --help`)"

func TestTeardownPinFile(t *testing.T) {
	t.Run("pins a local binary: stores the same bytes under their hash and writes a file pin for this platform", func(t *testing.T) {
		f := newTeardownFixture(t)
		if err := f.pin(f.pinOptions()); err != nil {
			t.Fatal(err)
		}
		pin := f.readPin(t)
		want := teardown.Pin{
			Version: stateformat.TeardownPin, Release: "my-build", Source: "file:" + f.file,
			PinnedAt:  "2026-10-14T12:00:00Z",
			Platforms: map[string]teardown.Platform{"linux_amd64": {BinarySHA256: f.digest}},
		}
		if pin.Version != want.Version || pin.Release != want.Release || pin.Source != want.Source || pin.Signer != "" ||
			pin.ChecksumsSHA256 != "" || pin.PinnedAt != want.PinnedAt || len(pin.Platforms) != 1 || pin.Platforms["linux_amd64"] != want.Platforms["linux_amd64"] {
			t.Fatalf("pin = %+v, want %+v", pin, want)
		}
		got, err := os.ReadFile(f.stored(f.digest))
		if err != nil || string(got) != string(f.content) {
			t.Fatalf("stored = %q, %v", got, err)
		}
		info, err := os.Stat(f.stored(f.digest))
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("stored mode = %v, %v", info, err)
		}
		if f.printed() != "" {
			t.Fatalf("stdout = %q, want nothing", f.printed())
		}
		if want := "info: Pinned the teardown binary: release my-build (linux_amd64), stored at " + f.stored(f.digest) + "."; f.logged() != want {
			t.Fatalf("log = %q\nwant  %q", f.logged(), want)
		}
	})

	t.Run("resolves a relative --file against the working directory", func(t *testing.T) {
		f := newTeardownFixture(t)
		options := f.pinOptions()
		options.File = "./sub/../treerig-local"
		if err := f.pin(options); err != nil {
			t.Fatal(err)
		}
		if got := f.readPin(t).Source; got != "file:"+f.file {
			t.Fatalf("source = %q, want the absolute, cleaned path", got)
		}
	})

	t.Run("refuses a --sha256 that does not match the file, and writes nothing", func(t *testing.T) {
		f := newTeardownFixture(t)
		wrong := binstore.Hash([]byte("something else"))
		options := f.pinOptions()
		options.SHA256 = wrong
		wantRefusal(t, f.pin(options), "Refusing to pin my-build: "+f.file+" has sha256 "+f.digest+", not the "+wrong+" you supplied. Nothing was pinned. Check that you are pinning the file you verified and that --sha256 is its digest.")
		if _, err := os.Lstat(f.control.TeardownDir); !os.IsNotExist(err) {
			t.Fatalf("teardown directory: %v, want none", err)
		}
	})

	t.Run("accepts the digest in capitals", func(t *testing.T) {
		f := newTeardownFixture(t)
		options := f.pinOptions()
		options.SHA256 = strings.ToUpper(f.digest)
		if err := f.pin(options); err != nil {
			t.Fatal(err)
		}
		if got := f.readPin(t).Platforms["linux_amd64"].BinarySHA256; got != f.digest {
			t.Fatalf("binary_sha256 = %q", got)
		}
	})

	t.Run("hashes and installs the same bytes it read", func(t *testing.T) {
		f := newTeardownFixture(t)
		// The file changes while the prompt is up: what was hashed is still
		// what is installed, and the pin names that hash.
		f.env.StdinIsTTY = true
		f.env.Stdin = readerFunc(func() string {
			if err := os.WriteFile(f.file, []byte("swapped after the hash"), 0o755); err != nil {
				t.Error(err)
			}
			return "pin teardown binary\n"
		})
		options := f.pinOptions()
		options.Yes = false
		if err := f.pin(options); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(f.stored(f.digest))
		if err != nil || string(got) != string(f.content) {
			t.Fatalf("stored = %q, %v", got, err)
		}
		if f.readPin(t).Platforms["linux_amd64"].BinarySHA256 != f.digest {
			t.Fatal("the pin does not name the bytes that were hashed")
		}
	})

	t.Run("refuses the flags it is missing or given wrongly", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			edit    func(*commands.TeardownPinOptions)
			message string
		}{
			{"no --file, but a digest", func(o *commands.TeardownPinOptions) { o.File = "" }, "--sha256 can only be used with --file."},
			{"no --sha256", func(o *commands.TeardownPinOptions) { o.SHA256 = "" }, "Missing required flag: --sha256."},
			{"no --release", func(o *commands.TeardownPinOptions) { o.Release = "" }, "Missing required flag: --release."},
			{"--file alone", func(o *commands.TeardownPinOptions) { o.SHA256, o.Release = "", "" }, "Missing required flags: --sha256, --release."},
			{"nothing at all asks for a source", func(o *commands.TeardownPinOptions) { *o = commands.TeardownPinOptions{} }, "Missing a source to pin: pass --release X.Y.Z or --latest to download and verify a release, or --file with --sha256 and --release to pin a binary you have verified."},
			{"only --yes asks for a source", func(o *commands.TeardownPinOptions) { *o = commands.TeardownPinOptions{Yes: true} }, "Missing a source to pin: pass --release X.Y.Z or --latest to download and verify a release, or --file with --sha256 and --release to pin a binary you have verified."},
			{"--file with --latest", func(o *commands.TeardownPinOptions) { o.Latest = true }, "--latest cannot be used with --file: a file pin is for a binary you verified yourself."},
			{"--release with --latest", func(o *commands.TeardownPinOptions) { *o = commands.TeardownPinOptions{Release: "1.2.3", Latest: true} }, "--release and --latest cannot be used together."},
			{"a download of something that is not a version", func(o *commands.TeardownPinOptions) { *o = commands.TeardownPinOptions{Release: "my-build"} }, "--release must be a version such as 1.2.3 or 0.1.0-rc.1, without a leading v."},
			{"a download of a v-prefixed version", func(o *commands.TeardownPinOptions) { *o = commands.TeardownPinOptions{Release: "v1.2.3"} }, "--release must be a version such as 1.2.3 or 0.1.0-rc.1, without a leading v."},
			{"a download of a version with a leading zero", func(o *commands.TeardownPinOptions) { *o = commands.TeardownPinOptions{Release: "01.2.3"} }, "--release must be a version such as 1.2.3 or 0.1.0-rc.1, without a leading v."},
			{"a short digest", func(o *commands.TeardownPinOptions) { o.SHA256 = "abc" }, "--sha256 must be 64 hexadecimal characters."},
			{"a digest that is not hex", func(o *commands.TeardownPinOptions) { o.SHA256 = strings.Repeat("g", 64) }, "--sha256 must be 64 hexadecimal characters."},
			{"a label with a slash", func(o *commands.TeardownPinOptions) { o.Release = "a/b" }, "--release must be 1 to 64 characters: letters, digits, '.', '_', '+' and '-', starting with a letter or a digit."},
			{"a label starting with a dot", func(o *commands.TeardownPinOptions) { o.Release = ".x" }, "--release must be 1 to 64 characters: letters, digits, '.', '_', '+' and '-', starting with a letter or a digit."},
			{"a label of 65 characters", func(o *commands.TeardownPinOptions) { o.Release = strings.Repeat("a", 65) }, "--release must be 1 to 64 characters: letters, digits, '.', '_', '+' and '-', starting with a letter or a digit."},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newTeardownFixture(t)
				options := f.pinOptions()
				tc.edit(&options)
				wantRefusal(t, f.pin(options), tc.message)
			})
		}
	})

	t.Run("accepts a 64-character label", func(t *testing.T) {
		f := newTeardownFixture(t)
		options := f.pinOptions()
		options.Release = strings.Repeat("a", 64)
		if err := f.pin(options); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("refuses a file it cannot read, a directory, and a path with control characters", func(t *testing.T) {
		f := newTeardownFixture(t)
		options := f.pinOptions()
		options.File = filepath.Join(f.dir, "missing")
		wantRefusal(t, f.pin(options), "Could not read "+options.File+": no such file or directory.")
		options.File = f.dir
		wantRefusal(t, f.pin(options), "Could not read "+f.dir+": it is not a regular file.")
		options.File = f.dir + "/a\x1b[2Kb"
		wantRefusal(t, f.pin(options), "Refusing to pin my-build: the path "+f.dir+"/a�[2Kb has control characters in it.")
	})

	t.Run("refuses a file that is not an executable for this platform, even when its digest matches", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			content []byte
			reason  string
		}{
			{"an empty file", nil, "the file is empty"},
			{"a script", []byte("#!/bin/sh\necho hi\n"), "not an ELF file"},
			{"an ELF for arm64", teardowntest.Executable("linux_arm64", ""), "an ELF file for EM_AARCH64, not EM_X86_64"},
			{"a Mach-O", teardowntest.Executable("darwin_amd64", ""), "not an ELF file"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newTeardownFixture(t)
				if err := os.WriteFile(f.file, tc.content, 0o755); err != nil {
					t.Fatal(err)
				}
				options := f.pinOptions()
				options.SHA256 = binstore.Hash(tc.content)
				wantRefusal(t, f.pin(options), "Refusing to pin my-build: "+f.file+" is not a linux_amd64 executable ("+tc.reason+"). Nothing was pinned. Pin the treerig binary built for this machine.")
				if _, err := os.Lstat(f.control.TeardownDir); !os.IsNotExist(err) {
					t.Fatalf("teardown directory: %v", err)
				}
			})
		}
	})

	t.Run("takes a fat Mach-O that holds this architecture on darwin", func(t *testing.T) {
		f := newTeardownFixture(t)
		f.env.OS, f.env.Arch = "darwin", "arm64"
		fat := teardowntest.Fat(teardowntest.Executable("darwin_amd64", ""), teardowntest.Executable("darwin_arm64", ""))
		if err := os.WriteFile(f.file, fat, 0o755); err != nil {
			t.Fatal(err)
		}
		options := f.pinOptions()
		options.SHA256 = binstore.Hash(fat)
		if err := f.pin(options); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("refuses a FIFO without opening it, so it cannot hang", func(t *testing.T) {
		f := newTeardownFixture(t)
		fifo := filepath.Join(f.dir, "fifo")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		options := f.pinOptions()
		options.File = fifo
		wantRefusal(t, f.pin(options), "Could not read "+fifo+": it is not a regular file.")
	})

	t.Run("refuses a file over the size cap, and takes one at it", func(t *testing.T) {
		f := newTeardownFixture(t)
		defer commands.SetMaxTeardownBinaryForTest(int64(len(f.content)) - 1)()
		wantRefusal(t, f.pin(f.pinOptions()), "Refusing to pin "+f.file+": it is larger than "+strconv.Itoa(len(f.content)-1)+" bytes.")
		restore := commands.SetMaxTeardownBinaryForTest(int64(len(f.content)))
		defer restore()
		if err := f.pin(f.pinOptions()); err != nil {
			t.Fatalf("a file exactly at the cap: %v", err)
		}
	})

	t.Run("stamps pinned_at with the clock read after the confirmation", func(t *testing.T) {
		f := newTeardownFixture(t)
		clock := teardownTime
		f.env.StdinIsTTY = true
		f.env.Stdin = readerFunc(func() string {
			clock = clock.Add(5 * time.Minute) // the prompt is up for a while
			return "pin teardown binary\n"
		})
		options := f.pinOptions()
		options.Yes = false
		options.Now = func() time.Time { return clock }
		if err := f.pin(options); err != nil {
			t.Fatal(err)
		}
		if got, want := f.readPin(t).PinnedAt, teardown.FormatTime(teardownTime.Add(5*time.Minute)); got != want {
			t.Fatalf("pinned_at = %s, want %s", got, want)
		}
	})

	t.Run("refuses a platform treerig does not build", func(t *testing.T) {
		f := newTeardownFixture(t)
		f.env.Arch = "riscv64"
		wantRefusal(t, f.pin(f.pinOptions()), "Refusing to pin my-build: linux_riscv64 is not a platform treerig builds (linux and darwin, on amd64 and arm64).")
	})

	t.Run("replaces an existing pin, naming the release it replaces in the plan", func(t *testing.T) {
		f := newTeardownFixture(t)
		f.releasePin(t, "1.1.0", "an older release")
		f.terminal("pin teardown binary\n")
		options := f.pinOptions()
		options.Yes = false
		if err := f.pin(options); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(f.stderr.String(), "release   my-build  (current pin: 1.1.0)") {
			t.Fatalf("plan = %q", f.stderr.String())
		}
		pin := f.readPin(t)
		if !pin.IsFile() || pin.Release != "my-build" || len(pin.Platforms) != 1 {
			t.Fatalf("pin = %+v, want the file pin to replace the release pin whole", pin)
		}
	})

	t.Run("refuses a newer or foreign pin and leaves it as it is", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			body string
			is   error
		}{
			{"newer", `{"version": 99}`, stateformat.ErrNewer},
			{"foreign", `{"version": 1, "extra": true}`, teardown.ErrForeign},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newTeardownFixture(t)
				if err := os.MkdirAll(f.control.TeardownDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.control.PinPath, []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
				err := f.pin(f.pinOptions())
				if !errors.Is(err, tc.is) {
					t.Fatalf("err = %v, want %v", err, tc.is)
				}
				if got, _ := os.ReadFile(f.control.PinPath); string(got) != tc.body {
					t.Fatalf("pin.json = %q, want it untouched", got)
				}
				if _, statErr := os.Lstat(f.stored(f.digest)); !os.IsNotExist(statErr) {
					t.Fatalf("binary installed anyway: %v", statErr)
				}
			})
		}
	})
}

// readerFunc is an io.Reader that runs a function on its first Read and then
// serves what it returned: a way to act while a prompt waits for an answer.
type readerFunc func() string

// Read serves the function's answer, running it first.
func (r readerFunc) Read(p []byte) (int, error) {
	answer := r()
	return copy(p, answer), nil
}

func TestTeardownPinConfirmation(t *testing.T) {
	t.Run("shows the plan on stderr and the phrase to type, then pins on the right answer", func(t *testing.T) {
		f := newTeardownFixture(t)
		f.terminal("pin teardown binary\n")
		options := f.pinOptions()
		options.Yes = false
		if err := f.pin(options); err != nil {
			t.Fatal(err)
		}
		want := "Pin the teardown binary\n" +
			"  release   my-build  (current pin: none)\n" +
			"  source    file:" + f.file + "\n" +
			"  signer    none (digest supplied by you); no signature was checked\n" +
			"  platform  linux_amd64  sha256 " + f.digest + "\n" +
			"  `worktree remove` will run this binary instead of the build you invoke.\n\n" +
			"Type this exact phrase to continue:\n\n  pin teardown binary\n\n> "
		if f.stderr.String() != want {
			t.Fatalf("stderr =\n%s\nwant\n%s", f.stderr.String(), want)
		}
		f.readPin(t)
	})

	t.Run("refuses without a terminal, and pins nothing", func(t *testing.T) {
		f := newTeardownFixture(t)
		options := f.pinOptions()
		options.Yes = false
		wantRefusal(t, f.pin(options), `Interactive confirmation required: "pin teardown binary".`)
		if _, err := os.Lstat(f.control.PinPath); !os.IsNotExist(err) {
			t.Fatalf("pin.json: %v", err)
		}
	})

	t.Run("cancels on a wrong answer, and pins nothing", func(t *testing.T) {
		f := newTeardownFixture(t)
		f.terminal("yes\n")
		options := f.pinOptions()
		options.Yes = false
		wantRefusal(t, f.pin(options), "Confirmation did not match; operation cancelled.")
		if _, err := os.Lstat(f.control.PinPath); !os.IsNotExist(err) {
			t.Fatalf("pin.json: %v", err)
		}
	})

	t.Run("--yes skips the phrase and the plan", func(t *testing.T) {
		f := newTeardownFixture(t)
		f.terminal("")
		if err := f.pin(f.pinOptions()); err != nil {
			t.Fatal(err)
		}
		if f.stderr.String() != "" {
			t.Fatalf("stderr = %q, want no prompt", f.stderr.String())
		}
	})

	t.Run("reads the pin again after the confirmation, so a pin that became newer meanwhile is not overwritten", func(t *testing.T) {
		f := newTeardownFixture(t)
		f.env.StdinIsTTY = true
		f.env.Stdin = readerFunc(func() string {
			if err := os.MkdirAll(f.control.TeardownDir, 0o700); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(f.control.PinPath, []byte(`{"version": 99}`), 0o600); err != nil {
				t.Error(err)
			}
			return "pin teardown binary\n"
		})
		options := f.pinOptions()
		options.Yes = false
		err := f.pin(options)
		if !errors.Is(err, stateformat.ErrNewer) {
			t.Fatalf("err = %v, want the newer-format refusal", err)
		}
		if got, _ := os.ReadFile(f.control.PinPath); string(got) != `{"version": 99}` {
			t.Fatalf("pin.json = %q, want the newer pin untouched", got)
		}
	})
}

func TestTeardownStatus(t *testing.T) {
	t.Run("with no pin prints nothing on stdout and refuses", func(t *testing.T) {
		f := newTeardownFixture(t)
		wantRefusal(t, f.status(false, "0.0.0-dev"), "No teardown binary is pinned. Pin one with "+pinCommand+".")
		if f.printed() != "" {
			t.Fatalf("stdout = %q", f.printed())
		}
	})

	t.Run("with no pin and --json prints {pinned: false} and refuses", func(t *testing.T) {
		f := newTeardownFixture(t)
		wantRefusal(t, f.status(true, "0.0.0-dev"), "No teardown binary is pinned. Pin one with "+pinCommand+".")
		if got := f.printed(); got != "{\n  \"pinned\": false\n}" {
			t.Fatalf("stdout = %q", got)
		}
	})

	t.Run("reports an intact file pin and succeeds", func(t *testing.T) {
		f := newTeardownFixture(t)
		if err := f.pin(f.pinOptions()); err != nil {
			t.Fatal(err)
		}
		f.log.Lines, f.log.Printed = nil, nil
		if err := f.status(false, "0.0.0-dev"); err != nil {
			t.Fatal(err)
		}
		want := "Teardown pin: release my-build (linux_amd64)\n" +
			"  source  file:" + f.file + "\n" +
			"  signer  none (digest supplied by you)\n" +
			"  binary  " + f.stored(f.digest) + " sha256 " + f.digest + " ok\n" +
			"  pinned  2026-10-14T12:00:00Z\n" +
			"worktree remove: will run this binary"
		if f.printed() != want {
			t.Fatalf("stdout =\n%s\nwant\n%s", f.printed(), want)
		}
		if f.logged() != "" {
			t.Fatalf("stderr = %q, want nothing", f.logged())
		}
	})

	t.Run("reports an intact release pin with its signer and says the attestation was not checked", func(t *testing.T) {
		f := newTeardownFixture(t)
		f.releasePin(t, "1.2.3", "release build")
		if err := f.status(false, "1.2.3"); err != nil {
			t.Fatal(err)
		}
		digest := binstore.Hash([]byte("release build"))
		want := "Teardown pin: release 1.2.3 (linux_amd64)\n" +
			"  source       https://github.com/sofired/treerig/releases/download/v1.2.3/\n" +
			"  signer       https://github.com/sofired/treerig/.github/workflows/release.yml@refs/tags/v1.2.3\n" +
			"  binary       " + f.stored(digest) + " sha256 " + digest + " ok\n" +
			"  pinned       2026-10-14T12:00:00Z\n" +
			"  attestation  not checked\n" +
			"worktree remove: will run this binary"
		if f.printed() != want {
			t.Fatalf("stdout =\n%s\nwant\n%s", f.printed(), want)
		}
	})

	t.Run("--json reports the pin, the binary and the running build", func(t *testing.T) {
		f := newTeardownFixture(t)
		if err := f.pin(f.pinOptions()); err != nil {
			t.Fatal(err)
		}
		f.env.Executable = func() (string, error) { return f.stored(f.digest), nil }
		f.log.Lines, f.log.Printed = nil, nil
		if err := f.status(true, "0.0.0-dev"); err != nil {
			t.Fatal(err)
		}
		var got commands.TeardownStatusReport
		if err := json.Unmarshal([]byte(f.printed()), &got); err != nil {
			t.Fatalf("%v\n%s", err, f.printed())
		}
		want := commands.TeardownStatusReport{
			Pinned: true, OK: true, Release: "my-build", Source: "file:" + f.file, PinnedAt: "2026-10-14T12:00:00Z", Platform: "linux_amd64",
			Binary:  commands.TeardownBinaryReport{Path: f.stored(f.digest), SHA256: f.digest, State: "ok"},
			Running: commands.TeardownRunningReport{Version: "0.0.0-dev", IsPin: true},
		}
		if got != want {
			t.Fatalf("report = %+v\nwant     %+v", got, want)
		}
		if strings.Contains(f.printed(), `"signer"`) {
			t.Errorf("a file pin has no signer to report:\n%s", f.printed())
		}
		for _, key := range []string{`"pinned"`, `"ok"`, `"release"`, `"source"`, `"pinnedAt"`, `"platform"`, `"binary"`, `"path"`, `"sha256"`, `"state"`, `"running"`, `"version"`, `"isPin"`, `"newerThanPin"`} {
			if !strings.Contains(f.printed(), key) {
				t.Errorf("JSON has no %s", key)
			}
		}
	})

	t.Run("says the running build is not the pin when its bytes differ, or when it cannot be found", func(t *testing.T) {
		f := newTeardownFixture(t)
		if err := f.pin(f.pinOptions()); err != nil {
			t.Fatal(err)
		}
		for _, executable := range []func() (string, error){
			nil,
			func() (string, error) { return f.file + ".nope", nil },
			func() (string, error) { return "", errors.New("no") },
		} {
			f.env.Executable = executable
			f.log.Printed = nil
			if err := f.status(true, "0.0.0-dev"); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(f.printed(), `"isPin": false`) {
				t.Fatalf("stdout = %s", f.printed())
			}
		}
		other := filepath.Join(f.dir, "other")
		if err := os.WriteFile(other, []byte("different bytes"), 0o755); err != nil {
			t.Fatal(err)
		}
		f.env.Executable = func() (string, error) { return other, nil }
		f.log.Printed = nil
		if err := f.status(true, "0.0.0-dev"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(f.printed(), `"isPin": false`) {
			t.Fatalf("stdout = %s", f.printed())
		}
	})

	t.Run("reports a missing binary with the report, then refuses", func(t *testing.T) {
		f := newTeardownFixture(t)
		if err := f.pin(f.pinOptions()); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(f.stored(f.digest)); err != nil {
			t.Fatal(err)
		}
		f.log.Lines, f.log.Printed = nil, nil
		err := f.status(false, "0.0.0-dev")
		wantRefusal(t, err, "The pinned teardown binary "+f.stored(f.digest)+" is missing. Pin it again with "+f.pinAgain()+".")
		if want := "  binary  " + f.stored(f.digest) + " missing\n"; !strings.Contains(f.printed()+"\n", want) {
			t.Fatalf("stdout = %q, want a line %q", f.printed(), want)
		}
	})

	t.Run("reports a binary with another hash as a mismatch", func(t *testing.T) {
		f := newTeardownFixture(t)
		if err := f.pin(f.pinOptions()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.stored(f.digest), []byte("tampered"), 0o700); err != nil {
			t.Fatal(err)
		}
		found := binstore.Hash([]byte("tampered"))
		f.log.Lines, f.log.Printed = nil, nil
		err := f.status(false, "0.0.0-dev")
		wantRefusal(t, err, "The pinned teardown binary "+f.stored(f.digest)+" has sha256 "+found+", not the pinned "+f.digest+". Pin it again with "+f.pinAgain()+".")
		if want := "  binary  " + f.stored(f.digest) + " sha256 " + found + ", pinned " + f.digest; !strings.Contains(f.printed(), want) {
			t.Fatalf("stdout = %q, want a line %q", f.printed(), want)
		}
		f.log.Printed = nil
		_ = f.status(true, "0.0.0-dev")
		if !strings.Contains(f.printed(), `"state": "mismatch"`) || !strings.Contains(f.printed(), `"ok": false`) {
			t.Fatalf("stdout = %s", f.printed())
		}
	})

	t.Run("reports a stored path that is not a regular file as a mismatch", func(t *testing.T) {
		f := newTeardownFixture(t)
		if err := f.pin(f.pinOptions()); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(f.stored(f.digest)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(f.file, f.stored(f.digest)); err != nil {
			t.Fatal(err)
		}
		wantRefusal(t, f.status(false, "0.0.0-dev"), "The pinned teardown binary "+f.stored(f.digest)+" is not a regular file. Pin it again with "+f.pinAgain()+".")
	})

	t.Run("reports a platform the pin has no binary for", func(t *testing.T) {
		f := newTeardownFixture(t)
		if err := f.pin(f.pinOptions()); err != nil {
			t.Fatal(err)
		}
		f.env.Arch = "arm64"
		f.log.Lines, f.log.Printed = nil, nil
		wantRefusal(t, f.status(false, "0.0.0-dev"), "The teardown pin has no binary for linux_arm64. Pin one for this machine with "+pinCommand+".")
		if !strings.Contains(f.printed(), "  binary  none\n") && !strings.HasSuffix(strings.Split(f.printed(), "\n")[3], "none") {
			t.Fatalf("stdout = %q", f.printed())
		}
		f.log.Printed = nil
		_ = f.status(true, "0.0.0-dev")
		if strings.Contains(f.printed(), `"path"`) || strings.Contains(f.printed(), `"sha256"`) {
			t.Errorf("a platform with no binary has no path or digest:\n%s", f.printed())
		}
		for _, want := range []string{`"state": "none"`, `"ok": false`, `"platform": "linux_arm64"`} {
			if !strings.Contains(f.printed(), want) {
				t.Errorf("JSON has no %s:\n%s", want, f.printed())
			}
		}
	})

	t.Run("refuses a newer pin and a foreign one with nothing on stdout", func(t *testing.T) {
		for _, tc := range []struct {
			name, body string
			is         error
		}{
			{"newer", `{"version": 99}`, stateformat.ErrNewer},
			{"foreign", `{"version": 1}`, teardown.ErrForeign},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newTeardownFixture(t)
				if err := os.MkdirAll(f.control.TeardownDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.control.PinPath, []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
				for _, json := range []bool{false, true} {
					if err := f.status(json, "0.0.0-dev"); !errors.Is(err, tc.is) || clierr.Code(err) != 1 {
						t.Fatalf("json=%v: err = %v, want %v with exit 1", json, err, tc.is)
					}
					if f.printed() != "" {
						t.Fatalf("json=%v: stdout = %q", json, f.printed())
					}
				}
			})
		}
	})

	t.Run("hints at the pin command when the running build is newer than the pinned release", func(t *testing.T) {
		f := newTeardownFixture(t)
		f.releasePin(t, "1.2.3", "release build")
		if err := f.status(false, "1.3.0"); err != nil {
			t.Fatal(err)
		}
		want := "info: The running build (1.3.0) is newer than the pinned release (1.2.3). Pin the new build with `treerig teardown pin --release 1.3.0` to update it."
		if f.logged() != want {
			t.Fatalf("stderr = %q\nwant     %q", f.logged(), want)
		}
	})

	t.Run("gives no hint for an equal, older, development or unparsable running version", func(t *testing.T) {
		f := newTeardownFixture(t)
		f.releasePin(t, "1.2.3", "release build")
		for _, version := range []string{"1.2.3", "1.2.2", "1.2.3-rc.1", "0.0.0-dev", "0.0.0-dev+abc123def456", "garbage", ""} {
			f.log.Lines = nil
			if err := f.status(false, version); err != nil {
				t.Fatal(err)
			}
			if f.logged() != "" {
				t.Errorf("version %q: stderr = %q, want no hint", version, f.logged())
			}
		}
	})

	t.Run("--json carries the comparison instead of the hint", func(t *testing.T) {
		f := newTeardownFixture(t)
		f.releasePin(t, "1.2.3", "release build")
		if err := f.status(true, "1.3.0"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(f.printed(), `"signer": "https://github.com/sofired/treerig/.github/workflows/release.yml@refs/tags/v1.2.3"`) {
			t.Fatalf("a release pin reports its signer:\n%s", f.printed())
		}
		if !strings.Contains(f.printed(), `"newerThanPin": true`) {
			t.Fatalf("stdout = %s", f.printed())
		}
		if f.logged() != "" {
			t.Fatalf("stderr = %q, want no hint with --json", f.logged())
		}
	})

	t.Run("gives a file pin no hint, whatever its label says", func(t *testing.T) {
		f := newTeardownFixture(t)
		options := f.pinOptions()
		options.Release = "1.0.0"
		if err := f.pin(options); err != nil {
			t.Fatal(err)
		}
		f.log.Lines, f.log.Printed = nil, nil
		if err := f.status(false, "9.9.9"); err != nil {
			t.Fatal(err)
		}
		if f.logged() != "" {
			t.Fatalf("stderr = %q, want no hint", f.logged())
		}
		_ = f.status(true, "9.9.9")
		if !strings.Contains(f.printed(), `"newerThanPin": false`) {
			t.Fatalf("stdout = %s", f.printed())
		}
	})

	t.Run("hints even when the binary is missing", func(t *testing.T) {
		f := newTeardownFixture(t)
		f.releasePin(t, "1.2.3", "release build")
		if err := os.Remove(f.stored(binstore.Hash([]byte("release build")))); err != nil {
			t.Fatal(err)
		}
		_ = f.status(false, "2.0.0")
		if !strings.Contains(f.logged(), "The running build (2.0.0) is newer than the pinned release (1.2.3).") {
			t.Fatalf("stderr = %q", f.logged())
		}
	})
}
