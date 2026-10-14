package registry

import (
	"strings"
	"testing"
)

// TestRefuseUnreadableAdvice covers what the refusal to allocate tells the
// user to do about the files it lists: a record a newer berth wrote is intact,
// so it is never advised to be moved aside.
func TestRefuseUnreadableAdvice(t *testing.T) {
	newer := RegistryProblem{File: "a.json", Reason: "Refusing to use the registration a.json: it is format 2", Newer: true}
	damaged := RegistryProblem{File: "b.json", Reason: "Registry record is not valid JSON: b.json"}
	const consequence = "a record that cannot be read may hold this port."

	t.Run("tells the user to upgrade, not to move a file aside, when every file is a newer berth's", func(t *testing.T) {
		got := refuseUnreadable("a port", consequence, []RegistryProblem{newer}).Error()
		if strings.Contains(got, "Move a file aside") || !strings.Contains(got, "Upgrade berth to read them: "+consequence) {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("still tells the user to move a damaged file aside", func(t *testing.T) {
		got := refuseUnreadable("a port", consequence, []RegistryProblem{damaged}).Error()
		if !strings.Contains(got, "Move a file aside once you know what it is: "+consequence) || strings.Contains(got, "Upgrade") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("separates the two when the files are a mixture", func(t *testing.T) {
		got := refuseUnreadable("a port", consequence, []RegistryProblem{newer, damaged}).Error()
		if !strings.Contains(got, "Upgrade berth to read the files that say so, and move any other file aside once you know what it is: "+consequence) {
			t.Fatalf("got %q", got)
		}
	})
}
