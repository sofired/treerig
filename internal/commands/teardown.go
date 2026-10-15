package commands

// `teardown pin --file` and `teardown status` (#85, docs/design/teardown.md):
// the commands over the pin store in internal/teardown. Nothing here pins,
// updates or unpins implicitly, and nothing runs the pinned binary: the
// removal gate (#146) does not exist yet, so `status` reports the pin's
// integrity on this machine and says nothing about removal.

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

	"github.com/sofired/berth/internal/binstore"
	"github.com/sofired/berth/internal/clierr"
	"github.com/sofired/berth/internal/layout"
	"github.com/sofired/berth/internal/semver"
	"github.com/sofired/berth/internal/teardown"
	"github.com/sofired/berth/internal/text"
)

// teardownPhrase is what `teardown pin` makes the user type.
const teardownPhrase = "pin teardown binary"

// maxTeardownBinary is the largest binary a pin accepts, 200 MiB
// (docs/design/teardown.md, "Sources"). A variable only so a test can lower it.
var maxTeardownBinary int64 = 200 << 20

// noSignerText is how a file pin's signer reads: nothing was verified.
const noSignerText = "none (digest supplied by you)"

// TeardownPinOptions are `teardown pin`'s flags.
type TeardownPinOptions struct {
	// File, SHA256 and Release are --file, --sha256 and --release: the
	// binary to pin, the digest the user verified it against, and the label
	// the pin carries.
	File    string
	SHA256  string
	Release string
	// Yes skips the confirmation phrase.
	Yes bool
	// Now is the clock for pinned_at and the sweep; nil means time.Now.
	Now func() time.Time
}

// TeardownStatusOptions are `teardown status`'s flags.
type TeardownStatusOptions struct {
	// JSON prints one JSON document on stdout instead of the report.
	JSON bool
	// Version is the running build's version (brand.Version), which the
	// freshness hint and --json's "running" object compare with the pin.
	Version string
}

// teardownPinCommand is the command the messages tell the user to run. Until
// the `--release X.Y.Z` and `--latest` download forms exist (step 4 of
// docs/design/teardown.md) that is the `--file` form; step 4 changes this one
// function. Nothing else in the
// commands depends on how a release is named.
func teardownPinCommand(env *Env) string {
	return fmt.Sprintf("`%s teardown pin --file PATH --sha256 HEX --release LABEL`", env.CommandName)
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

// teardownPlatform is this process's platform key, refused when berth builds
// no teardown binary for it (the pin has four keys and no others).
func teardownPlatform(env *Env, release string) (string, error) {
	key := teardown.PlatformKey(env.OS, env.Arch)
	if !slices.Contains(teardown.ReleasePlatforms, key) {
		return "", clierr.Newf("Refusing to pin %s: %s is not a platform berth builds (linux and darwin, on amd64 and arm64).", release, text.Printable(key))
	}
	return key, nil
}

// TeardownPin is `teardown pin --file PATH --sha256 HEX --release LABEL`: pin a
// local binary the user has verified by hand. The file is read once, those
// bytes are hashed and compared with --sha256, and the same bytes are
// installed. No signature is checked and no program is run; the plan says so.
func TeardownPin(ctx context.Context, env *Env, options TeardownPinOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var missing []string
	for _, flag := range []struct{ name, value string }{{"--file", options.File}, {"--sha256", options.SHA256}, {"--release", options.Release}} {
		if flag.value == "" {
			missing = append(missing, flag.name)
		}
	}
	switch len(missing) {
	case 0:
	case 1:
		return clierr.Newf("Missing required flag: %s.", missing[0])
	default:
		return clierr.Newf("Missing required flags: %s.", strings.Join(missing, ", "))
	}
	want := strings.ToLower(options.SHA256)
	if !teardown.ValidSHA256(want) {
		return clierr.New("--sha256 must be 64 hexadecimal characters.")
	}
	if !teardown.ValidLabel(options.Release) {
		return clierr.New("--release must be 1 to 64 characters: letters, digits, '.', '_', '+' and '-', starting with a letter or a digit.")
	}
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
		return clierr.Newf("Refusing to pin %s: %s is not a %s executable (%s). Nothing was pinned. Pin the berth binary built for this machine.", release, text.Printable(path), platform, err)
	}

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
	outcome := teardown.Update(existing, found, teardown.FilePin(path, release, platform, want, time.Time{}))
	plan := teardownPlan(existing, found, outcome, platform, want)
	if err := confirm(env, plan, teardownPhrase, options.Yes); err != nil {
		return err
	}

	// Read-modify-write with no lock: the plan above was drawn from a read
	// that is a prompt old, so the pin is read again immediately before the
	// update and the write, and the clock is read after the prompt too. A
	// writer that lands between this read and Save's rename still loses to
	// this one (the last writer wins), which docs/design/teardown.md accepts
	// for a file pin: it has no checksums to merge, so replacing is all
	// Update does with one.
	existing, found, err = teardown.ReadPin(control)
	if err != nil {
		return err
	}
	written := now()
	outcome = teardown.Update(existing, found, teardown.FilePin(path, release, platform, want, written))
	saved, err := teardown.Save(control, userHome, binary, outcome.Pin, written)
	if err != nil {
		return clierr.Wrap(fmt.Sprintf("Could not pin the teardown binary in %s: %s.", text.Printable(control.TeardownDir), text.OneLine(err.Error(), 300)), err)
	}
	env.Logger.Info(fmt.Sprintf("Pinned the teardown binary: release %s (%s), stored at %s.", release, platform, text.Printable(saved.BinaryPath)))
	if saved.SweepErr != nil {
		env.Logger.Warn(fmt.Sprintf("Could not clean up teardown binaries no pin names: %s", text.OneLine(saved.SweepErr.Error(), 300)))
	}
	return nil
}

