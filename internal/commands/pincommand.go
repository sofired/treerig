package commands

import (
	"fmt"
	"strings"

	"github.com/sofired/treerig/internal/semver"
	"github.com/sofired/treerig/internal/teardown"
	"github.com/sofired/treerig/internal/text"
)

// This file is the one place that names the pin command in the messages
// (docs/design/teardown.md, "The pin command"). `--latest` never selects a
// pre-release, and the repository has only pre-releases so far, so every
// command names a release. When the first release that is not a pre-release is
// published, what changes is exactly this: publishedRelease below (the route
// pinCommandFor and noPinRefusal share, and the example version in it), and the
// two development-build branches that spell it out. Nothing else in the commands
// depends on how a release is named, except the `teardown pin --latest` refusal
// in teardown_release.go, which uses the same constant.

// publishedRelease is the release route of the pin command for a build that has
// no release version of its own to put there: the flag and a placeholder with an
// example.
const publishedRelease = "--release <a published version, such as 0.1.0-rc.2>"

// shellWord is s as one word a POSIX shell reads back unchanged: as is when it
// uses only characters that need no quoting, and otherwise in single quotes with
// each embedded single quote written '\”. s goes through text.Printable first,
// so what is quoted is what would be printed.
func shellWord(s string) string {
	s = text.Printable(s)
	safe := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_./:@%+=,-", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// pinCommandFor is the command the messages tell the user to run to pin a
// treerig build, for the running build's version. A release build names its
// own version, which is a published release, so the command runs as written. A
// development or source build has no release to name: it says what to put there
// and where the other route is.
func pinCommandFor(commandName, version string) string {
	if semver.IsPublishedRelease(version) {
		return fmt.Sprintf("`%s teardown pin --release %s`", commandName, shellWord(version))
	}
	return fmt.Sprintf("`%s teardown pin %s` (or `--file`; see `%s teardown pin --help`)", commandName, publishedRelease, commandName)
}

// repinCommand is the command that makes pin right again on this machine. A
// release pin is pinned again by its own release, which is a published version.
// A file pin is pinned again from the same file with the digest it recorded
// and the label it carries, so it runs as written and refuses if the file is no
// longer the one that was approved. When the pin has no binary for platform
// there is nothing to repeat, and the generic command is given.
func repinCommand(commandName, version string, pin teardown.Pin, platform string) string {
	if pin.IsFile() {
		digest := pin.Platforms[platform].BinarySHA256
		if digest == "" {
			return pinCommandFor(commandName, version)
		}
		return fmt.Sprintf("`%s teardown pin --file %s --sha256 %s --release %s`", commandName,
			shellWord(strings.TrimPrefix(pin.Source, "file:")), digest, shellWord(pin.Release))
	}
	return fmt.Sprintf("`%s teardown pin --release %s`", commandName, shellWord(pin.Release))
}

// noPinRefusal is the refusal for a development build with no pin: development
// builds remove only with a pinned teardown binary (a release build needs none).
// It says so, and gives both routes that work for a build with no release of its
// own to name, on lines of their own.
func noPinRefusal(commandName, version string) string {
	return fmt.Sprintf("this is a development build (%s), and a development build removes only with a pinned teardown binary, which none is yet (a release build needs no pin).\n"+
		"Pin a published release: `%s teardown pin %s`\n"+
		"or a treerig you trust: `%s teardown pin --file <a treerig you trust> --sha256 <its sha256> --release <a label>`\n"+
		"(`%s teardown pin --help` explains both). To remove with this build's own code while you work on removal itself, add `--dev-teardown`.",
		text.Printable(version), commandName, publishedRelease, commandName, commandName)
}
