package semver_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sofired/treerig/internal/semver"
)

// mustParse parses s or fails the test.
func mustParse(t *testing.T, s string) semver.Version {
	t.Helper()
	v, err := semver.Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return v
}

func TestParse(t *testing.T) {
	t.Run("accepts strict SemVer 2.0 versions", func(t *testing.T) {
		for _, s := range []string{
			"0.0.0", "1.2.3", "10.20.30", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-0.3.7", "1.0.0-x.7.z.92",
			"1.0.0-x-y-z.--", "1.0.0+20130313144700", "1.0.0-beta+exp.sha.5114f85", "1.0.0+21AF26D3----117B344092BD",
			"0.1.0-rc.1", "0.0.0-dev+0123456789ab", "1.0.0+001", "99999999999999999999999.0.0",
		} {
			if _, err := semver.Parse(s); err != nil {
				t.Errorf("Parse(%q) = %v, want accepted", s, err)
			}
		}
	})

	t.Run("rejects 01.2.3, 1.2.3-01, 1.2.3-a..b and 1.2.3-+", func(t *testing.T) {
		for _, s := range []string{"01.2.3", "1.2.3-01", "1.2.3-a..b", "1.2.3-+"} {
			if _, err := semver.Parse(s); err == nil {
				t.Errorf("Parse(%q) accepted", s)
			}
		}
	})

	t.Run("rejects everything else that is not a version", func(t *testing.T) {
		for _, s := range []string{
			"", "1", "1.2", "1.2.3.4", "v1.2.3", " 1.2.3", "1.2.3 ", "1.2.x", "1..3", "1.2.-3", "+1.2.3", "-1.2.3",
			"1.02.3", "1.2.03", "1.2.3-", "1.2.3+", "1.2.3-a.", "1.2.3-.a", "1.2.3+a..b", "1.2.3-a_b", "1.2.3-é",
			"1.2.3+é", "1.2.3-a+", "1.2.3+a+b", "١.٢.٣", "1.2.3-00", "1.2.3-1.01",
		} {
			if _, err := semver.Parse(s); err == nil {
				t.Errorf("Parse(%q) accepted", s)
			}
		}
	})

	t.Run("a hyphen inside the core or the build is not a pre-release", func(t *testing.T) {
		if _, err := semver.Parse("1.2.3+a-b"); err != nil {
			t.Errorf("a hyphen in build metadata: %v", err)
		}
		if _, err := semver.Parse("1.2.3-a-b"); err != nil {
			t.Errorf("a hyphen in a pre-release identifier: %v", err)
		}
	})
}

