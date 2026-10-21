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
