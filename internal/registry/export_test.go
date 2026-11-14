package registry

import (
	"io/fs"
	"time"
)

// FailMarkerRemoval makes the store's deletion of a removal marker fail with
// err, for the test of Remove's partial outcome.
func (s *Store) FailMarkerRemoval(err error) {
	s.removeFile = func(string) error { return err }
}

// StatWith makes Entries ask stat whether a listed root is there, and give up
// on it after limit, for the test of a directory that never answers.
func (s *IdentityService) StatWith(stat func(path string) (fs.FileInfo, error), limit time.Duration) {
	s.stat, s.statLimit = stat, limit
}
