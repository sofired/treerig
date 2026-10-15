// Package teardown is the store behind the pinned teardown binary (#85,
// docs/design/teardown.md): the pin file, its strict reader, the rules for
// updating an existing pin, the installation of the pinned binaries, and the
// sweep of the ones no pin names any more.
//
// The pin is stored state, so it follows docs/design/state.md. Its version is
// judged first and on its own: a newer pin is refused as newer
// (stateformat.Newer), and a pin with no version, an unknown version, an unknown
// key or a bad field is foreign (ErrForeign) and is never rewritten. This
// package never runs a pinned binary and fetches nothing; the commands and the
// removal gate are built on it.
package teardown

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sofired/berth/internal/semver"
	"github.com/sofired/berth/internal/stateformat"
	"github.com/sofired/berth/internal/text"
)

// Pin is the contents of teardown/pin.json. The field order is the order on
// disk (files.AtomicJSON follows it).
type Pin struct {
	// Version is stateformat.TeardownPin. WritePin sets it.
	Version int `json:"version"`
	// Release is the pinned version for a release pin, or the --release label
	// for a file pin.
	Release string `json:"release"`
	// Source is the https release base URL, or "file:" and the absolute,
	// cleaned path of a file pin.
	Source string `json:"source"`
	// Signer is the cosign identity that was verified; empty for a file pin.
	Signer string `json:"signer"`
	// ChecksumsSHA256 is the hash of the verified checksums.txt; empty for a
	// file pin.
	ChecksumsSHA256 string `json:"checksums_sha256"`
	// PinnedAt is RFC 3339, UTC, with a Z suffix (see FormatTime).
	PinnedAt string `json:"pinned_at"`
	// Platforms is keyed by PlatformKey.
	Platforms map[string]Platform `json:"platforms"`
}

// Platform is one platform's entry in a pin.
type Platform struct {
	// ArchiveSHA256 is the verified archive's hash. Empty for a file pin,
	// which has no archive.
	ArchiveSHA256 string `json:"archive_sha256"`
	// BinarySHA256 is the hash of the extracted berth, the runtime trust
	// anchor and the name of its file in the teardown directory. Present only
	// for platforms installed on this machine.
	BinarySHA256 string `json:"binary_sha256,omitempty"`
}

// ReleasePlatforms are the platforms a release pin records an archive digest
// for, from the one verified checksums.txt (docs/design/teardown.md).
var ReleasePlatforms = []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"}

// PlatformKey is the key of a platform in a pin: "<GOOS>_<GOARCH>".
func PlatformKey(goos, goarch string) string { return goos + "_" + goarch }

// FormatTime is how a pin writes a time: RFC 3339, UTC, with a Z suffix.
func FormatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ErrForeign is what errors.Is finds in the refusal ReadPin gives a pin this
// build did not write, as stateformat.ErrNewer is for a newer one.
var ErrForeign = errors.New("not a teardown pin this build wrote")

// foreignError is the refusal for a foreign pin, under a type that also
// answers errors.Is(ErrForeign), built like stateformat's newer error: a plain
// refusal with nothing extra to print. It keeps why the pin was rejected behind
// Cause rather than Unwrap, because the reporter prints an unwrapped cause as a
// "caused by" line and the refusal's text is the whole message.
type foreignError struct {
	message string
	cause   error
}

// Cause is why the pin was judged foreign (the parse or validation failure),
// or nil when it was judged by its version alone. It is for debugging and
// tests; it is not part of the printed refusal.
func (e *foreignError) Cause() error { return e.cause }

// Error is the refusal's message.
func (e *foreignError) Error() string { return e.message }

// ExitStatus is 1, like any expected failure.
func (e *foreignError) ExitStatus() int { return 1 }

// Is reports whether target is ErrForeign.
func (e *foreignError) Is(target error) bool { return target == ErrForeign }

// foreign is the refusal for a pin at path in the teardown directory dir: it
// says to delete the directory, never to upgrade, because only a build that
// wrote the pin can rewrite it.
func foreign(path, dir string, cause error) error {
	return &foreignError{message: fmt.Sprintf(
		"Refusing to use the teardown pin %s: it is not a pin this build wrote. Delete %s/ and pin again.", path, dir), cause: cause}
}

