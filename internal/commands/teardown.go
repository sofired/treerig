package commands

// `teardown pin` (--file, --release, --latest) and `teardown status` (#85,
// docs/design/teardown.md): the commands over the pin store in
// internal/teardown. Nothing here pins, updates or unpins implicitly, and
// nothing here runs the pinned binary: the removal gate (teardown_gate.go,
// #146) does that. `status` reports the pin's integrity on this machine and
// whether removal would run the stored binary. The download forms are in
// teardown_release.go.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sofired/treerig/internal/binstore"
	"github.com/sofired/treerig/internal/clierr"
	"github.com/sofired/treerig/internal/layout"
	"github.com/sofired/treerig/internal/releasefetch"
	"github.com/sofired/treerig/internal/semver"
	"github.com/sofired/treerig/internal/teardown"
	"github.com/sofired/treerig/internal/text"
)

// teardownPhrase is what `teardown pin` makes the user type.
const teardownPhrase = "pin teardown binary"

// maxTeardownBinary is the largest binary a pin accepts, 200 MiB
// (docs/design/teardown.md, "Sources"). A variable only so a test can lower it.
var maxTeardownBinary int64 = 200 << 20

// releaseLimits bounds what a release archive may expand to and the binary it
// holds (docs/design/teardown.md, "Sources"). A variable only so a test can
// lower it.
var releaseLimits = releasefetch.DefaultLimits

// noSignerText is how a file pin's signer reads: nothing was verified.
const noSignerText = "none (digest supplied by you)"

// TeardownPinOptions are `teardown pin`'s flags.
type TeardownPinOptions struct {
	// File, SHA256 and Release are --file, --sha256 and --release. With File,
	// they are the binary to pin, the digest the user verified it against,
	// and the label the pin carries; without it, Release is the version to
	// download and verify.
	File    string
	SHA256  string
	Release string
	// Latest is --latest: download the newest release that is not a
	// pre-release. It excludes Release and File.
	Latest bool
	// Yes skips the confirmation phrase.
	Yes bool
	// Now is the clock for pinned_at and the sweep; nil means time.Now.
	Now func() time.Time

	// Source, Policy and DownloadTimeout are for tests, and internal/cli must
	// never set them: the production values are the zero ones, and a place a
	// release is downloaded from, or a way of verifying it, that a caller could
	// choose would defeat the pin. Source says where releases are published (the treerig
	// repository on GitHub), Policy builds the cosign policy for a release
	// (keyless, with the release workflow's identity), and DownloadTimeout
	// bounds the fetches and the cosign runs together (10 minutes).
	Source          releasefetch.Source
	Policy          func(release string) releasefetch.Policy
	DownloadTimeout time.Duration
}

// TeardownStatusOptions are `teardown status`'s flags.
type TeardownStatusOptions struct {
	// JSON prints one JSON document on stdout instead of the report.
	JSON bool
	// Version is the running build's version (brand.Version), which the
	// freshness hint and --json's "running" object compare with the pin.
	Version string
}

// teardownControl resolves the control home through checkedHome, the one path
// every verb takes to it, and derives the teardown layout from it. userHome is
// for the adoption check files.SecureTeardownDirectory makes before a write.
func teardownControl(env *Env) (control layout.Control, userHome string, err error) {
	home, userHome, err := checkedHome(env.Getenv)
	if err != nil {
		return layout.Control{}, "", err
	}
	controlHome, err := layout.ControlHomeFrom(home, env.Getenv, "")
	if err != nil {
		return layout.Control{}, "", err
	}
	return layout.ControlLayout(controlHome), userHome, nil
}

// teardownPlatform is this process's platform key, refused when treerig builds
// no teardown binary for it (the pin has four keys and no others).
func teardownPlatform(env *Env, release string) (string, error) {
	key := teardown.PlatformKey(env.OS, env.Arch)
	if !slices.Contains(teardown.ReleasePlatforms, key) {
		return "", clierr.Newf("Refusing to pin %s: %s is not a platform treerig builds (linux and darwin, on amd64 and arm64).", release, text.Printable(key))
	}
	return key, nil
}

