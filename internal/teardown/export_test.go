package teardown

// SetAfterWritePinForTest installs hook to run in Save just after the
// pin.json rename, and returns a func that restores the previous hook.
func SetAfterWritePinForTest(hook func(binaryPath string)) (restore func()) {
	previous := afterWritePinForTest
	afterWritePinForTest = hook
	return func() { afterWritePinForTest = previous }
}

// SetBeforeRemoveForTest installs hook to run in Sweep just before the last
// check of a file it is about to remove, and returns a func that restores the
// previous hook.
func SetBeforeRemoveForTest(hook func(path string)) (restore func()) {
	previous := beforeRemoveForTest
	beforeRemoveForTest = hook
	return func() { beforeRemoveForTest = previous }
}
