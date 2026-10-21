// Package semver is a small, strict comparer for Semantic Versioning 2.0.0
// versions (https://semver.org/spec/v2.0.0.html), written by hand rather than
// taken from golang.org/x/mod/semver, which would be a new dependency in
// go.mod (docs/design/teardown.md, "Versions").
//
// Parsing is strict: major.minor.patch with no leading zeros and no "v", an
// optional "-" pre-release and an optional "+" build, each a dot-separated list
// of [0-9A-Za-z-] identifiers that are never empty, and a numeric pre-release
// identifier with no leading zero. Precedence is section 11 of the
// specification: build metadata is ignored, a pre-release ranks below its
// release, and numbers are compared as arbitrary-size integers, so no integer
// width overflows.
//
// Failure is conservative. A version that does not parse, and a development
// version, are never newer than anything, and never meet a minimum.
package semver

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// releaseForm is release.yml's version pattern: what a release tag may carry
// after its "v" (docs/design/teardown.md, "Sources").
var releaseForm = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$`)

// IsRelease reports whether s is a version a release may carry: release.yml's
// pattern, and strict SemVer. `0.1.0-rc.1` and a snapshot's `0.0.0-dev+abc123`
// qualify; `v1.2.3` and `1.2` do not.
func IsRelease(s string) bool {
	if !releaseForm.MatchString(s) {
		return false
	}
	_, err := Parse(s)
	return err == nil
}

// gitDescribeTail is what `git describe --tags` appends to a tag for a commit
// past it: "-<commits>-g<abbreviated hash>". The "-dirty" or "-broken" that
// --dirty and --broken add after it is caught by gitDescribeMark.
var gitDescribeTail = regexp.MustCompile(`-[0-9]+-g[0-9a-f]{7,}$`)

// gitDescribeMark is a "-dirty" or "-broken" suffix, bare (`0.1.0-dirty`, a tag
// checked out with local changes) or after the tail above.
var gitDescribeMark = regexp.MustCompile(`-(dirty|broken)$`)

// IsPublishedRelease reports whether s is the version of a build that was cut
// from a release tag, as opposed to a development, snapshot or source build. It
// is the one place that decides, and it is strict, because the removal gate
// trusts it to tell a build users get from one a branch can produce
// (docs/design/teardown.md, decision 10):
//
//   - s is a release version (IsRelease), with a pre-release suffix such as
//     `-rc.2` allowed;
//   - it has no `+build` metadata, so a snapshot's `0.0.0-dev+<sha>` is out;
//   - it is not 0.0.0 in any form, so an unstamped build's `0.0.0-dev` is out;
//   - it does not end the way `git describe` ends a version for a commit past a
//     tag (`0.1.0-rc.2-5-gabc1234`, with `-dirty` or `-broken` after it, or a bare
//     `-dirty`/`-broken`), which would otherwise read as a pre-release.
//
// Nothing in this repository stamps a version that is not a tag's: the release
// takes it from the tag (GoReleaser, and Homebrew's formula), a snapshot stamps
// 0.0.0-dev+<sha>, and a plain build keeps 0.0.0-dev.
//
// What it cannot do is tell a version stamped by any other means from a tag's:
// a build whose version is set to something release-looking by hand, by
// -ldflags or GOFLAGS from an environment a branch controls (direnv, mise, a
// devcontainer), or by a build wrapper, is treated as a release, and with no pin
// it removes with itself. That is accepted: it is not a defence against a
// hostile process of the same user. The mitigation is a pin, which overrides it
// for every build.
func IsPublishedRelease(s string) bool {
	if !IsRelease(s) || strings.Contains(s, "+") || gitDescribeTail.MatchString(s) || gitDescribeMark.MatchString(s) {
		return false
	}
	return s != "0.0.0" && !strings.HasPrefix(s, "0.0.0-")
}

// Version is a parsed version. Numeric parts are kept as their decimal digits,
// which Compare orders by digit count and then lexically, so no width limits
// a version.
type Version struct {
	major, minor, patch string
	pre                 []string
}

// Parse reads s as a strict SemVer 2.0.0 version.
func Parse(s string) (Version, error) {
	rest, build, hasBuild := strings.Cut(s, "+")
	if hasBuild {
		if err := checkIdentifiers(build, false); err != nil {
			return Version{}, fmt.Errorf("invalid version %q: build metadata: %w", s, err)
		}
	}
	core, pre, hasPre := strings.Cut(rest, "-")
	numbers := strings.Split(core, ".")
	if len(numbers) != 3 {
		return Version{}, fmt.Errorf("invalid version %q: want major.minor.patch", s)
	}
	for _, n := range numbers {
		if err := checkNumber(n); err != nil {
			return Version{}, fmt.Errorf("invalid version %q: %w", s, err)
		}
	}
	version := Version{major: numbers[0], minor: numbers[1], patch: numbers[2]}
	if hasPre {
		if err := checkIdentifiers(pre, true); err != nil {
			return Version{}, fmt.Errorf("invalid version %q: pre-release: %w", s, err)
		}
		version.pre = strings.Split(pre, ".")
	}
	return version, nil
}

// checkNumber accepts digits with no leading zero (a lone "0" is fine).
func checkNumber(n string) error {
	if n == "" || !allDigits(n) {
		return errors.New("a number part is empty or not digits")
	}
	if len(n) > 1 && n[0] == '0' {
		return fmt.Errorf("number %q has a leading zero", n)
	}
	return nil
}

// checkIdentifiers accepts a dot-separated list of non-empty [0-9A-Za-z-]
// identifiers. For a pre-release (noLeadingZero) a numeric identifier has no
// leading zero; build metadata allows one.
func checkIdentifiers(list string, noLeadingZero bool) error {
	for _, id := range strings.Split(list, ".") {
		if id == "" {
			return errors.New("an identifier is empty")
		}
		for i := 0; i < len(id); i++ {
			c := id[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
				return fmt.Errorf("identifier %q has a character outside [0-9A-Za-z-]", id)
			}
		}
		if noLeadingZero && allDigits(id) && len(id) > 1 && id[0] == '0' {
			return fmt.Errorf("numeric identifier %q has a leading zero", id)
		}
	}
	return nil
}

// allDigits reports whether s is non-empty and made only of ASCII digits.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// IsDev reports whether v is a development version: 0.0.0 with a pre-release
// starting with "dev", what an unstamped build reports (brand.Version).
func (v Version) IsDev() bool {
	return v.major == "0" && v.minor == "0" && v.patch == "0" &&
		len(v.pre) > 0 && strings.HasPrefix(v.pre[0], "dev")
}

// Compare orders a and b by SemVer section 11 precedence: -1 when a ranks
// below b, 1 when above, 0 when equal. Build metadata plays no part.
func Compare(a, b Version) int {
	for _, pair := range [][2]string{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if c := compareNumbers(pair[0], pair[1]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		if c := compareIdentifiers(a.pre[i], b.pre[i]); c != 0 {
			return c
		}
	}
	return compareInts(len(a.pre), len(b.pre))
}

// compareNumbers compares two decimal strings without leading zeros as
// integers of any size: more digits is larger, equal lengths compare lexically.
func compareNumbers(a, b string) int {
	if c := compareInts(len(a), len(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

// compareIdentifiers compares two pre-release identifiers: numeric ones by
// value and below alphanumeric ones, alphanumeric ones as ASCII strings.
func compareIdentifiers(a, b string) int {
	aNumeric, bNumeric := allDigits(a), allDigits(b)
	switch {
	case aNumeric && bNumeric:
		return compareNumbers(a, b)
	case aNumeric:
		return -1
	case bNumeric:
		return 1
	}
	return strings.Compare(a, b)
}

// compareInts is -1, 0 or 1 as a is below, equal to or above b.
func compareInts(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Newer reports whether candidate is a higher version than than. It is false
// when either does not parse, and when candidate is a development version: a
// build that cannot say what it is never claims to be newer.
func Newer(candidate, than string) bool {
	c, err := Parse(candidate)
	if err != nil || c.IsDev() {
		return false
	}
	t, err := Parse(than)
	if err != nil {
		return false
	}
	return Compare(c, t) > 0
}

// AtLeast reports whether found is a valid, non-development version that
// ranks at or above minimum. An unparsable found, or minimum, fails; so does a
// pre-release of minimum (3.0.0-rc.1 is below 3.0.0).
func AtLeast(found, minimum string) bool {
	f, err := Parse(found)
	if err != nil || f.IsDev() {
		return false
	}
	m, err := Parse(minimum)
	if err != nil {
		return false
	}
	return Compare(f, m) >= 0
}