// pinSourceHelp is how a refusal names the ways to pin.
const pinSourceHelp = "pass --release X.Y.Z or --latest to download and verify a release, or --file with --sha256 and --release to pin a binary you have verified"

// checkPinFlags applies the flag rules of docs/design/teardown.md,
// "Installing or updating the pin": a download takes --release or --latest, and
// --file takes --sha256 and --release (the label). It returns whether this is a
// file pin.
func checkPinFlags(options TeardownPinOptions) (fromFile bool, err error) {
	if options.File != "" {
		if options.Latest {
			return true, clierr.New("--latest cannot be used with --file: a file pin is for a binary you verified yourself.")
		}
		var missing []string
		for _, flag := range []struct{ name, value string }{{"--sha256", options.SHA256}, {"--release", options.Release}} {
			if flag.value == "" {
				missing = append(missing, flag.name)
			}
		}
		switch len(missing) {
		case 0:
		case 1:
			return true, clierr.Newf("Missing required flag: %s.", missing[0])
		default:
			return true, clierr.Newf("Missing required flags: %s.", strings.Join(missing, ", "))
		}
		if !teardown.ValidSHA256(strings.ToLower(options.SHA256)) {
			return true, clierr.New("--sha256 must be 64 hexadecimal characters.")
		}
		if !teardown.ValidLabel(options.Release) {
			return true, clierr.New("--release must be 1 to 64 characters: letters, digits, '.', '_', '+' and '-', starting with a letter or a digit.")
		}
		return true, nil
	}
	switch {
	case options.SHA256 != "":
		return false, clierr.New("--sha256 can only be used with --file.")
	case options.Latest && options.Release != "":
		return false, clierr.New("--release and --latest cannot be used together.")
	case !options.Latest && options.Release == "":
		return false, clierr.Newf("Missing a source to pin: %s.", pinSourceHelp)
	case !options.Latest && !semver.IsRelease(options.Release):
		return false, clierr.New("--release must be a version such as 1.2.3 or 0.1.0-rc.1, without a leading v.")
	}
	return false, nil
}

// TeardownPin is `teardown pin`. With --file, it pins a local binary the user
// has verified by hand: the file is read once, those bytes are hashed and
// compared with --sha256, and the same bytes are installed. No signature is
// checked and no program is run; the plan says so. With --release or --latest
// it downloads a release and verifies it with cosign (teardown_release.go).
func TeardownPin(ctx context.Context, env *Env, options TeardownPinOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fromFile, err := checkPinFlags(options)
	if err != nil {
		return err
	}
	if !fromFile {
		return teardownPinRelease(ctx, env, options)
	}
	want := strings.ToLower(options.SHA256)
	release := options.Release
	platform, err := teardownPlatform(env, release)
	if err != nil {
		return err
	}
	path := options.File
	if !filepath.IsAbs(path) {
		path = filepath.Join(env.Cwd, path)
	}
	path = filepath.Clean(path)
	if text.HasControl(path) {
		return clierr.Newf("Refusing to pin %s: the path %s has control characters in it.", release, text.Printable(path))
	}

	binary, err := readPinFile(path)
	if err != nil {
		return err
	}
	if found := binstore.Hash(binary); found != want {
		return clierr.Newf("Refusing to pin %s: %s has sha256 %s, not the %s you supplied. Nothing was pinned. Check that you are pinning the file you verified and that --sha256 is its digest.", release, text.Printable(path), found, text.Printable(want))
	}
	// A digest that matches is the approval, but it must not make an empty
	// file or a binary for another platform a pin that `status` calls intact.
	if err := teardown.CheckExecutable(binary, platform); err != nil {
		return clierr.Newf("Refusing to pin %s: %s is not a %s executable (%s). Nothing was pinned. Pin the treerig binary built for this machine.", release, text.Printable(path), platform, err)
	}
	return installPin(env, options, binary, release, platform, func(now time.Time) teardown.Pin {
		return teardown.FilePin(path, release, platform, want, now)
	}, func(existing teardown.Pin, found bool, outcome teardown.Outcome) string {
		return pinPlan(existing, found, outcome, platform, want, [][2]string{{"signer", noSignerText + "; no signature was checked"}})
	})
}

