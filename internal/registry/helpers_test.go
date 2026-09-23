package registry_test

import (
	"regexp"
	"strings"
)

// uuidPattern mirrors the production package's own (unexported) UUID
// pattern, for assertions in this external test package.
var uuidPattern = regexp.MustCompile(`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}|00000000-0000-0000-0000-000000000000|ffffffff-ffff-ffff-ffff-ffffffffffff)$`)

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
