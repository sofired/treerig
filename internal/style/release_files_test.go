package style

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/brand"
	"github.com/sofired/treerig/internal/releasefetch"
)

// repoFile reads a file under the repository root, relative to this package.
func repoFile(t *testing.T, path ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, path...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// only returns the single capture group of the one match of re in text, and
// fails when there is not exactly one: a pattern that stops matching is a
// failure, never a skip.
func only(t *testing.T, file string, re *regexp.Regexp, text string) string {
	t.Helper()
	m := re.FindAllStringSubmatch(text, -1)
	if len(m) != 1 {
		t.Fatalf("%s: want exactly one match of %s, found %d", file, re, len(m))
	}
	return m[0][1]
}

// The release inputs that cannot import Go (install.sh, .goreleaser.yaml, the
// Homebrew formula and the snapshot verifier) spell the repository path, the
// product name and the archive name themselves. They must agree with
// internal/brand and releasefetch, or a rename would ship a release that the
// pin flow refuses.
func TestReleaseFilesAgreeWithBrand(t *testing.T) {
	const version, platform = "1.2.3", "linux_amd64"
	wantArchive := releasefetch.ArchiveName(version, platform)

	install := repoFile(t, "install.sh")
	goreleaser := repoFile(t, ".goreleaser.yaml")
	formula := repoFile(t, "packaging", "homebrew", "treerig.rb")
	verify := repoFile(t, "packaging", "verify-snapshot.sh")

	t.Run("install.sh names the repository and the archive", func(t *testing.T) {
		if got := only(t, "install.sh", regexp.MustCompile(`(?m)^REPO="([^"]*)"$`), install); got != brand.Repository {
			t.Errorf(`install.sh REPO = %q, want brand.Repository %q`, got, brand.Repository)
		}
		bin := only(t, "install.sh", regexp.MustCompile(`(?m)^BIN="([^"]*)"$`), install)
		if bin != brand.Name {
			t.Errorf(`install.sh BIN = %q, want brand.Name %q`, bin, brand.Name)
		}
		template := only(t, "install.sh", regexp.MustCompile(`(?m)^archive="([^"]*)"$`), install)
		got := strings.NewReplacer("${BIN}", bin, "${version}", version, "${platform}", platform).Replace(template)
		if got != wantArchive {
			t.Errorf("install.sh builds the archive name %q (from %q), want %q", got, template, wantArchive)
		}
	})

	t.Run(".goreleaser.yaml names the project and the archive", func(t *testing.T) {
		if got := only(t, ".goreleaser.yaml", regexp.MustCompile(`(?m)^project_name:\s*(\S+)\s*$`), goreleaser); got != brand.Name {
			t.Errorf(".goreleaser.yaml project_name = %q, want brand.Name %q", got, brand.Name)
		}
		template := only(t, ".goreleaser.yaml", regexp.MustCompile(`(?m)^\s*name_template:\s*"([^"]*\{\{ \.Os \}\}[^"]*)"\s*$`), goreleaser)
		got := strings.NewReplacer("{{ .ProjectName }}", brand.Name, "{{ .Version }}", version, "{{ .Os }}", "linux", "{{ .Arch }}", "amd64").Replace(template) + ".tar.gz"
		if got != wantArchive {
			t.Errorf(".goreleaser.yaml archive name_template %q gives %q, want %q", template, got, wantArchive)
		}
	})

	t.Run("the signing identity names a workflow that exists", func(t *testing.T) {
		const suffix = "/.github/workflows/release.yml@refs/tags/v" + version
		signer := releasefetch.KeylessPolicy{Release: version}.Signer()
		if want := "https://github.com/" + brand.Repository + suffix; signer != want {
			t.Errorf("the signing identity is %q, want %q", signer, want)
		}
		repoFile(t, ".github", "workflows", "release.yml")
	})

	t.Run("the version ldflags and repository URLs use brand.Repository", func(t *testing.T) {
		ldflags := regexp.MustCompile(`-X github\.com/([^ ]+?)/internal/brand\.Version=`)
		for name, text := range map[string]string{
			".goreleaser.yaml":              goreleaser,
			"packaging/homebrew/treerig.rb": formula,
			"packaging/verify-snapshot.sh":  verify,
		} {
			if got := only(t, name, ldflags, text); got != brand.Repository {
				t.Errorf("%s: -X ldflags module path is github.com/%s, want github.com/%s", name, got, brand.Repository)
			}
		}
		urls := regexp.MustCompile(`https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?(?:/|")`)
		found := urls.FindAllStringSubmatch(formula, -1)
		if len(found) == 0 {
			t.Fatal("packaging/homebrew/treerig.rb: found 0 repository URLs")
		}
		for _, m := range found {
			if m[1] != brand.Repository {
				t.Errorf("packaging/homebrew/treerig.rb: URL names %s, want %s", m[1], brand.Repository)
			}
		}
	})
}

// The short alias `trig` is a second name for the same binary: a symlink,
// never a copy, so brand.CLIName (which follows argv[0]) is the only thing
// that makes it say `trig`. Each place that creates the symlink spells the name
// itself, and a rename or a typo in one would ship an alias that points
// nowhere or is missing from one install route.
func TestAliasAgreesAcrossReleaseFiles(t *testing.T) {
	const alias = "trig"

	install := repoFile(t, "install.sh")
	goreleaser := repoFile(t, ".goreleaser.yaml")
	formula := repoFile(t, "packaging", "homebrew", "treerig.rb")
	completions := repoFile(t, "packaging", "completions.sh")
	verify := repoFile(t, "packaging", "verify-snapshot.sh")

	if got := only(t, "install.sh", regexp.MustCompile(`(?m)^ALIAS="([^"]*)"$`), install); got != alias {
		t.Errorf("install.sh ALIAS = %q, want %q", got, alias)
	}
	if !strings.Contains(install, `ln -s "$BIN" "$alias_path"`) {
		t.Error(`install.sh does not create the alias as a relative symlink to $BIN`)
	}

	// nfpm: a symlink whose target is the sibling binary.
	link := regexp.MustCompile(`(?m)^\s*- src: ` + brand.Name + `\n\s*dst: /usr/bin/` + alias + `\n\s*type: symlink$`)
	if !link.MatchString(goreleaser) {
		t.Errorf(".goreleaser.yaml does not ship /usr/bin/%s as a symlink to %s", alias, brand.Name)
	}

	if want := `bin.install_symlink "` + brand.Name + `" => "` + alias + `"`; !strings.Contains(formula, want) {
		t.Errorf("packaging/homebrew/treerig.rb does not contain %s", want)
	}

	// The completion scripts the packages ship are generated per name, and
	// every one the packages list must be one completions.sh writes.
	for _, f := range []string{alias + ".bash", "_" + alias, alias + ".fish"} {
		for name, text := range map[string]string{
			"packaging/completions.sh":     completions,
			".goreleaser.yaml":             goreleaser,
			"packaging/verify-snapshot.sh": verify,
		} {
			if !strings.Contains(text, f) {
				t.Errorf("%s does not mention the completion file %s", name, f)
			}
		}
	}
}