// installPin is the end of every pin: read the existing pin (a newer or foreign
// one refuses here), show the plan and ask, read the pin again, and call
// teardown.Update and teardown.Save with the bytes already verified. build
// builds the pin to write for a given time; plan draws the confirmation screen.
func installPin(env *Env, options TeardownPinOptions, binary []byte, release, platform string, build func(time.Time) teardown.Pin, plan func(teardown.Pin, bool, teardown.Outcome) string) error {
	control, userHome, err := teardownControl(env)
	if err != nil {
		return err
	}
	existing, found, err := teardown.ReadPin(control)
	if err != nil {
		return err
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	// The plan does not show the time, so the pin it is drawn from carries none.
	outcome := teardown.Update(existing, found, build(time.Time{}))
	if err := confirm(env, plan(existing, found, outcome), teardownPhrase, options.Yes); err != nil {
		return err
	}

	// Read-modify-write with no lock: the plan above was drawn from a read
	// that is a prompt old, so the pin is read again immediately before the
	// update and the write, and the clock is read after the prompt too. A
	// writer that lands between this read and Save's rename still loses to
	// this one (the last writer wins), which docs/design/teardown.md accepts.
	existing, found, err = teardown.ReadPin(control)
	if err != nil {
		return err
	}
	written := now()
	outcome = teardown.Update(existing, found, build(written))
	saved, err := teardown.Save(control, userHome, binary, outcome.Pin, written)
	if err != nil {
		return clierr.Wrap(fmt.Sprintf("Could not pin the teardown binary in %s: %s.", text.Printable(control.TeardownDir), text.OneLine(err.Error(), 300)), err)
	}
	env.Logger.Info(fmt.Sprintf("Pinned the teardown binary: release %s (%s), stored at %s.", release, platform, text.Printable(saved.BinaryPath)))
	if saved.SweepErr != nil {
		env.Logger.Warn(fmt.Sprintf("Could not clean up teardown binaries no pin names: %s", text.OneLine(saved.SweepErr.Error(), 300)))
	}
	if controlHomeNoExec(env.NoExec, control) {
		env.Logger.Warn(noExecAdvice(env.CommandName, control))
	}
	return nil
}

// controlHomeNoExec reports whether the control home's filesystem is mounted
// noexec, which would make the removal gate's exec fail, asking noExec (nil
// asks the operating system). A check that cannot be made says no: it only
// decides whether to warn.
func controlHomeNoExec(noExec func(path string) (bool, error), control layout.Control) bool {
	if noExec == nil {
		noExec = teardown.MountNoExec
	}
	noexec, err := noExec(control.ControlHome)
	return err == nil && noexec
}

// noExecAdvice is the warning for a noexec control home, which `teardown pin`,
// `teardown status` and `doctor` print: removal will refuse until it is fixed.
func noExecAdvice(commandName string, control layout.Control) string {
	return fmt.Sprintf("The filesystem holding the control home (%s) is mounted noexec, so the pinned teardown binary cannot run and `%s worktree remove` will refuse. Remount it with exec, or move the control home with TREERIG_CONTROL_HOME.",
		text.Printable(control.ControlHome), commandName)
}

// storeProblems lists, without running anything, what would make the removal
// gate refuse a pin whose binary is intact: a control home inside a git work
// tree, a pin store that is not private to the user, a filesystem mounted
// noexec. binary is the opened stored binary, or nil; boundary is
// teardown.HomeFromUserDatabase's. The sentences are the gate's own, so a warning reads as
// the refusal it predicts.
func storeProblems(commandName, version string, noExec func(string) (bool, error), control layout.Control, boundary teardown.HomeBoundary, binary *teardown.Binary) []string {
	var problems []string
	if problem, _ := locationProblem(control, boundary); problem != "" {
		problems = append(problems, problem)
	}
	if binary != nil {
		if problem, _ := ownershipProblem(commandName, version, control, binary); problem != "" {
			problems = append(problems, problem)
		}
	}
	if controlHomeNoExec(noExec, control) {
		problems = append(problems, noExecAdvice(commandName, control))
	}
	return problems
}

// readPinFile reads the file to pin once, in memory. It refuses anything that
// is not a regular file before opening it (a FIFO would hang the open), and
// anything over maxTeardownBinary.
func readPinFile(path string) ([]byte, error) {
	// O_NONBLOCK keeps a FIFO from hanging the open; the descriptor is then
	// the only thing checked, so nothing can be swapped in after the check.
	// Symlinks are followed: the path is the user's, and an installed treerig is
	// often one (a Homebrew bin/treerig, say).
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, clierr.Newf("Could not read %s: %s.", text.Printable(path), reasonWithoutPath(err))
	}
	defer func() { _ = file.Close() }()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, clierr.Newf("Could not read %s: it is not a regular file.", text.Printable(path))
	}
	data, err := io.ReadAll(io.LimitReader(file, maxTeardownBinary+1))
	if err != nil {
		return nil, clierr.Newf("Could not read %s: %s.", text.Printable(path), reasonWithoutPath(err))
	}
	if int64(len(data)) > maxTeardownBinary {
		return nil, clierr.Newf("Refusing to pin %s: it is larger than %s.", text.Printable(path), sizeText(maxTeardownBinary))
	}
	return data, nil
}

