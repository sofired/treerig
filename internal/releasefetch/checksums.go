package releasefetch

import (
	"fmt"
	"regexp"
	"strings"
)

// Platforms are the four platforms a release builds, as "<GOOS>_<GOARCH>"
// keys, in the order a pin records them.
var Platforms = []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"}

// ArchiveName is the platform archive of release, as docs/design/release.md
// names it: berth_X.Y.Z_<os>_<arch>.tar.gz.
func ArchiveName(release, platform string) string {
	return "berth_" + release + "_" + platform + ".tar.gz"
}

// checksumLine is one line of checksums.txt: a lowercase sha256, two spaces
// and a file name.
var checksumLine = regexp.MustCompile(`^([0-9a-f]{64})  ([^\x00-\x1f\x7f]+)$`)

// ParseChecksums reads checksums.txt, which the caller has had cosign verify,
// and returns the archive digest of each of the four platforms of release. The
// file must hold exactly one line for each of the four archive names; a line
// that is not `<sha256>  <name>` refuses the whole file, since a signed file
// that does not read as expected is not one to guess at.
func ParseChecksums(data []byte, release string) (map[string]string, error) {
	want := map[string]string{} // archive name -> platform
	for _, platform := range Platforms {
		want[ArchiveName(release, platform)] = platform
	}
	digests := map[string]string{}
	text := string(data)
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("checksums.txt is empty")
	}
	for number, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		match := checksumLine.FindStringSubmatch(line)
		if match == nil {
			return nil, fmt.Errorf("checksums.txt line %d is not `<sha256>  <file name>`", number+1)
		}
		platform, expected := want[match[2]]
		if !expected {
			continue
		}
		if _, seen := digests[platform]; seen {
			return nil, fmt.Errorf("checksums.txt lists %s more than once", match[2])
		}
		digests[platform] = match[1]
	}
	for _, platform := range Platforms {
		if _, ok := digests[platform]; !ok {
			return nil, fmt.Errorf("checksums.txt does not list %s", ArchiveName(release, platform))
		}
	}
	return digests, nil
}
