package cli

import "testing"

// TestPasswdHomeIgnoresTheEnvironment pins what the gate relies on: the home
// from the user database does not follow $HOME or $USER, even where os/user's
// Current would fall back to them (no cgo, no passwd entry).
func TestPasswdHomeIgnoresTheEnvironment(t *testing.T) {
	before := passwdHome()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USER", "someone-else")
	if after := passwdHome(); after != before {
		t.Fatalf("passwdHome = %q after HOME and USER changed, was %q", after, before)
	}
}