// sizeText is n in MiB when it is a whole number of them, else in bytes.
func sizeText(n int64) string {
	if n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}

// reasonWithoutPath is err's system error without the path an *fs.PathError
// names, for a message that names the path once.
func reasonWithoutPath(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return err.Error()
}

// pinPlan is the confirmation screen of a pin: the plan blocks of
// docs/design/teardown.md. extra rows (signer, and for a release pin cosign)
// are name, value pairs between "source" and "platform". Every value is
// borrowed from the network, the file system or a pin on disk, so each goes
// through text.Printable.
func pinPlan(existing teardown.Pin, found bool, outcome teardown.Outcome, platform, digest string, extra [][2]string) string {
	current := "none"
	if found {
		current = text.Printable(existing.Release)
		if outcome.Older {
			current += "; this is older"
		}
	}
	incoming := outcome.Pin
	rows := [][2]string{
		{"release", fmt.Sprintf("%s  (current pin: %s)", text.Printable(incoming.Release), current)},
		{"source", text.Printable(incoming.Source)},
	}
	rows = append(rows, extra...)
	rows = append(rows, [2]string{"platform", fmt.Sprintf("%s  sha256 %s", platform, digest)})
	lines := []string{"Pin the teardown binary"}
	for _, row := range rows {
		lines = append(lines, "  "+text.PadEnd(row[0], 8)+"  "+row[1])
	}
	lines = append(lines, "  `worktree remove` will run this binary instead of the build you invoke.")
	return strings.Join(lines, "\n")
}

// TeardownBinaryReport is --json's "binary" object. SHA256 is the digest the
// pin records for this platform; State says whether the file at Path has it. A
// platform with no binary (state none) has neither.
type TeardownBinaryReport struct {
	Path   string               `json:"path,omitempty"`
	SHA256 string               `json:"sha256,omitempty"`
	State  teardown.BinaryState `json:"state"`
}

