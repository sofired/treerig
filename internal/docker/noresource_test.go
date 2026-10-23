package docker

import "testing"

func TestIsNoResourceWarning(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want bool
	}{
		{"matches Compose 5's logfmt warning", `time="2026-10-23T21:13:32Z" level=warning msg="Warning: No resource found to remove for project \"treerig-zz\"."`, true},
		{"matches the plain sentence of earlier versions", `No resource found to remove for project "treerig-zz".`, true},
		{"ignores surrounding space", "  Warning: No resource found to remove for project \"p\".  ", true},
		{"does not match another warning", `time="2026-10-23T21:13:32Z" level=warning msg="Warning: something else"`, false},
		{"does not match a longer line that contains it", `error: No resource found to remove for project "p". And more`, false},
		{"does not match a different project phrase", `No resource found to remove for volume "v".`, false},
		{"does not match an empty line", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNoResourceWarning(tc.line); got != tc.want {
				t.Fatalf("IsNoResourceWarning(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

func TestWithoutNoResourceWarning(t *testing.T) {
	in := "first\n" + `time="t" level=warning msg="Warning: No resource found to remove for project \"p\"."` + "\nError response from daemon: boom\n"
	if got, want := WithoutNoResourceWarning(in), "first\nError response from daemon: boom\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
