package binstore

// SetBeforeChtimesForTest installs hook to run just after Install confirms
// an entry already exists and just before it calls Chtimes on it — the
// window a concurrent Prune, in another process, can win by removing that
// entry first (see Install's own doc comment). Returns a func that
// restores the previous hook; callers defer it so an override never leaks
// into another test. It lives in a _test.go file so that only test
// binaries can reach it — the same pattern internal/files uses for its own
// SetBeforeOpenControlHomeForTest.
func SetBeforeChtimesForTest(hook func(target string)) (restore func()) {
	previous := beforeChtimesForTest
	beforeChtimesForTest = hook
	return func() { beforeChtimesForTest = previous }
}