var (
	labelPattern    = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)
	sha256Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	platformPattern = regexp.MustCompile(`^(linux|darwin)_(amd64|arm64)$`)
)

// ValidLabel reports whether s is a label a file pin may carry as its release:
// 1 to 64 characters, letters, digits and . _ + -, starting with a letter or a
// digit.
func ValidLabel(s string) bool { return labelPattern.MatchString(s) }

// fileScheme is the prefix of a file pin's source.
const fileScheme = "file:"

// IsFile reports whether p is a file pin: made from a local binary and a
// digest the user supplied, with no signature checked.
func (p Pin) IsFile() bool { return strings.HasPrefix(p.Source, fileScheme) }

// BinaryHashes are the binary_sha256 values p names, sorted, without
// duplicates.
func (p Pin) BinaryHashes() []string {
	seen := map[string]bool{}
	var hashes []string
	for _, platform := range p.Platforms {
		if platform.BinarySHA256 != "" && !seen[platform.BinarySHA256] {
			seen[platform.BinarySHA256] = true
			hashes = append(hashes, platform.BinarySHA256)
		}
	}
	sort.Strings(hashes)
	return hashes
}

// Validate reports why p is not a pin this build writes, or nil. It is the
// rule for every field of "Where the pin lives" in docs/design/teardown.md,
// applied to what is read and to what is about to be written.
func (p Pin) Validate() error {
	if p.Version != stateformat.TeardownPin {
		return fmt.Errorf("version is %d, not %d", p.Version, stateformat.TeardownPin)
	}
	if err := p.validateIdentity(); err != nil {
		return err
	}
	if t, err := time.Parse(time.RFC3339, p.PinnedAt); err != nil || FormatTime(t) != p.PinnedAt {
		return fmt.Errorf("pinned_at %q is not RFC 3339 in UTC with a Z suffix", p.PinnedAt)
	}
	return p.validatePlatforms()
}

// validateIdentity checks release, source, signer and checksums_sha256, which
// differ for a release pin and a file pin.
func (p Pin) validateIdentity() error {
	if p.IsFile() {
		if !labelPattern.MatchString(p.Release) {
			return fmt.Errorf("release %q is not a label", p.Release)
		}
		path := strings.TrimPrefix(p.Source, fileScheme)
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || text.HasControl(path) {
			return fmt.Errorf("source %q is not file: and an absolute, cleaned path", p.Source)
		}
		if p.Signer != "" || p.ChecksumsSHA256 != "" {
			return errors.New("a file pin has no signer and no checksums_sha256")
		}
		return nil
	}
	if !semver.IsRelease(p.Release) {
		return fmt.Errorf("release %q is not a release version", p.Release)
	}
	if err := validateReleaseSource(p.Source, p.Release); err != nil {
		return err
	}
	if p.Signer == "" || text.HasControl(p.Signer) || strings.ContainsAny(p.Signer, " \t") {
		return fmt.Errorf("signer %q is not an identity", p.Signer)
	}
	if !sha256Pattern.MatchString(p.ChecksumsSHA256) {
		return errors.New("checksums_sha256 is not a lowercase sha256")
	}
	return nil
}

// validateReleaseSource accepts only an https base URL ending
// /releases/download/v<release>/, with no user, query or fragment.
func validateReleaseSource(source, release string) error {
	parsed, err := url.Parse(source)
	if err != nil || text.HasControl(source) || strings.ContainsAny(source, " \t") ||
		parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" ||
		!strings.HasSuffix(source, "/releases/download/v"+release+"/") {
		return fmt.Errorf("source %q is not an https .../releases/download/v%s/ URL", source, release)
	}
	return nil
}