// TeardownRunningReport is --json's "running" object: the build that ran
// `status`, whether it is the pinned binary (its bytes hash to the pin's digest
// for this platform), and whether its version ranks above the pin's release.
type TeardownRunningReport struct {
	Version      string `json:"version"`
	IsPin        bool   `json:"isPin"`
	NewerThanPin bool   `json:"newerThanPin"`
}

// What `worktree remove` would do, as `teardown status --json` reports it in
// "removal": run the pinned binary, remove with this release build (no pin,
// and none needed), or refuse.
const (
	removalPin       = "pin"
	removalThisBuild = "this-build"
	removalRefuse    = "refuse"
)

// TeardownStatusReport is `teardown status --json` for a pin that exists. OK
// is true only when the pinned binary for this platform is installed and has
// the recorded digest. Removal says what `worktree remove` would do, which
// also needs the pin store to be somewhere and owned as the gate requires.
type TeardownStatusReport struct {
	Pinned   bool                  `json:"pinned"`
	OK       bool                  `json:"ok"`
	Removal  string                `json:"removal"`
	Release  string                `json:"release"`
	Source   string                `json:"source"`
	Signer   string                `json:"signer,omitempty"`
	PinnedAt string                `json:"pinnedAt"`
	Platform string                `json:"platform"`
	Binary   TeardownBinaryReport  `json:"binary"`
	Running  TeardownRunningReport `json:"running"`
}

// TeardownStatus is `teardown status`: it reads the pin and checks that the
// stored binary for this platform is there and has the digest the pin
// records, and says what `worktree remove` would do. Exit 0 means removal would
// run: the pin is intact and its store acceptable to the gate (outside every
// checkout, private to you, on a filesystem that can run programs), or there
// is no pin and this is a release build, which needs none. Anything else is
// reported on stderr, with exit 1: a development build with no pin, a pin that
// cannot be used, a store the gate would refuse (each of those as a warning
// first). Stdout carries the report, or with --json one document that always
// has "removal".
func TeardownStatus(ctx context.Context, env *Env, options TeardownStatusOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	control, userHome, err := teardownControl(env)
	if err != nil {
		return err
	}
	pin, found, err := teardown.ReadPin(control)
	if err != nil {
		return err
	}
	if !found {
		// No pin is fine for a release build, which removes with itself; for a
		// development build it is the problem it was.
		release := semver.IsPublishedRelease(options.Version)
		removal, closing := removalRefuse, "worktree remove: would refuse"
		if release {
			removal, closing = removalThisBuild, "worktree remove: will run this build (a release; no pin needed)"
		}
		if options.JSON {
			if err := outputJSON(env, struct {
				Pinned  bool   `json:"pinned"`
				Removal string `json:"removal"`
			}{Removal: removal}); err != nil {
				return err
			}
		} else {
			env.Logger.Output(closing)
		}
		if release {
			env.Logger.Info(fmt.Sprintf("No teardown binary is pinned, and a release build needs none. Pin one with %s to make removal keep running that verified build.",
				pinCommandFor(env.CommandName, options.Version)))
			return nil
		}
		return clierr.Newf("No teardown binary is pinned. Pin one with %s.", pinCommandFor(env.CommandName, options.Version))
	}

	platform := teardown.PlatformKey(env.OS, env.Arch)
	check, binary := teardown.OpenBinary(control, pin, platform)
	defer binary.Close()
	running := TeardownRunningReport{
		Version: options.Version,
		IsPin:   check.Pinned != "" && runningIs(env, check.Pinned),
		// A file pin's release is a free label, not a version.
		NewerThanPin: !pin.IsFile() && semver.Newer(options.Version, pin.Release),
	}

	problems := storeProblems(env.CommandName, options.Version, env.NoExec, control, teardown.HomeFromUserDatabase(env.PasswdHome, userHome), binary)
	removal, closing := removalPin, "worktree remove: will run this binary"
	switch {
	case check.State != teardown.BinaryOK:
		removal, closing = removalRefuse, "worktree remove: would refuse"
	case len(problems) > 0:
		removal, closing = removalRefuse, "worktree remove: would refuse (see the warning below)"
	}
	if options.JSON {
		report := TeardownStatusReport{
			Pinned: true, OK: check.State == teardown.BinaryOK, Removal: removal, Release: pin.Release, Source: pin.Source, Signer: pin.Signer,
			PinnedAt: pin.PinnedAt, Platform: platform,
			Binary:  TeardownBinaryReport{Path: check.Path, SHA256: check.Pinned, State: check.State},
			Running: running,
		}
		if err := outputJSON(env, report); err != nil {
			return err
		}
	} else {
		printTeardownReport(env, pin, platform, check, closing)
		if hint := newerHint(env.CommandName, pin, options.Version); hint != "" {
			env.Logger.Info(hint)
		}
	}
	// Stdout stays the report or the document; what would make removal refuse
	// besides the pin's own integrity is on stderr in both forms.
	for _, problem := range problems {
		env.Logger.Warn(problem)
	}
	again := repinCommand(env.CommandName, options.Version, pin, platform)
	switch check.State {
	case teardown.BinaryOK:
		if len(problems) > 0 {
			return clierr.Newf("The pin is intact, but `%s worktree remove` would refuse; see the warning above.", env.CommandName)
		}
		return nil
	case teardown.BinaryNone:
		return clierr.Newf("The teardown pin has no binary for %s. Pin one for this machine with %s.", platform, again)
	}
	return clierr.Newf("The pinned teardown binary %s %s. Pin it again with %s.", text.Printable(check.Path), check.Problem, again)
}

