package docker

import (
	"regexp"
	"strings"
)

// noResourceWarning is the one line Compose's `down` prints when the project
// has nothing left to remove: a logfmt warning from Compose 2.2x and later, a
// plain sentence from earlier ones. Matched whole, so that no other line is
// ever taken for it.
var noResourceWarning = regexp.MustCompile(`^(?:time="[^"]*" level=warning msg=")?(?:Warning: )?No resource found to remove for project (?:\\"[^"\\]*\\"|"[^"]*")\."?$`)

// IsNoResourceWarning reports whether line is Compose's warning that `down`
// found nothing to remove. A project with nothing left is not a problem, and
// treerig says so in its own words.
func IsNoResourceWarning(line string) bool {
	return noResourceWarning.MatchString(strings.TrimSpace(line))
}

// WithoutNoResourceWarning is text without the lines IsNoResourceWarning
// matches, so a failure quotes what Compose said that mattered.
func WithoutNoResourceWarning(text string) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if !IsNoResourceWarning(line) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}
