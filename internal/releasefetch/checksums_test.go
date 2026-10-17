package releasefetch_test

import (
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/releasefetch"
)

// checksumsFor is a checksums.txt listing the four archives of release with
// a digest per platform, and the extra lines a real one has.
func checksumsFor(release string, digest func(platform string) string, extra ...string) string {
	var lines []string
	for _, platform := range releasefetch.Platforms {
		lines = append(lines, digest(platform)+"  "+releasefetch.ArchiveName(release, platform))
	}
	lines = append(lines, extra...)
	return strings.Join(lines, "\n") + "\n"
}

func TestParseChecksums(t *testing.T) {
	digest := func(platform string) string { return strings.Repeat(string("0123456789abcdef"[len(platform)%16]), 64) }

	t.Run("returns the digest of each platform's archive and ignores the other assets", func(t *testing.T) {
		data := checksumsFor("1.2.3", digest,
			strings.Repeat("e", 64)+"  treerig_1.2.3_amd64.deb",
			strings.Repeat("f", 64)+"  treerig-1.2.3.tar.gz",
		)
		got, err := releasefetch.ParseChecksums([]byte(data), "1.2.3")
		if err != nil || len(got) != 4 {
			t.Fatalf("ParseChecksums = %v, %v", got, err)
		}
		for _, platform := range releasefetch.Platforms {
			if got[platform] != digest(platform) {
				t.Fatalf("%s = %q", platform, got[platform])
			}
		}
	})

	t.Run("refuses a file that lacks one of the four archives", func(t *testing.T) {
		for _, platform := range releasefetch.Platforms {
			var lines []string
			for _, p := range releasefetch.Platforms {
				if p != platform {
					lines = append(lines, digest(p)+"  "+releasefetch.ArchiveName("1.2.3", p))
				}
			}
			_, err := releasefetch.ParseChecksums([]byte(strings.Join(lines, "\n")+"\n"), "1.2.3")
			if err == nil || !strings.Contains(err.Error(), "does not list "+releasefetch.ArchiveName("1.2.3", platform)) {
				t.Fatalf("without %s: err = %v", platform, err)
			}
		}
	})

	t.Run("refuses an archive named twice, even with the same digest", func(t *testing.T) {
		name := releasefetch.ArchiveName("1.2.3", "linux_amd64")
		data := checksumsFor("1.2.3", digest, digest("linux_amd64")+"  "+name)
		if _, err := releasefetch.ParseChecksums([]byte(data), "1.2.3"); err == nil || !strings.Contains(err.Error(), "more than once") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("lists the archives of another release as missing", func(t *testing.T) {
		data := checksumsFor("1.2.4", digest)
		if _, err := releasefetch.ParseChecksums([]byte(data), "1.2.3"); err == nil {
			t.Fatal("the archives of 1.2.4 satisfied 1.2.3")
		}
	})

	t.Run("refuses a line that is not a lowercase sha256, two spaces and a name", func(t *testing.T) {
		for name, line := range map[string]string{
			"uppercase digest": strings.Repeat("A", 64) + "  x",
			"short digest":     strings.Repeat("a", 63) + "  x",
			"one space":        strings.Repeat("a", 64) + " x",
			"binary marker":    strings.Repeat("a", 64) + " *x",
			"blank line":       "",
			"carriage return":  strings.Repeat("a", 64) + "  x\r",
			"no name":          strings.Repeat("a", 64) + "  ",
		} {
			t.Run(name, func(t *testing.T) {
				data := checksumsFor("1.2.3", digest, line)
				if _, err := releasefetch.ParseChecksums([]byte(data), "1.2.3"); err == nil || !strings.Contains(err.Error(), "line 5 is not") {
					t.Fatalf("err = %v", err)
				}
			})
		}
	})

	t.Run("refuses an empty file", func(t *testing.T) {
		for _, data := range []string{"", "\n", "  \n"} {
			if _, err := releasefetch.ParseChecksums([]byte(data), "1.2.3"); err == nil {
				t.Fatalf("%q was accepted", data)
			}
		}
	})
}

// TestArchiveNameLiteral pins the archive file name as a literal, independent
// of brand.Name: install.sh and .goreleaser.yaml spell it the same way.
func TestArchiveNameLiteral(t *testing.T) {
	if got := releasefetch.ArchiveName("1.2.3", "linux_amd64"); got != "treerig_1.2.3_linux_amd64.tar.gz" {
		t.Errorf("ArchiveName = %q", got)
	}
}
