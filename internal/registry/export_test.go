package registry

// FailMarkerRemoval makes the store's deletion of a removal marker fail with
// err, for the test of Remove's partial outcome.
func (s *Store) FailMarkerRemoval(err error) {
	s.removeFile = func(string) error { return err }
}