func TestCompare(t *testing.T) {
	t.Run("orders section 11's own example, each below the next", func(t *testing.T) {
		ordered := []string{
			"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0",
		}
		for i := range ordered {
			for j := range ordered {
				got := semver.Compare(mustParse(t, ordered[i]), mustParse(t, ordered[j]))
				want := 0
				if i < j {
					want = -1
				} else if i > j {
					want = 1
				}
				if got != want {
					t.Errorf("Compare(%s, %s) = %d, want %d", ordered[i], ordered[j], got, want)
				}
			}
		}
	})

	t.Run("orders major, minor and patch numerically, not as text", func(t *testing.T) {
		ordered := []string{"0.9.9", "1.0.0", "1.0.1", "1.0.10", "1.2.0", "1.10.0", "2.0.0", "10.0.0"}
		for i := 1; i < len(ordered); i++ {
			if semver.Compare(mustParse(t, ordered[i-1]), mustParse(t, ordered[i])) != -1 {
				t.Errorf("%s is not below %s", ordered[i-1], ordered[i])
			}
		}
	})

	t.Run("compares numbers too big for any integer width", func(t *testing.T) {
		big := "18446744073709551616" // 2^64
		bigger := "18446744073709551617"
		for _, tc := range []struct {
			a, b string
			want int
		}{
			{big + ".0.0", bigger + ".0.0", -1},
			{"1." + bigger + ".0", "1." + big + ".0", 1},
			{"1.0." + big, "1.0." + big, 0},
			{"1.0.0-" + bigger, "1.0.0-" + big, 1},
			{"1.0.0-" + big + "0", "1.0.0-" + bigger, 1},
			{"99999999999999999999.0.0", "100000000000000000000.0.0", -1},
		} {
			if got := semver.Compare(mustParse(t, tc.a), mustParse(t, tc.b)); got != tc.want {
				t.Errorf("Compare(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		}
	})

	t.Run("ignores build metadata", func(t *testing.T) {
		if got := semver.Compare(mustParse(t, "1.0.0+a"), mustParse(t, "1.0.0+b")); got != 0 {
			t.Errorf("builds differ: %d", got)
		}
		if got := semver.Compare(mustParse(t, "1.0.0-rc.1+zzz"), mustParse(t, "1.0.0-rc.1")); got != 0 {
			t.Errorf("build on a pre-release: %d", got)
		}
	})

	t.Run("a numeric identifier ranks below an alphanumeric one, whatever their text", func(t *testing.T) {
		if semver.Compare(mustParse(t, "1.0.0-999"), mustParse(t, "1.0.0-a")) != -1 {
			t.Error("999 is not below a")
		}
		if semver.Compare(mustParse(t, "1.0.0-1a"), mustParse(t, "1.0.0-2")) != 1 {
			t.Error("1a is not above 2")
		}
	})

	t.Run("alphanumeric identifiers compare as ASCII, so uppercase ranks below lowercase", func(t *testing.T) {
		if semver.Compare(mustParse(t, "1.0.0-Z"), mustParse(t, "1.0.0-a")) != -1 {
			t.Error("Z is not below a")
		}
	})

	t.Run("a shorter pre-release list ranks below a longer one when the shared identifiers are equal", func(t *testing.T) {
		if semver.Compare(mustParse(t, "1.0.0-a.b"), mustParse(t, "1.0.0-a.b.c")) != -1 {
			t.Error("a.b is not below a.b.c")
		}
		if semver.Compare(mustParse(t, "1.0.0-a.c"), mustParse(t, "1.0.0-a.b.c")) != 1 {
			t.Error("a.c is not above a.b.c: an earlier difference decides before length")
		}
	})
}

func TestNewer(t *testing.T) {
	t.Run("is true only for a higher version", func(t *testing.T) {
		for _, tc := range []struct {
			candidate, than string
			want            bool
		}{
			{"1.3.0", "1.2.9", true},
			{"1.2.9", "1.3.0", false},
			{"1.3.0", "1.3.0", false},
			{"1.3.0", "1.3.0-rc.1", true},
			{"1.3.0-rc.1", "1.3.0", false},
			{"1.3.0+b", "1.3.0+a", false},
		} {
			if got := semver.Newer(tc.candidate, tc.than); got != tc.want {
				t.Errorf("Newer(%q, %q) = %v, want %v", tc.candidate, tc.than, got, tc.want)
			}
		}
	})

	t.Run("a development version is never newer than anything", func(t *testing.T) {
		for _, dev := range []string{"0.0.0-dev", "0.0.0-dev+0123456789ab", "0.0.0-dev.5"} {
			for _, than := range []string{"0.0.0", "0.0.0-a", "0.0.0-dev", "1.0.0", "garbage"} {
				if semver.Newer(dev, than) {
					t.Errorf("Newer(%q, %q) = true", dev, than)
				}
			}
		}
		// A real release is newer than a development pin.
		if !semver.Newer("0.1.0", "0.0.0-dev") {
			t.Error("0.1.0 is not newer than a development version")
		}
	})

	t.Run("an unparsable version is never newer, and nothing is newer than one", func(t *testing.T) {
		for _, bad := range []string{"", "v1.2.3", "1.2", "01.2.3", "my-build", strings.Repeat("9", 5)} {
			if semver.Newer(bad, "0.0.1") {
				t.Errorf("Newer(%q, 0.0.1) = true", bad)
			}
			if semver.Newer("9.9.9", bad) {
				t.Errorf("Newer(9.9.9, %q) = true", bad)
			}
		}
	})
}

func TestAtLeast(t *testing.T) {
	t.Run("meets a minimum at or above it", func(t *testing.T) {
		for found, want := range map[string]bool{
			"3.0.6": true, "3.0.0": true, "3.0.0+meta": true, "4.0.0": true, "10.0.0": true, "2.9.9": false,
			"3.0.0-rc.1": false, "3.0.0-0": false, "": false, "devel": false, "v3.0.6": false, "3.0": false,
			"0.0.0-dev": false,
		} {
			if got := semver.AtLeast(found, "3.0.0"); got != want {
				t.Errorf("AtLeast(%q, 3.0.0) = %v, want %v", found, got, want)
			}
		}
	})

	t.Run("an unparsable minimum is never met", func(t *testing.T) {
		if semver.AtLeast("3.0.0", "three") {
			t.Error("met a minimum that is not a version")
		}
	})
}

func TestIsPreRelease(t *testing.T) {
	t.Run("is true for a pre-release part and false for a release or build metadata alone", func(t *testing.T) {
		for s, want := range map[string]bool{
			"0.1.0-rc.3": true, "1.2.3-beta": true, "0.1.0-rc.2-5": true, "0.0.0-dev+abc": true, "0.1.0-rc.1+build": true,
			"0.1.0": false, "1.2.3": false, "1.2.3+build": false,
		} {
			if got := mustParse(t, s).IsPreRelease(); got != want {
				t.Errorf("IsPreRelease(%q) = %v, want %v", s, got, want)
			}
		}
	})
}

func TestIsDev(t *testing.T) {
	t.Run("is 0.0.0 with a dev pre-release", func(t *testing.T) {
		for s, want := range map[string]bool{
			"0.0.0-dev": true, "0.0.0-dev+abc": true, "0.0.0-dev.1": true, "0.0.0-devel": true, "0.0.0-dev2": true, "0.0.0": false, "0.0.0-rc.1": false,
			"0.0.1-dev": false, "0.1.0-dev": false, "1.0.0-dev": false,
		} {
			if got := mustParse(t, s).IsDev(); got != want {
				t.Errorf("IsDev(%q) = %v, want %v", s, got, want)
			}
		}
	})
}

// publishedReleaseCases is every version IsPublishedRelease is pinned on, and
// whether it counts as a release; the tag-validation script is held to the same
// table.
var publishedReleaseCases = map[string]bool{
	"0.1.0":                        true,
	"1.2.3":                        true,
	"0.1.0-rc.1":                   true,
	"0.1.0-rc.2":                   true,
	"10.20.30-beta.4":              true,
	"0.0.1":                        true,
	"0.1.0-rc.2-5":                 true, // a hyphenated pre-release identifier is legal, and no git hash follows
	"0.0.0-dev":                    false,
	"0.0.0-dev+abc123def456":       false,
	"0.0.0":                        false,
	"0.0.0-rc.1":                   false,
	"0.1.0+abc":                    false,
	"0.1.0-rc.2+abc":               false,
	"0.1.0-rc.2-5-gabc1234":        false, // git describe
	"1.0.0-5-gabcdef0123":          false,
	"0.1.0-rc.2-5-gabc1234-dirty":  false, // git describe --dirty
	"0.1.0-rc.2-5-gabc1234-broken": false, // git describe --broken
	"0.1.0-dirty":                  false, // a tag checked out with local changes
	"0.1.0-broken":                 false,
	"0.1.0-rc.2-dirty":             false,
	"v1.2.3":                       false,
	"1.2":                          false,
	"01.2.3":                       false,
	"invoked-9.9.9":                false,
	"pinned-test":                  false,
	"":                             false,
	"garbage":                      false,
	"1.2.3 ":                       false,
}

func TestIsPublishedRelease(t *testing.T) {
	for in, want := range publishedReleaseCases {
		if got := semver.IsPublishedRelease(in); got != want {
			t.Errorf("IsPublishedRelease(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestValidateTagAgreesWithIsPublishedRelease runs .github/scripts/validate-tag.sh,
// the release workflow's tag check, for every version the Go predicate is pinned
// on, plus the ones a looser pattern let through, and requires that the script
// accepts a tag exactly when IsPublishedRelease accepts its version.
func TestValidateTagAgreesWithIsPublishedRelease(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", ".github", "scripts", "validate-tag.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{}
	for in, want := range publishedReleaseCases {
		cases[in] = want
	}
	for _, in := range []string{"0.0.00", "00.1.0", "1.2.3-01", "1.2.3-rc..1", "1.2.3-", "1.2.3-.rc", "1.2.3-rc.01"} {
		cases[in] = false
	}
	for in, want := range cases {
		if got := semver.IsPublishedRelease(in); got != want {
			t.Errorf("IsPublishedRelease(%q) = %v, want %v", in, got, want)
		}
		cmd := exec.Command(bash, script)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GITHUB_REF_NAME=v" + in}
		accepted := cmd.Run() == nil
		if accepted != want {
			t.Errorf("validate-tag.sh accepts tag v%s = %v, but IsPublishedRelease(%q) = %v", in, accepted, in, want)
		}
	}
}
