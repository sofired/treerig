package files

// SetBeforeOpenControlHomeForTest installs hook to run just before
// establishControlHome opens the control home directory itself, for a test
// that needs to swap something in during that exact window. Returns a func
// that restores the previous hook; callers defer it so an override never
// leaks into another test. It lives in a _test.go file so that only test
// binaries can reach it.
func SetBeforeOpenControlHomeForTest(hook func(controlHome string)) (restore func()) {
	previous := beforeOpenControlHomeForTest
	beforeOpenControlHomeForTest = hook
	return func() { beforeOpenControlHomeForTest = previous }
}

// SetAfterMarkerReadForTest installs hook to run just after
// establishControlHome reads populated and the marker's own contents, and
// just before it acts on what it read — the window in which a goroutine or
// process that goes on to lose a race to establish the control home is
// sitting on a stale view of it. Returns a func that restores the previous
// hook; callers defer it so an override never leaks into another test. It
// lives in a _test.go file so that only test binaries can reach it.
func SetAfterMarkerReadForTest(hook func(controlHome string)) (restore func()) {
	previous := afterMarkerReadForTest
	afterMarkerReadForTest = hook
	return func() { afterMarkerReadForTest = previous }
}
