package registry

import "testing"

// TestCouldContain pins which unreadable recorded directories keep a path
// lookup from finishing: the path itself or one of its parents, and nothing
// else.
func TestCouldContain(t *testing.T) {
	for _, c := range []struct {
		name  string
		root  string
		paths []string
		want  bool
	}{
		{"the root itself", "/r/app", []string{"/r/app"}, true},
		{"a path inside it", "/r/app", []string{"/r/app/src"}, true},
		{"another spelling's case", "/r/App", []string{"/r/app/src"}, true},
		{"the second spelling", "/r/app", []string{"/x", "/r/app"}, true},
		{"a root with a trailing separator", "/r/app/", []string{"/r/app/src"}, true},
		{"a sibling sharing the prefix", "/r/app", []string{"/r/app2"}, false},
		{"a parent of the root", "/r/app", []string{"/r"}, false},
		{"an unrelated directory", "/r/app", []string{"/other"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := couldContain(c.root, c.paths...); got != c.want {
				t.Fatalf("couldContain(%q, %q) = %v, want %v", c.root, c.paths, got, c.want)
			}
		})
	}
}
