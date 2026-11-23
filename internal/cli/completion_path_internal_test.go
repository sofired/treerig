package cli

import "testing"

// TestIsPathPrefix pins which words completion leaves to the shell's own
// directory completion: exactly those registry.IsPathName reads as a path.
func TestIsPathPrefix(t *testing.T) {
	for _, c := range []struct {
		word string
		want bool
	}{
		{".", true},
		{"..", true},
		{"./", true},
		{"./x", true},
		{"../", true},
		{"../x", true},
		{"/", true},
		{"/tmp/x", true},
		{"~", true},
		{"~/x", true},
		{".github", false},
		{".h", false},
		{"..x", false},
		{"...", false},
		{"", false},
		{"feature", false},
		{"app.acme.localhost", false},
	} {
		t.Run(c.word, func(t *testing.T) {
			if got := isPathPrefix(c.word); got != c.want {
				t.Fatalf("isPathPrefix(%q) = %v, want %v", c.word, got, c.want)
			}
		})
	}
}
