package cli

import (
	"os"
	"os/user"
	"strconv"
)

// passwdHome is the home directory the user database lists for the effective
// user, or "" when it has none to give. The teardown gate uses it as the one
// boundary its git work tree check does not look past
// (docs/design/teardown.md, "How `worktree remove` uses it"), so it must never
// come from the environment, which a checkout's environment file can set.
//
// It asks os/user.LookupId, not Current, deliberately. Without cgo, on Linux
// (what a release build uses, CGO_ENABLED=0), Current falls back to $HOME and
// $USER when /etc/passwd has no entry for the user: it can return the
// environment's answer. LookupId reads /etc/passwd (non-cgo Linux, which never
// consults NSS, LDAP or sssd) or, on macOS, getpwuid_r through libc without cgo
// (so Directory Services users resolve), and returns an error instead of
// guessing. An error here means "no independent home", and the gate then
// exempts nothing; a user the database does not list simply has no exemption
// for a dotfiles repository at their home directory.
func passwdHome() string {
	found, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil {
		return ""
	}
	return found.HomeDir
}