// printTeardownReport prints the human report on stdout, ending with closing,
// the line that says what removal would do.
func printTeardownReport(env *Env, pin teardown.Pin, platform string, check teardown.BinaryCheck, closing string) {
	signer := noSignerText
	if !pin.IsFile() {
		signer = text.Printable(pin.Signer)
	}
	binary := "none"
	switch check.State {
	case teardown.BinaryOK:
		binary = fmt.Sprintf("%s sha256 %s ok", text.Printable(check.Path), check.Found)
	case teardown.BinaryMissing:
		binary = text.Printable(check.Path) + " missing"
	case teardown.BinaryMismatch:
		if check.Found != "" {
			binary = fmt.Sprintf("%s sha256 %s, pinned %s", text.Printable(check.Path), check.Found, check.Pinned)
		} else {
			binary = text.Printable(check.Path) + " " + check.Problem
		}
	}
	rows := [][2]string{
		{"source", text.Printable(pin.Source)},
		{"signer", signer},
		{"binary", binary},
		{"pinned", text.Printable(pin.PinnedAt)},
	}
	if !pin.IsFile() {
		rows = append(rows, [2]string{"attestation", "not checked"})
	}
	width := 0
	for _, row := range rows {
		width = max(width, len(row[0]))
	}
	env.Logger.Output(fmt.Sprintf("Teardown pin: release %s (%s)", text.Printable(pin.Release), platform))
	for _, row := range rows {
		env.Logger.Output("  " + text.PadEnd(row[0], width) + "  " + row[1])
	}
	// Not a row: the label would widen the column of every other one.
	env.Logger.Output(closing)
}

// runningIs reports whether the running build's bytes hash to digest. It is
// false when the entry point gave no way to find the running binary
// (Env.Executable is nil in tests and in cli.Main) and when it cannot be read.
func runningIs(env *Env, digest string) bool {
	if env.Executable == nil {
		return false
	}
	path, err := env.Executable()
	if err != nil {
		return false
	}
	found, err := binstore.HashFile(path)
	return err == nil && found == digest
}

// outputJSON prints value as the command's one JSON document on stdout.
func outputJSON(env *Env, value any) error {
	out, err := text.SafeJSON(value)
	if err != nil {
		return err
	}
	env.Logger.Output(out)
	return nil
}
