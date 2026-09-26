package commands

// White-box (package commands, not commands_test) so this file can swap
// resolveUserHome: checkedHome's own empty-userHome guard (review round 4,
// item 2) has no portable way to reach through a real environment — the
// real layout.HomeDir falls back to the password database, not anything a
// Getenv can override, when HOME is unset, and that database has a home
// directory on every machine this suite runs on.

import (
	"testing"

	"github.com/sofired/berth/internal/layout"
)

// fixedGetenv is a layout.Getenv backed by a fixed map.
func fixedGetenv(values map[string]string) layout.Getenv {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

func TestCheckedHomeRefusesAnEmptyUserHome(t *testing.T) {
	previous := resolveUserHome
	resolveUserHome = func(layout.Getenv) (string, error) { return "", nil }
	t.Cleanup(func() { resolveUserHome = previous })

	_, _, err := checkedHome(fixedGetenv(map[string]string{}))
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if err.Error() != "Could not determine your home directory; set HOME." {
		t.Fatalf("got %q", err.Error())
	}
}