// validatePlatforms checks the platforms map: known keys, digests of the right
// shape, every release platform present in a release pin, and at least one
// binary the pin names.
func (p Pin) validatePlatforms() error {
	if len(p.Platforms) == 0 {
		return errors.New("platforms is empty")
	}
	if p.IsFile() && len(p.Platforms) != 1 {
		return errors.New("a file pin has exactly one platform entry, the machine it was made on")
	}
	binaries := 0
	for key, platform := range p.Platforms {
		if !platformPattern.MatchString(key) {
			return fmt.Errorf("platform %q is not one berth builds", key)
		}
		if p.IsFile() {
			if platform.ArchiveSHA256 != "" {
				return fmt.Errorf("platform %s: a file pin has no archive_sha256", key)
			}
			if platform.BinarySHA256 == "" {
				return fmt.Errorf("platform %s: a file pin needs a binary_sha256", key)
			}
		} else if !sha256Pattern.MatchString(platform.ArchiveSHA256) {
			return fmt.Errorf("platform %s: archive_sha256 is not a lowercase sha256", key)
		}
		if platform.BinarySHA256 != "" {
			if !sha256Pattern.MatchString(platform.BinarySHA256) {
				return fmt.Errorf("platform %s: binary_sha256 is not a lowercase sha256", key)
			}
			binaries++
		}
	}
	if !p.IsFile() {
		for _, key := range ReleasePlatforms {
			if _, ok := p.Platforms[key]; !ok {
				return fmt.Errorf("a release pin records platform %s", key)
			}
		}
	}
	if binaries == 0 {
		return errors.New("no platform names a binary")
	}
	return nil
}

// topKeys are the keys a pin has, all required.
var topKeys = []string{"version", "release", "source", "signer", "checksums_sha256", "pinned_at", "platforms"}

// parse reads a pin whose version is already known to be the current one. Any
// shape this build does not write is an error: an unknown key (matched
// case-sensitively, which encoding/json alone does not), a missing key, a null,
// a wrong type, trailing data or an invalid field.
func parse(data []byte) (Pin, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return Pin{}, err
	}
	if err := noDuplicateKeys(data); err != nil {
		return Pin{}, err
	}
	if err := exactKeys(top, topKeys, nil); err != nil {
		return Pin{}, err
	}
	var platforms map[string]map[string]json.RawMessage
	if err := json.Unmarshal(top["platforms"], &platforms); err != nil {
		return Pin{}, fmt.Errorf("platforms: %w", err)
	}
	for key, entry := range platforms {
		if err := exactKeys(entry, []string{"archive_sha256"}, []string{"binary_sha256"}); err != nil {
			return Pin{}, fmt.Errorf("platform %s: %w", key, err)
		}
		// The writer omits a binary it has not installed; an empty one is not
		// something this build wrote.
		if raw, ok := entry["binary_sha256"]; ok && string(bytes.TrimSpace(raw)) == `""` {
			return Pin{}, fmt.Errorf("platform %s: binary_sha256 is empty", key)
		}
	}
	var pin Pin
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pin); err != nil {
		return Pin{}, err
	}
	if err := pin.Validate(); err != nil {
		return Pin{}, err
	}
	return pin, nil
}

// exactKeys requires every key in required, allows those in optional, and
// refuses any other key and any null value. Keys are compared exactly.
func exactKeys(object map[string]json.RawMessage, required, optional []string) error {
	allowed := map[string]bool{}
	for _, key := range required {
		allowed[key] = true
		if _, ok := object[key]; !ok {
			return fmt.Errorf("missing key %q", key)
		}
	}
	for _, key := range optional {
		allowed[key] = true
	}
	for key, value := range object {
		if !allowed[key] {
			return fmt.Errorf("unknown key %q", key)
		}
		if string(bytes.TrimSpace(value)) == "null" {
			return fmt.Errorf("key %q is null", key)
		}
	}
	return nil
}

// noDuplicateKeys refuses a document that repeats a key within one object, at
// any depth: encoding/json keeps the last of them, so the key set the exact-key
// check sees would not be what a reader of the file sees.
func noDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var scan func() error
	scan = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, _ := keyToken.(string)
				if seen[key] {
					return fmt.Errorf("duplicate key %q", key)
				}
				seen[key] = true
				if err := scan(); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		case json.Delim('['):
			for decoder.More() {
				if err := scan(); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		}
		return nil
	}
	return scan()
}