// readPinFile reads the file to pin once, in memory. It refuses anything that
// is not a regular file before opening it (a FIFO would hang the open), and
// anything over maxTeardownBinary.
func readPinFile(path string) ([]byte, error) {
	// O_NONBLOCK keeps a FIFO from hanging the open; the descriptor is then
	// the only thing checked, so nothing can be swapped in after the check.
	// Symlinks are followed: the path is the user's, and an installed berth is
	// often one (a Homebrew bin/berth, say).
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

// teardownPlan is the confirmation screen for a file pin: the plan block of
// docs/design/teardown.md. Every value is borrowed from the file system or
// from a pin on disk, so each goes through text.Printable. The last line is
// the pre-gate one; #146 changes it.
func teardownPlan(existing teardown.Pin, found bool, outcome teardown.Outcome, platform, digest string) string {
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
		{"signer", noSignerText + "; no signature was checked"},
		{"platform", fmt.Sprintf("%s  sha256 %s", platform, digest)},
	}
	lines := []string{"Pin the teardown binary"}
	for _, row := range rows {
		lines = append(lines, "  "+text.PadEnd(row[0], 8)+"  "+row[1])
	}
	lines = append(lines, "  `worktree remove` does not run this binary yet.")
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

// TeardownStatusReport is `teardown status --json` for a pin that exists. OK
// is true only when the pinned binary for this platform is installed and has
// the recorded digest.
type TeardownStatusReport struct {
	Pinned   bool                  `json:"pinned"`
	OK       bool                  `json:"ok"`
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
// records. It reports pin integrity only, and says nothing about removal until
// the gate exists. Stdout carries the report; a problem is an error, printed
// on stderr, and exit 1. No pin is an error too (with --json, after
// {"pinned": false}).
func TeardownStatus(ctx context.Context, env *Env, options TeardownStatusOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	control, _, err := teardownControl(env)
	if err != nil {
		return err
	}
	command := teardownPinCommand(env)
	pin, found, err := teardown.ReadPin(control)
	if err != nil {
		return err
	}
	if !found {
		if options.JSON {
			if err := outputJSON(env, struct {
				Pinned bool `json:"pinned"`
			}{}); err != nil {
				return err
			}
		}
		return clierr.Newf("No teardown binary is pinned. Pin one with %s.", command)
	}

	platform := teardown.PlatformKey(env.OS, env.Arch)
	check := teardown.InspectBinary(control, pin, platform)
	running := TeardownRunningReport{
		Version: options.Version,
		IsPin:   check.Pinned != "" && runningIs(env, check.Pinned),
		// A file pin's release is a free label, not a version.
		NewerThanPin: !pin.IsFile() && semver.Newer(options.Version, pin.Release),
	}

	if options.JSON {
		report := TeardownStatusReport{
			Pinned: true, OK: check.State == teardown.BinaryOK, Release: pin.Release, Source: pin.Source, Signer: pin.Signer,
			PinnedAt: pin.PinnedAt, Platform: platform,
			Binary:  TeardownBinaryReport{Path: check.Path, SHA256: check.Pinned, State: check.State},
			Running: running,
		}
		if err := outputJSON(env, report); err != nil {
			return err
		}
	} else {
		printTeardownReport(env, pin, platform, check)
		if running.NewerThanPin {
			env.Logger.Info(fmt.Sprintf("The running build (%s) is newer than the pinned release (%s). Pin the new build with %s to update it.",
				text.Printable(options.Version), text.Printable(pin.Release), command))
		}
	}
	switch check.State {
	case teardown.BinaryOK:
		return nil
	case teardown.BinaryNone:
		return clierr.Newf("The teardown pin has no binary for %s. Pin one on this machine with %s.", platform, command)
	}
	return clierr.Newf("The pinned teardown binary %s %s. Pin it again with %s.", text.Printable(check.Path), check.Problem, command)
}

// printTeardownReport prints the human report on stdout.
func printTeardownReport(env *Env, pin teardown.Pin, platform string, check teardown.BinaryCheck) {
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
