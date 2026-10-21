package commands

import "testing"

func TestShellWord(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                      "plain",
		"/home/me/treerig":           "/home/me/treerig",
		"0.1.0-rc.2+build_1,a:b@c%d": "0.1.0-rc.2+build_1,a:b@c%d",
		"":                           "''",
		"/home/me/sp dir/my treerig": "'/home/me/sp dir/my treerig'",
		"it's":                       `'it'\''s'`,
		"a$b":                        "'a$b'",
		"a;b":                        "'a;b'",
		"a\nb":                       "'a b'", // text.Printable turns a line break into a space before it is quoted
	} {
		if got := shellWord(in); got != want {
			t.Errorf("shellWord(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestPinCommandFor(t *testing.T) {
	const latest = "`treerig teardown pin --latest`"
	const devLatest = "`treerig teardown pin --latest` (or `--file`; see `treerig teardown pin --help`)"
	t.Run("a pre-release build names its own version, since --latest never selects a pre-release", func(t *testing.T) {
		for version, want := range map[string]string{
			"0.1.0-rc.3":      "`treerig teardown pin --release 0.1.0-rc.3`",
			"10.20.30-beta.4": "`treerig teardown pin --release 10.20.30-beta.4`",
		} {
			if got := pinCommandFor("treerig", version); got != want {
				t.Errorf("pinCommandFor(%q) = %s, want %s", version, got, want)
			}
		}
	})
	t.Run("a stable release build is told --latest, with no --file route", func(t *testing.T) {
		for _, version := range []string{"0.1.0", "1.2.3"} {
			if got := pinCommandFor("treerig", version); got != latest {
				t.Errorf("pinCommandFor(%q) = %s, want %s", version, got, latest)
			}
		}
	})
	t.Run("a development or source build is told --latest and the --file route", func(t *testing.T) {
		for _, version := range []string{"0.0.0-dev", "0.0.0-dev+abc123def456", "0.1.0-rc.3-5-gabc1234", "0.1.0-dirty", "garbage", ""} {
			if got := pinCommandFor("treerig", version); got != devLatest {
				t.Errorf("pinCommandFor(%q) = %s, want %s", version, got, devLatest)
			}
		}
	})
}
