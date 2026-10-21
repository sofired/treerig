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

// SetEuidForTest makes CheckOwnership believe the effective user is uid, and
// returns a func that restores the real one.
func SetEuidForTest(uid int) (restore func()) {
	previous := euid
	euid = func() int { return uid }
	return func() { euid = previous }
}
